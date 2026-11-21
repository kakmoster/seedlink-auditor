package qbit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"seedlink-auditor/internal/config"
)

var (
	seedingStates = map[string]struct{}{
		"uploading":  {},
		"stalledUP":  {},
		"queuedUP":   {},
		"checkingUP": {},
		"forcedUP":   {},
	}
	pausedUpStates = map[string]struct{}{
		"pausedUP":  {},
		"stoppedUP": {},
	}
	downloadingStates = map[string]struct{}{
		"downloading": {},
		"stalledDL":   {},
		"metaDL":      {},
		"queuedDL":    {},
		"pausedDL":    {},
		"stoppedDL":   {},
		"checkingDL":  {},
		"forcedDL":    {},
	}
)

type TorrentFileEntry struct {
	InstanceID    string  `json:"instance_id"`
	InstanceName  string  `json:"instance_name"`
	TorrentHash   string  `json:"torrent_hash"`
	TorrentName   string  `json:"torrent_name"`
	State         string  `json:"state"`
	IsSeeding     bool    `json:"is_seeding"`
	SeedingStatus string  `json:"seeding_status"`
	Ratio         float64 `json:"ratio"`
	Tracker       string  `json:"tracker"`
	TrackerHost   string  `json:"tracker_host"`
	TrackerHealth string  `json:"tracker_health"`
	TrackerMsg    string  `json:"tracker_msg"`
	SavePath      string  `json:"save_path"`
	RelativeName     string  `json:"relative_name"`
	MappedPath       string  `json:"mapped_path"`
	Filename         string  `json:"-"`
	Size             int64   `json:"size"`
	TorrentFileCount int     `json:"torrent_file_count"`
}

type InstanceReport struct {
	InstanceID   string  `json:"instance_id"`
	InstanceName string  `json:"instance_name"`
	URL          string  `json:"url"`
	OK           bool    `json:"ok"`
	TorrentCount int     `json:"torrent_count"`
	FileCount    int     `json:"file_count"`
	Error        *string `json:"error"`
}

type TestResult struct {
	OK           bool   `json:"ok"`
	Version      string `json:"version,omitempty"`
	TorrentCount int    `json:"torrent_count,omitempty"`
	Error        string `json:"error,omitempty"`
}

type MatchedEntry struct {
	Entry     TorrentFileEntry
	MatchType string // "path" or "name_size"
}

type nameSizeKey struct {
	NameLower string
	Size      int64
}

type Index struct {
	ByPath     map[string][]TorrentFileEntry
	ByPathCI   map[string][]TorrentFileEntry
	ByNameSize map[nameSizeKey][]TorrentFileEntry
	Instances  []InstanceReport
	AllEntries []TorrentFileEntry
}

func NewIndex() *Index {
	return &Index{
		ByPath:     make(map[string][]TorrentFileEntry),
		ByPathCI:   make(map[string][]TorrentFileEntry),
		ByNameSize: make(map[nameSizeKey][]TorrentFileEntry),
		Instances:  make([]InstanceReport, 0),
		AllEntries: make([]TorrentFileEntry, 0),
	}
}

func (idx *Index) AddEntry(e TorrentFileEntry) {
	idx.AllEntries = append(idx.AllEntries, e)
	norm := NormalizeSlashPath(e.MappedPath)
	idx.ByPath[norm] = append(idx.ByPath[norm], e)
	lower := strings.ToLower(norm)
	idx.ByPathCI[lower] = append(idx.ByPathCI[lower], e)
	k := nameSizeKey{NameLower: strings.ToLower(e.Filename), Size: e.Size}
	idx.ByNameSize[k] = append(idx.ByNameSize[k], e)
}

func (idx *Index) FindMatches(absPath, filename string, size int64) []MatchedEntry {
	if idx == nil {
		return nil
	}
	norm := NormalizeSlashPath(absPath)
	if list, ok := idx.ByPath[norm]; ok {
		out := make([]MatchedEntry, 0, len(list))
		for _, e := range list {
			out = append(out, MatchedEntry{Entry: e, MatchType: "path"})
		}
		return out
	}
	if list, ok := idx.ByPathCI[strings.ToLower(norm)]; ok {
		out := make([]MatchedEntry, 0, len(list))
		for _, e := range list {
			out = append(out, MatchedEntry{Entry: e, MatchType: "path"})
		}
		return out
	}
	k := nameSizeKey{NameLower: strings.ToLower(filename), Size: size}
	if list, ok := idx.ByNameSize[k]; ok {
		out := make([]MatchedEntry, 0, len(list))
		for _, e := range list {
			out = append(out, MatchedEntry{Entry: e, MatchType: "name_size"})
		}
		return out
	}
	return nil
}

