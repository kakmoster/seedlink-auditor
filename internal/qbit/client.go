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

type TorrentFileInfo struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type TorrentDetails struct {
	InstanceID   string            `json:"instance_id"`
	InstanceName string            `json:"instance_name"`
	Hash         string            `json:"hash"`
	Name         string            `json:"name"`
	SavePath     string            `json:"save_path"`
	State        string            `json:"state"`
	Files        []TorrentFileInfo `json:"files"`
}

// FetchTorrentsDetails fetches torrent metadata and file lists for the given hashes from one instance.
func (c *Client) FetchTorrentsDetails(ctx context.Context, inst config.QbitInstanceConfig, hashes []string) (map[string]TorrentDetails, error) {
	if !inst.Enabled {
		return nil, fmt.Errorf("qBittorrent instance %q is disabled", inst.Name)
	}
	unique, wantSet := uniqueNonEmptyHashes(hashes)
	if len(unique) == 0 {
		return map[string]TorrentDetails{}, nil
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
		infoURL := baseURL + "/api/v2/torrents/info?hashes=" + url.QueryEscape(strings.Join(unique[i:end], "|"))
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

	out := make(map[string]TorrentDetails, len(matchedTorrents))
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for _, t := range matchedTorrents {
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
				if firstErr == nil {
					firstErr = fErr
				}
				mu.Unlock()
				return
			}

			fileInfos := make([]TorrentFileInfo, 0, len(files))
			for _, f := range files {
				relName := strings.TrimLeft(NormalizeSlashPath(f.Name), "/")
				if relName == "" {
					continue
				}
				fileInfos = append(fileInfos, TorrentFileInfo{
					Name: relName,
					Size: f.Size,
				})
			}

			key := strings.ToLower(strings.TrimSpace(torrent.Hash))
			mu.Lock()
			out[key] = TorrentDetails{
				InstanceID:   inst.ID,
				InstanceName: inst.Name,
				Hash:         torrent.Hash,
				Name:         torrent.Name,
				SavePath:     NormalizeSlashPath(torrent.SavePath),
				State:        torrent.State,
				Files:        fileInfos,
			}
			mu.Unlock()
		}(t)
	}
	wg.Wait()

	if firstErr != nil && len(out) == 0 {
		return nil, firstErr
	}
	return out, nil
}

// FetchTorrentDetails fetches metadata and file list for a single torrent hash.
func (c *Client) FetchTorrentDetails(ctx context.Context, inst config.QbitInstanceConfig, hash string) (TorrentDetails, error) {
	trimmed := strings.TrimSpace(hash)
	if trimmed == "" {
		return TorrentDetails{}, errors.New("missing torrent hash")
	}
	detailsMap, err := c.FetchTorrentsDetails(ctx, inst, []string{trimmed})
	if err != nil {
		return TorrentDetails{}, err
	}
	d, ok := detailsMap[strings.ToLower(trimmed)]
	if !ok {
		return TorrentDetails{}, fmt.Errorf("torrent %s not found in qBittorrent", trimmed)
	}
	return d, nil
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

func postFormRetry409(ctx context.Context, hc *http.Client, fullURL string, form url.Values, opName string) error {
	const maxAttempts = 6
	for attempt := 0; attempt < maxAttempts; attempt++ {
		status, body, err := doPostForm(ctx, hc, fullURL, form)
		if err != nil {
			return err
		}
		if status == http.StatusConflict && attempt < maxAttempts-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(150 * time.Millisecond):
				continue
			}
		}
		if status >= 400 {
			return fmt.Errorf("%s failed with HTTP %d: %s", opName, status, body)
		}
		return nil
	}
	return fmt.Errorf("%s failed after retries", opName)
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

type RelocatedFileChange struct {
	OldRelPath string `json:"old_rel_path"`
	NewRelPath string `json:"new_rel_path"`
	Renamed    bool   `json:"renamed"`
}

type MovedCompanionRecord struct {
	OldLocalPath string `json:"old_local_path"`
	NewLocalPath string `json:"new_local_path"`
	Kind         string `json:"kind"`
}

type RelocateTorrentOutcome struct {
	InstanceID      string                 `json:"instance_id"`
	InstanceName    string                 `json:"instance_name"`
	TorrentHash     string                 `json:"torrent_hash"`
	TorrentName     string                 `json:"torrent_name"`
	FileCount       int                    `json:"file_count"`
	OldSavePath     string                 `json:"old_save_path"`
	OldRelPath      string                 `json:"old_rel_path"`
	NewSavePath     string                 `json:"new_save_path"`
	NewRelPath      string                 `json:"new_rel_path"`
	Renamed         bool                   `json:"renamed"`
	RenamedCount    int                    `json:"renamed_count,omitempty"`
	FileChanges     []RelocatedFileChange  `json:"file_changes,omitempty"`
	MovedCompanions []MovedCompanionRecord `json:"moved_companions,omitempty"`
	Warnings        []string               `json:"warnings,omitempty"`
}

