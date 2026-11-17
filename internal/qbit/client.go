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
	SavePath      string  `json:"save_path"`
	RelativeName  string  `json:"relative_name"`
	MappedPath    string  `json:"mapped_path"`
	Filename      string  `json:"-"`
	Size          int64   `json:"size"`
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

func ClassifyTorrentState(state string) (bool, string) {
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

type torrentInfoJSON struct {
	Hash     string  `json:"hash"`
	Name     string  `json:"name"`
	SavePath string  `json:"save_path"`
	State    string  `json:"state"`
	Ratio    float64 `json:"ratio"`
	Tracker  string  `json:"tracker"`
}

type torrentFileJSON struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type Client struct {
	timeout time.Duration
}

func NewClient(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &Client{timeout: timeout}
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
		Timeout: c.timeout,
		Jar:     jar,
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
	if err := doGetJSON(ctx, hc, baseURL+"/api/v2/torrents/info", &torrents); err != nil {
		msg := err.Error()
		return InstanceReport{
			InstanceID:   inst.ID,
			InstanceName: inst.Name,
			URL:          inst.URL,
			OK:           false,
			Error:        &msg,
		}, nil
	}

	sem := make(chan struct{}, 10)
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

			built := buildEntriesForTorrent(inst, torrent, files)
			mu.Lock()
			entries = append(entries, built...)
			mu.Unlock()
		}(t)
	}
	wg.Wait()

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

func buildEntriesForTorrent(inst config.QbitInstanceConfig, t torrentInfoJSON, files []torrentFileJSON) []TorrentFileEntry {
	isSeeding, seedingStatus := ClassifyTorrentState(t.State)
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
			SavePath:      savePath,
			RelativeName:  relName,
			MappedPath:    mapped,
			Filename:      filename,
			Size:          f.Size,
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

func (c *Client) DeleteTorrents(ctx context.Context, inst config.QbitInstanceConfig, hashes []string, deleteFiles bool) ([]string, error) {
	seen := make(map[string]struct{}, len(hashes))
	unique := make([]string, 0, len(hashes))
	for _, h := range hashes {
		trimmed := strings.TrimSpace(h)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		unique = append(unique, trimmed)
	}
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v2/torrents/delete", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("qBittorrent delete failed with HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return unique, nil
}