func NormalizeSlashPath(pathStr string) string {
	cleaned := strings.ReplaceAll(strings.TrimSpace(pathStr), "\\", "/")
	if strings.HasPrefix(cleaned, "//") {
		rest := cleaned[2:]
		for strings.Contains(rest, "//") {
			rest = strings.ReplaceAll(rest, "//", "/")
		}
		cleaned = "//" + rest
	} else {
		for strings.Contains(cleaned, "//") {
			cleaned = strings.ReplaceAll(cleaned, "//", "/")
		}
	}
	if len(cleaned) > 1 {
		cleaned = strings.TrimRight(cleaned, "/")
		if cleaned == "" {
			cleaned = "/"
		}
	}
	return cleaned
}

func ApplyPathMappings(pathStr string, mappings []config.PathMapping) string {
	norm := NormalizeSlashPath(pathStr)
	for _, m := range mappings {
		remote := NormalizeSlashPath(m.RemotePrefix)
		local := NormalizeSlashPath(m.LocalPrefix)
		if remote == "" {
			continue
		}
		if norm == remote {
			return local
		}
		if strings.HasPrefix(norm, remote+"/") {
			suffix := norm[len(remote):]
			return NormalizeSlashPath(local + suffix)
		}
		if runtime.GOOS == "windows" && len(norm) > len(remote) &&
			strings.EqualFold(norm[:len(remote)+1], remote+"/") {
			suffix := norm[len(remote):]
			return NormalizeSlashPath(local + suffix)
		}
	}
	return norm
}

// ToRemotePath reverse-maps a local host/container path back to an instance's remote path.
func ToRemotePath(localPath string, mappings []config.PathMapping) string {
	norm := NormalizeSlashPath(localPath)
	for _, m := range mappings {
		remote := NormalizeSlashPath(m.RemotePrefix)
		local := NormalizeSlashPath(m.LocalPrefix)
		if local == "" {
			continue
		}
		if norm == local || (runtime.GOOS == "windows" && strings.EqualFold(norm, local)) {
			return remote
		}
		if strings.HasPrefix(norm, local+"/") {
			suffix := norm[len(local):]
			return NormalizeSlashPath(remote + suffix)
		}
		if runtime.GOOS == "windows" && len(norm) > len(local) &&
			strings.EqualFold(norm[:len(local)+1], local+"/") {
			suffix := norm[len(local):]
			return NormalizeSlashPath(remote + suffix)
		}
	}
	return norm
}

var (
	urlStripPattern      = regexp.MustCompile(`(?i)https?://\S+`)
	unregisteredPhrases = []string{
		"unregistered",
		"not registered",
		"torrent not found",
		"infohash not found",
		"torrent does not exist",
		"not exist",
		"torrent has been deleted",
		"torrent has been nuked",
		"torrent has been rejected",
		"torrent is not authorized",
		"torrent is not found",
		"nuked",
		"trump",
		"dupe",
		"complete season uploaded",
		"season pack",
		"pack is available",
		"packs are available",
		"repack available",
		"internal available",
		"grab internal",
		"dead",
		"upgraded",
		"uploaded",
		"retitled",
		"specifically banned",
		"problem with",
		"other:",
		"unknown",
		"tidak terdaftar",
		"nicht registriert",
		"nicht gefunden",
		"existiert nicht",
		"introuvable",
		"não registrado",
		"nem található",
	}
	authErrorPhrases = []string{
		"passkey",
		"torrent pass",
		"user does not exist",
	}
)

func MatchesUnregisteredMessage(msg string) bool {
	text := strings.ToLower(strings.TrimSpace(msg))
	if text == "" {
		return false
	}
	text = strings.ReplaceAll(text, "(unknown http error)", "")

	matched := false
	for _, phrase := range unregisteredPhrases {
		if strings.Contains(text, phrase) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}

	authText := text
	if strings.Contains(authText, "://") {
		authText = urlStripPattern.ReplaceAllString(authText, "")
	}
	for _, phrase := range authErrorPhrases {
		if strings.Contains(authText, phrase) {
			return false
		}
	}
	return true
}