func splitDirSegments(normDir string) (prefix string, segs []string) {
	switch {
	case strings.HasPrefix(normDir, "//"):
		trimmed := strings.TrimLeft(normDir[2:], "/")
		if trimmed == "" {
			return "//", nil
		}
		return "//", strings.Split(trimmed, "/")
	case strings.HasPrefix(normDir, "/"):
		trimmed := strings.TrimLeft(normDir[1:], "/")
		if trimmed == "" {
			return "/", nil
		}
		return "/", strings.Split(trimmed, "/")
	default:
		if normDir == "" || normDir == "." {
			return "", nil
		}
		return "", strings.Split(normDir, "/")
	}
}

func joinDirSegments(prefix string, segs []string) string {
	switch prefix {
	case "/":
		if len(segs) == 0 {
			return "/"
		}
		return "/" + strings.Join(segs, "/")
	case "//":
		if len(segs) < 2 {
			return ""
		}
		return "//" + strings.Join(segs, "/")
	default:
		if len(segs) == 0 {
			return ""
		}
		if len(segs) == 1 && len(segs[0]) == 2 && segs[0][1] == ':' {
			return segs[0] + "/"
		}
		return strings.Join(segs, "/")
	}
}

func isValidSavePath(p string) bool {
	norm := NormalizeSlashPath(p)
	if norm == "" || norm == "." || norm == "/" || norm == "//" {
		return false
	}
	if len(norm) >= 2 && norm[1] == ':' {
		rest := strings.Trim(norm[2:], "/")
		if rest == "" {
			return false
		}
	}
	if strings.HasPrefix(norm, "//") {
		_, segs := splitDirSegments(norm)
		if len(segs) < 2 {
			return false
		}
	}
	return true
}

// CommonSlashDir returns the longest common parent directory across normalized file paths.
func CommonSlashDir(paths []string, caseInsensitive bool) string {
	var commonPrefix string
	var commonSegs []string
	initialized := false

	for _, raw := range paths {
		norm := NormalizeSlashPath(raw)
		if norm == "" {
			continue
		}
		dir := NormalizeSlashPath(path.Dir(norm))
		pref, segs := splitDirSegments(dir)
		if !initialized {
			commonPrefix = pref
			commonSegs = segs
			initialized = true
			continue
		}
		if pref != commonPrefix {
			return ""
		}
		limit := len(commonSegs)
		if len(segs) < limit {
			limit = len(segs)
		}
		matchLen := 0
		for matchLen < limit {
			a, b := commonSegs[matchLen], segs[matchLen]
			equal := a == b || (caseInsensitive && strings.EqualFold(a, b))
			if !equal {
				break
			}
			matchLen++
		}
		commonSegs = commonSegs[:matchLen]
	}

	if !initialized {
		return ""
	}
	return joinDirSegments(commonPrefix, commonSegs)
}

func stripBaseSavePath(base, fullPath string) (string, bool) {
	b := NormalizeSlashPath(base)
	f := NormalizeSlashPath(fullPath)
	if b == "" || f == "" || len(f) <= len(b) {
		return "", false
	}
	prefix := b + "/"
	if b == "/" {
		prefix = "/"
	}
	if len(f) <= len(prefix) {
		return "", false
	}
	if strings.HasPrefix(f, prefix) {
		return strings.TrimLeft(f[len(prefix):], "/"), true
	}
	if strings.EqualFold(f[:len(prefix)], prefix) {
		return strings.TrimLeft(f[len(prefix):], "/"), true
	}
	return "", false
}