func EvaluateTrackerHealth(state string, trackers []torrentTrackerJSON) (string, string) {
	if state == "error" || state == "missingFiles" {
		return "LOCAL_ERROR", "qBittorrent state: " + state
	}

	var hasWorking bool
	var hasStatusError bool
	var unregisteredMsg string
	var errorMsg string

	for _, tr := range trackers {
		if tr.Status == 0 || strings.HasPrefix(tr.URL, "** [") {
			continue
		}
		if tr.Status == 2 {
			hasWorking = true
			continue
		}
		trimmedMsg := strings.TrimSpace(tr.Msg)
		if (tr.Status == 3 || tr.Status >= 4) && MatchesUnregisteredMessage(trimmedMsg) {
			if unregisteredMsg == "" {
				unregisteredMsg = trimmedMsg
			}
		}
		if tr.Status >= 4 {
			hasStatusError = true
			if errorMsg == "" && trimmedMsg != "" {
				errorMsg = trimmedMsg
			}
		}
	}

	if !hasWorking {
		if unregisteredMsg != "" {
			return "UNREGISTERED", unregisteredMsg
		}
		if errorMsg != "" {
			return "TRACKER_ERROR", errorMsg
		}
		if hasStatusError {
			return "TRACKER_ERROR", "Tracker not working"
		}
	}
	return "", ""
}

func ClassifyTorrentState(state, trackerHealth string) (bool, string) {
	if trackerHealth == "UNREGISTERED" {
		return false, "unregistered"
	}
	if trackerHealth == "LOCAL_ERROR" || trackerHealth == "TRACKER_ERROR" {
		return false, "error"
	}
	if _, ok := seedingStates[state]; ok {
		return true, "seeding"
	}
	if _, ok := pausedUpStates[state]; ok {
		return false, "paused"
	}
	if _, ok := downloadingStates[state]; ok {
		return false, "downloading"
	}
	return false, "other"
}

func ExtractTrackerHost(trackerURL string) string {
	trimmed := strings.TrimSpace(trackerURL)
	if trimmed == "" {
		return ""
	}
	u, err := url.Parse(trimmed)
	if err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return trimmed
}

type torrentTrackerJSON struct {
	URL    string `json:"url"`
	Status int    `json:"status"`
	Msg    string `json:"msg"`
}

type torrentInfoJSON struct {
	Hash     string               `json:"hash"`
	Name     string               `json:"name"`
	SavePath string               `json:"save_path"`
	State    string               `json:"state"`
	Ratio    float64              `json:"ratio"`
	Tracker  string               `json:"tracker"`
	Trackers []torrentTrackerJSON `json:"trackers"`
}

type torrentFileJSON struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type Client struct {
	timeout   time.Duration
	transport *http.Transport
}

func NewClient(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &Client{
		timeout:   timeout,
		transport: transport,
	}
}

func (c *Client) newAuthedHTTPClient(ctx context.Context, inst config.QbitInstanceConfig) (*http.Client, string, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(inst.URL), "/")
	if baseURL == "" {
		return nil, "", errors.New("empty qBittorrent URL")
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, "", err
	}
	hc := &http.Client{
		Timeout:   c.timeout,
		Jar:       jar,
		Transport: c.transport,
	}

	form := url.Values{}
	form.Set("username", inst.Username)
	form.Set("password", inst.Password)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v2/auth/login", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	bodyText := strings.TrimSpace(string(bodyBytes))

	if resp.StatusCode == http.StatusForbidden {
		return nil, "", errors.New("qBittorrent authentication banned or forbidden (HTTP 403)")
	}
	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("qBittorrent login failed with HTTP %d: %s", resp.StatusCode, bodyText)
	}
	if bodyText != "" && bodyText != "Ok." {
		return nil, "", fmt.Errorf("qBittorrent login rejected: %s", bodyText)
	}
	return hc, baseURL, nil
}

func doGetJSON(ctx context.Context, hc *http.Client, fullURL string, dest any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, fullURL, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(dest)
}

func doGetText(ctx context.Context, hc *http.Client, fullURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return strings.TrimSpace(string(b)), nil
}