// RelocateTorrent moves a single-file or multi-file torrent to a new save_path and renames each
// file to its new relative path so qBittorrent can keep seeding after Arr Move imports.
func (c *Client) RelocateTorrent(ctx context.Context, inst config.QbitInstanceConfig, hash string, remoteTargets map[string]string, dryRun bool) (RelocateTorrentOutcome, error) {
	trimmedHash := strings.TrimSpace(hash)
	if trimmedHash == "" {
		return RelocateTorrentOutcome{}, errors.New("missing torrent hash")
	}

	normTargets := make(map[string]string, len(remoteTargets))
	lowerTargets := make(map[string]string, len(remoteTargets))
	for k, v := range remoteTargets {
		nk := strings.TrimLeft(NormalizeSlashPath(k), "/")
		nv := NormalizeSlashPath(v)
		if nk == "" || nv == "" {
			continue
		}
		normTargets[nk] = nv
		lowerTargets[strings.ToLower(nk)] = nv
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
	if len(files) == 0 {
		return RelocateTorrentOutcome{}, errors.New("torrent contains no files")
	}

	oldRels := make([]string, 0, len(files))
	targetRemotes := make([]string, 0, len(files))
	unmapped := make([]string, 0)

	for _, f := range files {
		oldRel := strings.TrimLeft(NormalizeSlashPath(f.Name), "/")
		if oldRel == "" {
			return RelocateTorrentOutcome{}, errors.New("empty relative file path in torrent")
		}
		targetRemote, ok := normTargets[oldRel]
		if !ok {
			targetRemote, ok = lowerTargets[strings.ToLower(oldRel)]
		}
		if !ok || targetRemote == "" {
			unmapped = append(unmapped, oldRel)
			continue
		}
		oldRels = append(oldRels, oldRel)
		targetRemotes = append(targetRemotes, targetRemote)
	}
	if len(unmapped) > 0 {
		limit := len(unmapped)
		if limit > 3 {
			limit = 3
		}
		return RelocateTorrentOutcome{}, fmt.Errorf("missing target remote path for %d torrent file(s): %s", len(unmapped), strings.Join(unmapped[:limit], ", "))
	}

	var newSavePath string
	newRels := make([]string, len(files))

	if len(files) == 1 {
		oldRelPath := oldRels[0]
		newRemote := targetRemotes[0]
		if !strings.Contains(oldRelPath, "/") {
			newSavePath = path.Dir(newRemote)
			newRels[0] = path.Base(newRemote)
		} else {
			parentDir := path.Dir(newRemote)
			grandParent := path.Dir(parentDir)
			if grandParent != "" && grandParent != "." && grandParent != parentDir && isValidSavePath(grandParent) {
				newSavePath = grandParent
				newRels[0] = path.Base(parentDir) + "/" + path.Base(newRemote)
			} else {
				newSavePath = parentDir
				newRels[0] = path.Base(newRemote)
			}
		}
		if !isValidSavePath(newSavePath) {
			return RelocateTorrentOutcome{}, fmt.Errorf("invalid target save path %q", newSavePath)
		}
	} else {
		commonDir := CommonSlashDir(targetRemotes, runtime.GOOS == "windows")
		if !isValidSavePath(commonDir) {
			return RelocateTorrentOutcome{}, fmt.Errorf("target files do not share a valid common directory (got %q)", commonDir)
		}

		// Preserve wrapped torrent folder semantics (e.g. Show/Season 13/Ep.mkv) when all
		// original torrent files shared a single top-level directory segment.
		sharedTopDir := ""
		allShareTopDir := true
		for _, oldRel := range oldRels {
			slashIdx := strings.IndexByte(oldRel, '/')
			if slashIdx <= 0 {
				allShareTopDir = false
				break
			}
			top := oldRel[:slashIdx]
			if sharedTopDir == "" {
				sharedTopDir = top
			} else if sharedTopDir != top {
				allShareTopDir = false
				break
			}
		}

		newSavePath = commonDir
		if allShareTopDir {
			parentDir := NormalizeSlashPath(path.Dir(commonDir))
			if parentDir != "" && parentDir != "." && parentDir != commonDir && isValidSavePath(parentDir) {
				newSavePath = parentDir
			}
		}

		seenNewRels := make(map[string]struct{}, len(files))
		for i, nr := range targetRemotes {
			rel, ok := stripBaseSavePath(newSavePath, nr)
			if !ok || rel == "" {
				return RelocateTorrentOutcome{}, fmt.Errorf("cannot compute relative path for %q under %q", nr, newSavePath)
			}
			relKey := strings.ToLower(rel)
			if _, dup := seenNewRels[relKey]; dup {
				return RelocateTorrentOutcome{}, fmt.Errorf("target path collision inside torrent: multiple files map to %q", rel)
			}
			seenNewRels[relKey] = struct{}{}
			newRels[i] = rel
		}
	}

	changes := make([]RelocatedFileChange, len(files))
	renamedCount := 0
	for i := range files {
		changed := oldRels[i] != newRels[i]
		if changed {
			renamedCount++
		}
		changes[i] = RelocatedFileChange{
			OldRelPath: oldRels[i],
			NewRelPath: newRels[i],
			Renamed:    changed,
		}
	}

	outcome := RelocateTorrentOutcome{
		InstanceID:   inst.ID,
		InstanceName: inst.Name,
		TorrentHash:  targetInfo.Hash,
		TorrentName:  targetInfo.Name,
		FileCount:    len(files),
		OldSavePath:  NormalizeSlashPath(targetInfo.SavePath),
		OldRelPath:   oldRels[0],
		NewSavePath:  NormalizeSlashPath(newSavePath),
		NewRelPath:   newRels[0],
		Renamed:      renamedCount > 0,
		RenamedCount: renamedCount,
		FileChanges:  changes,
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
	if err := postFormRetry409(ctx, hc, baseURL+"/api/v2/torrents/setLocation", locForm, "qBittorrent setLocation"); err != nil {
		return RelocateTorrentOutcome{}, err
	}

	// Collision-safe multi-file rename: park any current path that occupies another file's
	// target path into a unique temp path first, then rename everything into final targets.
	currentRels := append([]string(nil), oldRels...)
	activeLower := make(map[string]int, len(files))
	targetLower := make(map[string]int, len(files))
	for i := range files {
		activeLower[strings.ToLower(currentRels[i])] = i
		targetLower[strings.ToLower(newRels[i])] = i
	}

	for i := range files {
		if currentRels[i] == newRels[i] {
			continue
		}
		if ownerIdx, collided := targetLower[strings.ToLower(currentRels[i])]; collided && ownerIdx != i {
			suffix := 0
			var tmpRel string
			for {
				candidate := fmt.Sprintf(".seedlink_reloc_tmp_%d_%d/%s", i, suffix, path.Base(newRels[i]))
				candLower := strings.ToLower(candidate)
				_, inActive := activeLower[candLower]
				_, inTarget := targetLower[candLower]
				if !inActive && !inTarget {
					tmpRel = candidate
					break
				}
				suffix++
			}
			renForm := url.Values{}
			renForm.Set("hash", targetInfo.Hash)
			renForm.Set("oldPath", currentRels[i])
			renForm.Set("newPath", tmpRel)
			if err := postFormRetry409(ctx, hc, baseURL+"/api/v2/torrents/renameFile", renForm, "qBittorrent renameFile (stage)"); err != nil {
				return RelocateTorrentOutcome{}, err
			}
			delete(activeLower, strings.ToLower(currentRels[i]))
			currentRels[i] = tmpRel
			activeLower[strings.ToLower(tmpRel)] = i
		}
	}

	for i := range files {
		if currentRels[i] == newRels[i] {
			continue
		}
		renForm := url.Values{}
		renForm.Set("hash", targetInfo.Hash)
		renForm.Set("oldPath", currentRels[i])
		renForm.Set("newPath", newRels[i])
		if err := postFormRetry409(ctx, hc, baseURL+"/api/v2/torrents/renameFile", renForm, "qBittorrent renameFile"); err != nil {
			return RelocateTorrentOutcome{}, err
		}
		delete(activeLower, strings.ToLower(currentRels[i]))
		currentRels[i] = newRels[i]
		activeLower[strings.ToLower(newRels[i])] = i
	}

	if err := postFormRetry409(ctx, hc, baseURL+"/api/v2/torrents/recheck", hashForm, "qBittorrent recheck"); err != nil {
		return RelocateTorrentOutcome{}, err
	}

	if err := postFormWithFallback(ctx, hc, baseURL+"/api/v2/torrents/start", baseURL+"/api/v2/torrents/resume", hashForm, "qBittorrent start"); err != nil {
		return RelocateTorrentOutcome{}, err
	}

	return outcome, nil
}

// RelocateSingleFileTorrent moves and (if needed) renames a single-file torrent in qBittorrent
// so it can continue seeding from the new path created by Radarr/Sonarr Move imports.
func (c *Client) RelocateSingleFileTorrent(ctx context.Context, inst config.QbitInstanceConfig, hash, newRemotePath string, dryRun bool) (RelocateTorrentOutcome, error) {
	newRemote := NormalizeSlashPath(newRemotePath)
	if newRemote == "" {
		return RelocateTorrentOutcome{}, errors.New("missing target remote path")
	}
	details, err := c.FetchTorrentDetails(ctx, inst, hash)
	if err != nil {
		return RelocateTorrentOutcome{}, err
	}
	if len(details.Files) != 1 {
		return RelocateTorrentOutcome{}, fmt.Errorf("skipping multi-file torrent (%d files); only single-file torrents can be auto-relocated", len(details.Files))
	}
	targets := map[string]string{
		details.Files[0].Name: newRemote,
	}
	return c.RelocateTorrent(ctx, inst, details.Hash, targets, dryRun)
}