func (c *Client) TestConnection(ctx context.Context, inst config.QbitInstanceConfig) TestResult {
	hc, baseURL, err := c.newAuthedHTTPClient(ctx, inst)
	if err != nil {
		return TestResult{OK: false, Error: err.Error()}
	}
	ver, err := doGetText(ctx, hc, baseURL+"/api/v2/app/version")
	if err != nil {
		return TestResult{OK: false, Error: err.Error()}
	}
	var torrents []torrentInfoJSON
	if err := doGetJSON(ctx, hc, baseURL+"/api/v2/torrents/info", &torrents); err != nil {
		return TestResult{OK: false, Error: err.Error()}
	}
	return TestResult{
		OK:           true,
		Version:      ver,
		TorrentCount: len(torrents),
	}
}

func (c *Client) FetchInstanceEntries(ctx context.Context, inst config.QbitInstanceConfig) (InstanceReport, []TorrentFileEntry) {
	if !inst.Enabled {
		disabledMsg := "Disabled"
		return InstanceReport{
			InstanceID:   inst.ID,
			InstanceName: inst.Name,
			URL:          inst.URL,
			OK:           true,
			Error:        &disabledMsg,
		}, nil
	}

	hc, baseURL, err := c.newAuthedHTTPClient(ctx, inst)
	if err != nil {
		msg := err.Error()
		return InstanceReport{
			InstanceID:   inst.ID,
			InstanceName: inst.Name,
			URL:          inst.URL,
			OK:           false,
			Error:        &msg,
		}, nil
	}

	var torrents []torrentInfoJSON
	if err := doGetJSON(ctx, hc, baseURL+"/api/v2/torrents/info?includeTrackers=true", &torrents); err != nil {
		msg := err.Error()
		return InstanceReport{
			InstanceID:   inst.ID,
			InstanceName: inst.Name,
			URL:          inst.URL,
			OK:           false,
			Error:        &msg,
		}, nil
	}

	entries, errs := c.enrichTorrentsEntries(ctx, hc, baseURL, inst, torrents)

	var errPtr *string
	if len(errs) > 0 {
		joined := strings.Join(errs, "; ")
		errPtr = &joined
	}

	return InstanceReport{
		InstanceID:   inst.ID,
		InstanceName: inst.Name,
		URL:          inst.URL,
		OK:           len(errs) == 0,
		TorrentCount: len(torrents),
		FileCount:    len(entries),
		Error:        errPtr,
	}, entries
}

// enrichTorrentsEntries fetches per-torrent file lists and fallback tracker statuses concurrently.
func (c *Client) enrichTorrentsEntries(ctx context.Context, hc *http.Client, baseURL string, inst config.QbitInstanceConfig, torrents []torrentInfoJSON) ([]TorrentFileEntry, []string) {
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	var mu sync.Mutex
	entries := make([]TorrentFileEntry, 0, len(torrents))
	errs := make([]string, 0)

	for _, t := range torrents {
		if strings.TrimSpace(t.Hash) == "" {
			continue
		}
		wg.Add(1)
		go func(torrent torrentInfoJSON) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			var files []torrentFileJSON
			filesURL := baseURL + "/api/v2/torrents/files?hash=" + url.QueryEscape(torrent.Hash)
			if fErr := doGetJSON(ctx, hc, filesURL, &files); fErr != nil {
				mu.Lock()
				if len(errs) < 3 {
					errs = append(errs, fErr.Error())
				}
				mu.Unlock()
				return
			}

			if len(torrent.Trackers) == 0 {
				var trackers []torrentTrackerJSON
				trackersURL := baseURL + "/api/v2/torrents/trackers?hash=" + url.QueryEscape(torrent.Hash)
				if tErr := doGetJSON(ctx, hc, trackersURL, &trackers); tErr == nil {
					torrent.Trackers = trackers
				}
			}

			built := buildEntriesForTorrent(inst, torrent, files)
			mu.Lock()
			entries = append(entries, built...)
			mu.Unlock()
		}(t)
	}
	wg.Wait()
	return entries, errs
}

func buildEntriesForTorrent(inst config.QbitInstanceConfig, t torrentInfoJSON, files []torrentFileJSON) []TorrentFileEntry {
	trackerHealth, trackerMsg := EvaluateTrackerHealth(t.State, t.Trackers)
	isSeeding, seedingStatus := ClassifyTorrentState(t.State, trackerHealth)
	trackerHost := ExtractTrackerHost(t.Tracker)
	savePath := NormalizeSlashPath(t.SavePath)
	ratio := math.Round(t.Ratio*100) / 100

	out := make([]TorrentFileEntry, 0, len(files))
	for _, f := range files {
		relName := strings.TrimLeft(NormalizeSlashPath(f.Name), "/")
		if relName == "" {
			continue
		}
		combined := relName
		if savePath != "" {
			combined = savePath + "/" + relName
		}
		mapped := ApplyPathMappings(combined, inst.PathMappings)
		filename := path.Base(relName)
		out = append(out, TorrentFileEntry{
			InstanceID:    inst.ID,
			InstanceName:  inst.Name,
			TorrentHash:   t.Hash,
			TorrentName:   t.Name,
			State:         t.State,
			IsSeeding:     isSeeding,
			SeedingStatus: seedingStatus,
			Ratio:         ratio,
			Tracker:       t.Tracker,
			TrackerHost:   trackerHost,
			TrackerHealth: trackerHealth,
			TrackerMsg:    trackerMsg,
			SavePath:         savePath,
			RelativeName:     relName,
			MappedPath:       mapped,
			Filename:         filename,
			Size:             f.Size,
			TorrentFileCount: len(files),
		})
	}
	return out
}

func (c *Client) BuildIndex(ctx context.Context, instances []config.QbitInstanceConfig) *Index {
	idx := NewIndex()
	if len(instances) == 0 {
		return idx
	}

	type result struct {
		pos     int
		report  InstanceReport
		entries []TorrentFileEntry
	}

	results := make([]result, len(instances))
	var wg sync.WaitGroup
	for i, inst := range instances {
		wg.Add(1)
		go func(pos int, cfg config.QbitInstanceConfig) {
			defer wg.Done()
			rep, entries := c.FetchInstanceEntries(ctx, cfg)
			results[pos] = result{pos: pos, report: rep, entries: entries}
		}(i, inst)
	}
	wg.Wait()

	for _, r := range results {
		idx.Instances = append(idx.Instances, r.report)
		for _, e := range r.entries {
			idx.AddEntry(e)
		}
	}
	return idx
}

func uniqueNonEmptyHashes(hashes []string) ([]string, map[string]struct{}) {
	seen := make(map[string]struct{}, len(hashes))
	unique := make([]string, 0, len(hashes))
	for _, h := range hashes {
		trimmed := strings.TrimSpace(h)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		if _, ok := seen[lower]; ok {
			continue
		}
		seen[lower] = struct{}{}
		unique = append(unique, trimmed)
	}
	return unique, seen
}

// FetchTorrentsByHashes fetches file and tracker entries for only the specified torrent hashes.
func (c *Client) FetchTorrentsByHashes(ctx context.Context, inst config.QbitInstanceConfig, hashes []string) ([]TorrentFileEntry, error) {
	if !inst.Enabled {
		return nil, nil
	}
	unique, wantSet := uniqueNonEmptyHashes(hashes)
	if len(unique) == 0 {
		return nil, nil
	}

	hc, baseURL, err := c.newAuthedHTTPClient(ctx, inst)
	if err != nil {
		return nil, err
	}

	const batchSize = 50
	matchedTorrents := make([]torrentInfoJSON, 0, len(unique))
	for i := 0; i < len(unique); i += batchSize {
		end := i + batchSize
		if end > len(unique) {
			end = len(unique)
		}
		infoURL := baseURL + "/api/v2/torrents/info?hashes=" + url.QueryEscape(strings.Join(unique[i:end], "|")) + "&includeTrackers=true"
		var batch []torrentInfoJSON
		if err := doGetJSON(ctx, hc, infoURL, &batch); err != nil {
			return nil, err
		}
		for _, t := range batch {
			if _, ok := wantSet[strings.ToLower(strings.TrimSpace(t.Hash))]; ok {
				matchedTorrents = append(matchedTorrents, t)
			}
		}
	}

	if len(matchedTorrents) == 0 {
		return nil, nil
	}

	entries, errs := c.enrichTorrentsEntries(ctx, hc, baseURL, inst, matchedTorrents)
	if len(errs) > 0 {
		return entries, errors.New(strings.Join(errs, "; "))
	}
	return entries, nil
}

// BuildIndexForHashes constructs a targeted qBittorrent index querying only the requested hashes per instance.
func (c *Client) BuildIndexForHashes(ctx context.Context, instances []config.QbitInstanceConfig, hashesByInstance map[string][]string) *Index {
	idx := NewIndex()
	if len(instances) == 0 || len(hashesByInstance) == 0 {
		return idx
	}

	type result struct {
		report  InstanceReport
		entries []TorrentFileEntry
		present bool
	}

	results := make([]result, len(instances))
	var wg sync.WaitGroup
	for i, inst := range instances {
		hashes := hashesByInstance[inst.ID]
		if !inst.Enabled || len(hashes) == 0 {
			continue
		}
		wg.Add(1)
		go func(pos int, cfg config.QbitInstanceConfig, targetHashes []string) {
			defer wg.Done()
			entries, err := c.FetchTorrentsByHashes(ctx, cfg, targetHashes)
			rep := InstanceReport{
				InstanceID:   cfg.ID,
				InstanceName: cfg.Name,
				URL:          cfg.URL,
				OK:           err == nil,
				TorrentCount: len(targetHashes),
				FileCount:    len(entries),
			}
			if err != nil {
				msg := err.Error()
				rep.Error = &msg
			}
			results[pos] = result{report: rep, entries: entries, present: true}
		}(i, inst, hashes)
	}
	wg.Wait()

	for _, r := range results {
		if !r.present {
			continue
		}
		idx.Instances = append(idx.Instances, r.report)
		for _, e := range r.entries {
			idx.AddEntry(e)
		}
	}
	return idx
}

func doPostForm(ctx context.Context, hc *http.Client, fullURL string, form url.Values) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := hc.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return resp.StatusCode, strings.TrimSpace(string(b)), nil
}

func postFormChecked(ctx context.Context, hc *http.Client, fullURL string, form url.Values, opName string) error {
	status, body, err := doPostForm(ctx, hc, fullURL, form)
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("%s failed with HTTP %d: %s", opName, status, body)
	}
	return nil
}

func postFormWithFallback(ctx context.Context, hc *http.Client, primaryURL, fallbackURL string, form url.Values, opName string) error {
	status, body, err := doPostForm(ctx, hc, primaryURL, form)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound && fallbackURL != "" {
		return postFormChecked(ctx, hc, fallbackURL, form, opName)
	}
	if status >= 400 {
		return fmt.Errorf("%s failed with HTTP %d: %s", opName, status, body)
	}
	return nil
}

func (c *Client) DeleteTorrents(ctx context.Context, inst config.QbitInstanceConfig, hashes []string, deleteFiles bool) ([]string, error) {
	unique, _ := uniqueNonEmptyHashes(hashes)
	if len(unique) == 0 {
		return []string{}, nil
	}

	hc, baseURL, err := c.newAuthedHTTPClient(ctx, inst)
	if err != nil {
		return nil, err
	}

	form := url.Values{}
	form.Set("hashes", strings.Join(unique, "|"))
	if deleteFiles {
		form.Set("deleteFiles", "true")
	} else {
		form.Set("deleteFiles", "false")
	}

	if err := postFormChecked(ctx, hc, baseURL+"/api/v2/torrents/delete", form, "qBittorrent delete"); err != nil {
		return nil, err
	}
	return unique, nil
}

type RelocateTorrentOutcome struct {
	InstanceID   string `json:"instance_id"`
	InstanceName string `json:"instance_name"`
	TorrentHash  string `json:"torrent_hash"`
	TorrentName  string `json:"torrent_name"`
	FileCount    int    `json:"file_count"`
	OldSavePath  string `json:"old_save_path"`
	OldRelPath   string `json:"old_rel_path"`
	NewSavePath  string `json:"new_save_path"`
	NewRelPath   string `json:"new_rel_path"`
	Renamed      bool   `json:"renamed"`
}

// RelocateSingleFileTorrent moves and (if needed) renames a single-file torrent in qBittorrent
// so it can continue seeding from the new path created by Radarr/Sonarr Move imports.
func (c *Client) RelocateSingleFileTorrent(ctx context.Context, inst config.QbitInstanceConfig, hash, newRemotePath string, dryRun bool) (RelocateTorrentOutcome, error) {
	trimmedHash := strings.TrimSpace(hash)
	if trimmedHash == "" {
		return RelocateTorrentOutcome{}, errors.New("missing torrent hash")
	}
	newRemote := NormalizeSlashPath(newRemotePath)
	if newRemote == "" {
		return RelocateTorrentOutcome{}, errors.New("missing target remote path")
	}

	hc, baseURL, err := c.newAuthedHTTPClient(ctx, inst)
	if err != nil {
		return RelocateTorrentOutcome{}, err
	}

	var infos []torrentInfoJSON
	infoURL := baseURL + "/api/v2/torrents/info?hashes=" + url.QueryEscape(trimmedHash)
	if err := doGetJSON(ctx, hc, infoURL, &infos); err != nil {
		return RelocateTorrentOutcome{}, err
	}
	var targetInfo *torrentInfoJSON
	for i := range infos {
		if strings.EqualFold(strings.TrimSpace(infos[i].Hash), trimmedHash) {
			targetInfo = &infos[i]
			break
		}
	}
	if targetInfo == nil {
		return RelocateTorrentOutcome{}, fmt.Errorf("torrent %s not found in qBittorrent", trimmedHash)
	}

	var files []torrentFileJSON
	filesURL := baseURL + "/api/v2/torrents/files?hash=" + url.QueryEscape(targetInfo.Hash)
	if err := doGetJSON(ctx, hc, filesURL, &files); err != nil {
		return RelocateTorrentOutcome{}, err
	}
	if len(files) != 1 {
		return RelocateTorrentOutcome{}, fmt.Errorf("skipping multi-file torrent (%d files); only single-file torrents can be auto-relocated", len(files))
	}

	oldRelPath := strings.TrimLeft(NormalizeSlashPath(files[0].Name), "/")
	if oldRelPath == "" {
		return RelocateTorrentOutcome{}, errors.New("empty relative file path in torrent")
	}

	var newSavePath, newRelPath string
	if !strings.Contains(oldRelPath, "/") {
		newSavePath = path.Dir(newRemote)
		newRelPath = path.Base(newRemote)
	} else {
		parentDir := path.Dir(newRemote)
		grandParent := path.Dir(parentDir)
		if grandParent != "" && grandParent != "." && grandParent != parentDir {
			newSavePath = grandParent
			newRelPath = path.Base(parentDir) + "/" + path.Base(newRemote)
		} else {
			newSavePath = parentDir
			newRelPath = path.Base(newRemote)
		}
	}

	outcome := RelocateTorrentOutcome{
		InstanceID:   inst.ID,
		InstanceName: inst.Name,
		TorrentHash:  targetInfo.Hash,
		TorrentName:  targetInfo.Name,
		FileCount:    len(files),
		OldSavePath:  NormalizeSlashPath(targetInfo.SavePath),
		OldRelPath:   oldRelPath,
		NewSavePath:  NormalizeSlashPath(newSavePath),
		NewRelPath:   newRelPath,
		Renamed:      oldRelPath != newRelPath,
	}
	if dryRun {
		return outcome, nil
	}

	hashForm := url.Values{}
	hashForm.Set("hashes", targetInfo.Hash)

	if err := postFormWithFallback(ctx, hc, baseURL+"/api/v2/torrents/stop", baseURL+"/api/v2/torrents/pause", hashForm, "qBittorrent stop"); err != nil {
		return RelocateTorrentOutcome{}, err
	}

	tmmForm := url.Values{}
	tmmForm.Set("hashes", targetInfo.Hash)
	tmmForm.Set("enable", "false")
	if err := postFormChecked(ctx, hc, baseURL+"/api/v2/torrents/setAutoManagement", tmmForm, "qBittorrent setAutoManagement"); err != nil {
		return RelocateTorrentOutcome{}, err
	}

	locForm := url.Values{}
	locForm.Set("hashes", targetInfo.Hash)
	locForm.Set("location", outcome.NewSavePath)
	if err := postFormChecked(ctx, hc, baseURL+"/api/v2/torrents/setLocation", locForm, "qBittorrent setLocation"); err != nil {
		return RelocateTorrentOutcome{}, err
	}

	if outcome.Renamed {
		renForm := url.Values{}
		renForm.Set("hash", targetInfo.Hash)
		renForm.Set("oldPath", oldRelPath)
		renForm.Set("newPath", newRelPath)
		if err := postFormChecked(ctx, hc, baseURL+"/api/v2/torrents/renameFile", renForm, "qBittorrent renameFile"); err != nil {
			return RelocateTorrentOutcome{}, err
		}
	}

	if err := postFormChecked(ctx, hc, baseURL+"/api/v2/torrents/recheck", hashForm, "qBittorrent recheck"); err != nil {
		return RelocateTorrentOutcome{}, err
	}

	if err := postFormWithFallback(ctx, hc, baseURL+"/api/v2/torrents/start", baseURL+"/api/v2/torrents/resume", hashForm, "qBittorrent start"); err != nil {
		return RelocateTorrentOutcome{}, err
	}

	return outcome, nil
}
