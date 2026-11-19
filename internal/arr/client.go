package arr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"seedlink-auditor/internal/config"
	"seedlink-auditor/internal/qbit"
)

var seasonEpisodeRe = regexp.MustCompile(`(?i)\bS(\d{1,2})E(\d{1,3})\b`)

type TestResult struct {
	OK        bool   `json:"ok"`
	AppName   string `json:"app_name,omitempty"`
	Version   string `json:"version,omitempty"`
	ItemCount int    `json:"item_count,omitempty"`
	Error     string `json:"error,omitempty"`
}

type InstanceReport struct {
	InstanceID   string  `json:"instance_id"`
	InstanceName string  `json:"instance_name"`
	AppType      string  `json:"app_type"`
	URL          string  `json:"url"`
	OK           bool    `json:"ok"`
	ItemCount    int     `json:"item_count"`
	FileCount    int     `json:"file_count"`
	QueueCount   int     `json:"queue_count"`
	Error        *string `json:"error"`
}

type ArrFileEntry struct {
	InstanceID   string `json:"instance_id"`
	InstanceName string `json:"instance_name"`
	AppType      string `json:"app_type"`
	ItemID       int    `json:"item_id"`
	Title        string `json:"title"`
	Year         int    `json:"year,omitempty"`
	SeasonNumber int    `json:"season_number,omitempty"`
	Monitored    bool   `json:"monitored"`
	RemotePath   string `json:"remote_path"`
	MappedPath   string `json:"mapped_path"`
	Filename     string `json:"-"`
	Size         int64  `json:"size"`
}

type ArrFileMatch struct {
	Entry     ArrFileEntry
	MatchType string // "path" or "name_size"
}

type ArrQueueEntry struct {
	InstanceID            string `json:"instance_id"`
	InstanceName          string `json:"instance_name"`
	AppType               string `json:"app_type"`
	QueueID               int    `json:"queue_id"`
	ItemID                int    `json:"item_id"`
	EpisodeID             int    `json:"episode_id,omitempty"`
	Title                 string `json:"title"`
	Status                string `json:"status"`
	TrackedDownloadStatus string `json:"tracked_download_status"`
	TrackedDownloadState  string `json:"tracked_download_state"`
	DownloadID            string `json:"download_id"`
	ErrorMessage          string `json:"error_message"`
	StatusMessage         string `json:"status_message"`
	IsBlocked             bool   `json:"is_blocked"`
}

type ArrLibraryEntry struct {
	InstanceID   string `json:"instance_id"`
	InstanceName string `json:"instance_name"`
	AppType      string `json:"app_type"`
	ItemID       int    `json:"item_id"`
	Title        string `json:"title"`
	Year         int    `json:"year,omitempty"`
	Monitored    bool   `json:"monitored"`
	HasFile      bool   `json:"has_file"`
	RemotePath   string `json:"remote_path"`
	MappedPath   string `json:"mapped_path"`
}

type ArrTitleMatch struct {
	InstanceID    string `json:"instance_id"`
	InstanceName  string `json:"instance_name"`
	AppType       string `json:"app_type"`
	ItemID        int    `json:"item_id"`
	EpisodeIDs    []int  `json:"episode_ids,omitempty"`
	Title         string `json:"title"`
	Year          int    `json:"year,omitempty"`
	SeasonNumber  int    `json:"season_number,omitempty"`
	EpisodeNumber int    `json:"episode_number,omitempty"`
	Monitored     bool   `json:"monitored"`
	HasFile       bool   `json:"has_file"`
	RootPath      string `json:"root_path"`
}

type radarrTitleItem struct {
	InstanceID   string
	InstanceName string
	ItemID       int
	Title        string
	NormTitles   []string
	Year         int
	Monitored    bool
	HasFile      bool
	RootPath     string
}

type sonarrSeriesItem struct {
	InstanceID   string
	InstanceName string
	SeriesID     int
	Title        string
	NormTitle    string
	Year         int
	Monitored    bool
	RootPath     string
}

type wantedEpisodeKey struct {
	InstanceID string
	SeriesID   int
	Season     int
	Episode    int
}

type wantedEpisodeInfo struct {
	EpisodeID int
	Title     string
	Monitored bool
}

type nameSizeKey struct {
	NameLower string
	Size      int64
}

type Index struct {
	ByPath         map[string][]ArrFileEntry
	ByPathCI       map[string][]ArrFileEntry
	ByNameSize     map[nameSizeKey][]ArrFileEntry
	QueueByHash    map[string][]ArrQueueEntry
	LibraryRoots   []ArrLibraryEntry
	RadarrTitles   []radarrTitleItem
	SonarrSeries   []sonarrSeriesItem
	WantedEpisodes map[wantedEpisodeKey]wantedEpisodeInfo
	Instances      []InstanceReport
}

func NewIndex() *Index {
	return &Index{
		ByPath:         make(map[string][]ArrFileEntry),
		ByPathCI:       make(map[string][]ArrFileEntry),
		ByNameSize:     make(map[nameSizeKey][]ArrFileEntry),
		QueueByHash:    make(map[string][]ArrQueueEntry),
		LibraryRoots:   make([]ArrLibraryEntry, 0),
		RadarrTitles:   make([]radarrTitleItem, 0),
		SonarrSeries:   make([]sonarrSeriesItem, 0),
		WantedEpisodes: make(map[wantedEpisodeKey]wantedEpisodeInfo),
		Instances:      make([]InstanceReport, 0),
	}
}

func (idx *Index) addFileEntry(e ArrFileEntry) {
	norm := qbit.NormalizeSlashPath(e.MappedPath)
	idx.ByPath[norm] = append(idx.ByPath[norm], e)
	idx.ByPathCI[strings.ToLower(norm)] = append(idx.ByPathCI[strings.ToLower(norm)], e)
	k := nameSizeKey{NameLower: strings.ToLower(e.Filename), Size: e.Size}
	idx.ByNameSize[k] = append(idx.ByNameSize[k], e)
}

func (idx *Index) FindFileMatches(absPath, filename string, size int64) []ArrFileMatch {
	if idx == nil {
		return nil
	}
	norm := qbit.NormalizeSlashPath(absPath)
	if list, ok := idx.ByPath[norm]; ok {
		out := make([]ArrFileMatch, 0, len(list))
		for _, e := range list {
			out = append(out, ArrFileMatch{Entry: e, MatchType: "path"})
		}
		return out
	}
	if list, ok := idx.ByPathCI[strings.ToLower(norm)]; ok {
		out := make([]ArrFileMatch, 0, len(list))
		for _, e := range list {
			out = append(out, ArrFileMatch{Entry: e, MatchType: "path"})
		}
		return out
	}
	k := nameSizeKey{NameLower: strings.ToLower(filename), Size: size}
	if list, ok := idx.ByNameSize[k]; ok {
		out := make([]ArrFileMatch, 0, len(list))
		for _, e := range list {
			out = append(out, ArrFileMatch{Entry: e, MatchType: "name_size"})
		}
		return out
	}
	return nil
}

func (idx *Index) FindQueueMatches(hashes []string) []ArrQueueEntry {
	if idx == nil || len(hashes) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	out := make([]ArrQueueEntry, 0)
	for _, h := range hashes {
		lh := strings.ToLower(strings.TrimSpace(h))
		if lh == "" {
			continue
		}
		for _, q := range idx.QueueByHash[lh] {
			key := fmt.Sprintf("%s:%d", q.InstanceID, q.QueueID)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, q)
		}
	}
	return out
}

func (idx *Index) FindLibraryPathOwner(absPath string) []ArrLibraryEntry {
	if idx == nil || len(idx.LibraryRoots) == 0 {
		return nil
	}
	norm := qbit.NormalizeSlashPath(absPath)
	normLower := strings.ToLower(norm)
	out := make([]ArrLibraryEntry, 0)
	for _, lib := range idx.LibraryRoots {
		root := qbit.NormalizeSlashPath(lib.MappedPath)
		if root == "" {
			continue
		}
		if strings.HasPrefix(norm, root+"/") || (runtime.GOOS == "windows" && strings.HasPrefix(normLower, strings.ToLower(root)+"/")) {
			out = append(out, lib)
		}
	}
	return out
}

func normalizeTitle(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastSpace := true
	for _, r := range strings.ToLower(s) {
		if r == '\'' || r == '’' {
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastSpace = false
		} else if !lastSpace {
			b.WriteByte(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

func containsWordToken(haystack, token string) bool {
	if token == "" {
		return false
	}
	padded := " " + haystack + " "
	return strings.Contains(padded, " "+token+" ")
}

func stripExt(filename string) string {
	ext := path.Ext(filename)
	if ext != "" && len(ext) <= 6 {
		return strings.TrimSuffix(filename, ext)
	}
	return filename
}

func (idx *Index) FindTitleMatches(filename, torrentName string) []ArrTitleMatch {
	if idx == nil {
		return nil
	}
	candidates := make([]string, 0, 2)
	if f := strings.TrimSpace(stripExt(filename)); f != "" {
		candidates = append(candidates, f)
	}
	if t := strings.TrimSpace(torrentName); t != "" {
		candidates = append(candidates, t)
	}
	if len(candidates) == 0 {
		return nil
	}

	normCandidates := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if nc := normalizeTitle(c); nc != "" {
			normCandidates = append(normCandidates, nc)
		}
	}

	seen := make(map[string]struct{})
	out := make([]ArrTitleMatch, 0)

	// 1. Radarr title + year matching
	for _, m := range idx.RadarrTitles {
		if m.Year <= 0 {
			continue
		}
		yearStr := strconv.Itoa(m.Year)
		matched := false
		for _, nc := range normCandidates {
			if !containsWordToken(nc, yearStr) {
				continue
			}
			for _, nt := range m.NormTitles {
				if nt == "" {
					continue
				}
				if nc == nt || strings.HasPrefix(nc, nt+" ") {
					matched = true
					break
				}
			}
			if matched {
				break
			}
		}
		if matched {
			key := fmt.Sprintf("radarr:%s:%d", m.InstanceID, m.ItemID)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, ArrTitleMatch{
				InstanceID:   m.InstanceID,
				InstanceName: m.InstanceName,
				AppType:      "radarr",
				ItemID:       m.ItemID,
				Title:        m.Title,
				Year:         m.Year,
				Monitored:    m.Monitored,
				HasFile:      m.HasFile,
				RootPath:     m.RootPath,
			})
		}
	}

	// 2. Sonarr SxxExx + series title matching against wanted/missing episodes
	for _, raw := range candidates {
		spaced := strings.NewReplacer(".", " ", "_", " ", "-", " ", "[", " ", "]", " ", "(", " ", ")", " ").Replace(raw)
		loc := seasonEpisodeRe.FindStringSubmatchIndex(spaced)
		if loc == nil {
			continue
		}
		seasonNum, sErr := strconv.Atoi(spaced[loc[2]:loc[3]])
		epNum, eErr := strconv.Atoi(spaced[loc[4]:loc[5]])
		if sErr != nil || eErr != nil {
			continue
		}
		prefix := normalizeTitle(spaced[:loc[0]])
		if prefix == "" {
			continue
		}
		for _, sr := range idx.SonarrSeries {
			if sr.NormTitle == "" {
				continue
			}
			if prefix != sr.NormTitle && !strings.HasPrefix(prefix, sr.NormTitle+" ") {
				continue
			}
			wk := wantedEpisodeKey{
				InstanceID: sr.InstanceID,
				SeriesID:   sr.SeriesID,
				Season:     seasonNum,
				Episode:    epNum,
			}
			wantedEp, isWanted := idx.WantedEpisodes[wk]
			if !isWanted {
				continue
			}
			key := fmt.Sprintf("sonarr:%s:%d:%d", sr.InstanceID, sr.SeriesID, wantedEp.EpisodeID)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, ArrTitleMatch{
				InstanceID:    sr.InstanceID,
				InstanceName:  sr.InstanceName,
				AppType:       "sonarr",
				ItemID:        sr.SeriesID,
				EpisodeIDs:    []int{wantedEp.EpisodeID},
				Title:         sr.Title,
				Year:          sr.Year,
				SeasonNumber:  seasonNum,
				EpisodeNumber: epNum,
				Monitored:     sr.Monitored && wantedEp.Monitored,
				HasFile:       false,
				RootPath:      sr.RootPath,
			})
		}
	}

	return out
}

func ToRemotePath(localPath string, mappings []config.PathMapping) string {
	norm := qbit.NormalizeSlashPath(localPath)
	for _, m := range mappings {
		remote := qbit.NormalizeSlashPath(m.RemotePrefix)
		local := qbit.NormalizeSlashPath(m.LocalPrefix)
		if local == "" {
			continue
		}
		if norm == local {
			return remote
		}
		if strings.HasPrefix(norm, local+"/") {
			suffix := norm[len(local):]
			return qbit.NormalizeSlashPath(remote + suffix)
		}
		if runtime.GOOS == "windows" && len(norm) > len(local) &&
			strings.EqualFold(norm[:len(local)+1], local+"/") {
			suffix := norm[len(local):]
			return qbit.NormalizeSlashPath(remote + suffix)
		}
	}
	return norm
}

type systemStatusJSON struct {
	AppName string `json:"appName"`
	Version string `json:"version"`
}

type radarrMovieFileJSON struct {
	ID           int    `json:"id"`
	Path         string `json:"path"`
	RelativePath string `json:"relativePath"`
	Size         int64  `json:"size"`
}

type radarrMovieJSON struct {
	ID            int                  `json:"id"`
	Title         string               `json:"title"`
	OriginalTitle string               `json:"originalTitle"`
	Year          int                  `json:"year"`
	Path          string               `json:"path"`
	HasFile       bool                 `json:"hasFile"`
	Monitored     bool                 `json:"monitored"`
	MovieFile     *radarrMovieFileJSON `json:"movieFile"`
}

type sonarrSeriesJSON struct {
	ID        int    `json:"id"`
	Title     string `json:"title"`
	Year      int    `json:"year"`
	Path      string `json:"path"`
	Monitored bool   `json:"monitored"`
}

type sonarrEpisodeFileJSON struct {
	ID           int    `json:"id"`
	SeriesID     int    `json:"seriesId"`
	SeasonNumber int    `json:"seasonNumber"`
	Path         string `json:"path"`
	RelativePath string `json:"relativePath"`
	Size         int64  `json:"size"`
}

type sonarrWantedRecordJSON struct {
	ID            int               `json:"id"`
	SeriesID      int               `json:"seriesId"`
	SeasonNumber  int               `json:"seasonNumber"`
	EpisodeNumber int               `json:"episodeNumber"`
	Title         string            `json:"title"`
	Monitored     bool              `json:"monitored"`
	HasFile       bool              `json:"hasFile"`
	Series        *sonarrSeriesJSON `json:"series"`
}

type sonarrWantedPageJSON struct {
	Page         int                      `json:"page"`
	PageSize     int                      `json:"pageSize"`
	TotalRecords int                      `json:"totalRecords"`
	Records      []sonarrWantedRecordJSON `json:"records"`
}

type arrQueueStatusMsgJSON struct {
	Title    string   `json:"title"`
	Messages []string `json:"messages"`
}

type arrQueueRecordJSON struct {
	ID                    int                     `json:"id"`
	MovieID               int                     `json:"movieId"`
	SeriesID              int                     `json:"seriesId"`
	EpisodeID             int                     `json:"episodeId"`
	Title                 string                  `json:"title"`
	Status                string                  `json:"status"`
	TrackedDownloadStatus string                  `json:"trackedDownloadStatus"`
	TrackedDownloadState  string                  `json:"trackedDownloadState"`
	DownloadID            string                  `json:"downloadId"`
	ErrorMessage          string                  `json:"errorMessage"`
	StatusMessages        []arrQueueStatusMsgJSON `json:"statusMessages"`
}

type arrQueuePageJSON struct {
	Page         int                  `json:"page"`
	PageSize     int                  `json:"pageSize"`
	TotalRecords int                  `json:"totalRecords"`
	Records      []arrQueueRecordJSON `json:"records"`
}

type ManualImportRejection struct {
	Reason string `json:"reason"`
	Type   string `json:"type"`
}

type ManualImportMovie struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	Year  int    `json:"year"`
}

type ManualImportSeries struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
}

type ManualImportEpisode struct {
	ID            int    `json:"id"`
	SeasonNumber  int    `json:"seasonNumber"`
	EpisodeNumber int    `json:"episodeNumber"`
	Title         string `json:"title"`
}

type ManualImportItem struct {
	ID           int                     `json:"id"`
	Path         string                  `json:"path"`
	RelativePath string                  `json:"relativePath"`
	Name         string                  `json:"name"`
	Size         int64                   `json:"size"`
	Movie        *ManualImportMovie      `json:"movie,omitempty"`
	Series       *ManualImportSeries     `json:"series,omitempty"`
	Episodes     []ManualImportEpisode   `json:"episodes,omitempty"`
	Quality      json.RawMessage         `json:"quality,omitempty"`
	Languages    json.RawMessage         `json:"languages,omitempty"`
	ReleaseGroup string                  `json:"releaseGroup,omitempty"`
	IndexerFlags int                     `json:"indexerFlags,omitempty"`
	DownloadID   string                  `json:"downloadId,omitempty"`
	Rejections   []ManualImportRejection `json:"rejections"`
}

type ManualImportFilePayload struct {
	Path         string          `json:"path"`
	FolderName   string          `json:"folderName,omitempty"`
	MovieID      int             `json:"movieId,omitempty"`
	SeriesID     int             `json:"seriesId,omitempty"`
	EpisodeIDs   []int           `json:"episodeIds,omitempty"`
	Quality      json.RawMessage `json:"quality,omitempty"`
	Languages    json.RawMessage `json:"languages,omitempty"`
	ReleaseGroup string          `json:"releaseGroup,omitempty"`
	IndexerFlags int             `json:"indexerFlags,omitempty"`
	DownloadID   string          `json:"downloadId,omitempty"`
}

type CommandResponse struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type Client struct {
	httpClient *http.Client
}

func NewClient(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &Client{
		httpClient: &http.Client{Timeout: timeout},
	}
}

func (c *Client) doJSON(ctx context.Context, method, fullURL, apiKey string, bodyPayload any, dest any) error {
	var bodyReader io.Reader
	if bodyPayload != nil {
		raw, err := json.Marshal(bodyPayload)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", apiKey)
	if bodyPayload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("Arr API rejected credentials (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, fullURL, strings.TrimSpace(string(b)))
	}
	if dest != nil {
		return json.NewDecoder(resp.Body).Decode(dest)
	}
	return nil
}

func (c *Client) TestConnection(ctx context.Context, inst config.ArrInstanceConfig) TestResult {
	baseURL := strings.TrimRight(strings.TrimSpace(inst.URL), "/")
	if baseURL == "" {
		return TestResult{OK: false, Error: "empty Radarr/Sonarr URL"}
	}
	appType := strings.ToLower(strings.TrimSpace(inst.Type))
	if appType != "radarr" && appType != "sonarr" {
		return TestResult{OK: false, Error: "instance type must be 'radarr' or 'sonarr'"}
	}

	var status systemStatusJSON
	if err := c.doJSON(ctx, http.MethodGet, baseURL+"/api/v3/system/status", inst.APIKey, nil, &status); err != nil {
		return TestResult{OK: false, Error: err.Error()}
	}

	var itemCount int
	if appType == "radarr" {
		var movies []radarrMovieJSON
		if err := c.doJSON(ctx, http.MethodGet, baseURL+"/api/v3/movie", inst.APIKey, nil, &movies); err != nil {
			return TestResult{OK: false, Error: err.Error()}
		}
		itemCount = len(movies)
	} else {
		var series []sonarrSeriesJSON
		if err := c.doJSON(ctx, http.MethodGet, baseURL+"/api/v3/series", inst.APIKey, nil, &series); err != nil {
			return TestResult{OK: false, Error: err.Error()}
		}
		itemCount = len(series)
	}

	appName := status.AppName
	if appName == "" {
		if appType == "radarr" {
			appName = "Radarr"
		} else {
			appName = "Sonarr"
		}
	}
	return TestResult{
		OK:        true,
		AppName:   appName,
		Version:   status.Version,
		ItemCount: itemCount,
	}
}

type fetchedInstanceData struct {
	report         InstanceReport
	files          []ArrFileEntry
	queue          []ArrQueueEntry
	libraryRoots   []ArrLibraryEntry
	radarrTitles   []radarrTitleItem
	sonarrSeries   []sonarrSeriesItem
	wantedEpisodes map[wantedEpisodeKey]wantedEpisodeInfo
}

func summarizeQueueRecord(inst config.ArrInstanceConfig, rec arrQueueRecordJSON) ArrQueueEntry {
	itemID := rec.MovieID
	if inst.Type == "sonarr" && rec.SeriesID > 0 {
		itemID = rec.SeriesID
	}

	msgs := make([]string, 0)
	if strings.TrimSpace(rec.ErrorMessage) != "" {
		msgs = append(msgs, strings.TrimSpace(rec.ErrorMessage))
	}
	for _, sm := range rec.StatusMessages {
		for _, m := range sm.Messages {
			if tm := strings.TrimSpace(m); tm != "" {
				msgs = append(msgs, tm)
			}
		}
		if len(sm.Messages) == 0 && strings.TrimSpace(sm.Title) != "" {
			msgs = append(msgs, strings.TrimSpace(sm.Title))
		}
	}
	statusMsg := strings.Join(msgs, "; ")

	tds := strings.ToLower(rec.TrackedDownloadStatus)
	tdState := strings.ToLower(rec.TrackedDownloadState)
	isBlocked := tds == "warning" || tds == "error" ||
		tdState == "importblocked" || tdState == "importfailed" || tdState == "importpending"

	return ArrQueueEntry{
		InstanceID:            inst.ID,
		InstanceName:          inst.Name,
		AppType:               inst.Type,
		QueueID:               rec.ID,
		ItemID:                itemID,
		EpisodeID:             rec.EpisodeID,
		Title:                 rec.Title,
		Status:                rec.Status,
		TrackedDownloadStatus: rec.TrackedDownloadStatus,
		TrackedDownloadState:  rec.TrackedDownloadState,
		DownloadID:            strings.ToLower(strings.TrimSpace(rec.DownloadID)),
		ErrorMessage:          rec.ErrorMessage,
		StatusMessage:         statusMsg,
		IsBlocked:             isBlocked,
	}
}

func (c *Client) fetchInstanceData(ctx context.Context, inst config.ArrInstanceConfig) fetchedInstanceData {
	if !inst.Enabled {
		disabledMsg := "Disabled"
		return fetchedInstanceData{
			report: InstanceReport{
				InstanceID:   inst.ID,
				InstanceName: inst.Name,
				AppType:      inst.Type,
				URL:          inst.URL,
				OK:           true,
				Error:        &disabledMsg,
			},
		}
	}

	baseURL := strings.TrimRight(strings.TrimSpace(inst.URL), "/")
	if baseURL == "" {
		msg := "empty URL"
		return fetchedInstanceData{
			report: InstanceReport{
				InstanceID:   inst.ID,
				InstanceName: inst.Name,
				AppType:      inst.Type,
				URL:          inst.URL,
				OK:           false,
				Error:        &msg,
			},
		}
	}

	out := fetchedInstanceData{
		wantedEpisodes: make(map[wantedEpisodeKey]wantedEpisodeInfo),
	}

	if inst.Type == "radarr" {
		var movies []radarrMovieJSON
		if err := c.doJSON(ctx, http.MethodGet, baseURL+"/api/v3/movie", inst.APIKey, nil, &movies); err != nil {
			msg := err.Error()
			out.report = InstanceReport{
				InstanceID: inst.ID, InstanceName: inst.Name, AppType: inst.Type, URL: inst.URL, OK: false, Error: &msg,
			}
			return out
		}

		for _, m := range movies {
			mappedRoot := qbit.ApplyPathMappings(m.Path, inst.PathMappings)
			out.libraryRoots = append(out.libraryRoots, ArrLibraryEntry{
				InstanceID:   inst.ID,
				InstanceName: inst.Name,
				AppType:      "radarr",
				ItemID:       m.ID,
				Title:        m.Title,
				Year:         m.Year,
				Monitored:    m.Monitored,
				HasFile:      m.HasFile,
				RemotePath:   qbit.NormalizeSlashPath(m.Path),
				MappedPath:   mappedRoot,
			})

			normTitles := make([]string, 0, 2)
			if nt := normalizeTitle(m.Title); nt != "" {
				normTitles = append(normTitles, nt)
			}
			if ot := normalizeTitle(m.OriginalTitle); ot != "" && (len(normTitles) == 0 || ot != normTitles[0]) {
				normTitles = append(normTitles, ot)
			}
			out.radarrTitles = append(out.radarrTitles, radarrTitleItem{
				InstanceID:   inst.ID,
				InstanceName: inst.Name,
				ItemID:       m.ID,
				Title:        m.Title,
				NormTitles:   normTitles,
				Year:         m.Year,
				Monitored:    m.Monitored,
				HasFile:      m.HasFile,
				RootPath:     mappedRoot,
			})

			if m.MovieFile != nil {
				rawPath := m.MovieFile.Path
				if rawPath == "" && m.Path != "" && m.MovieFile.RelativePath != "" {
					rawPath = qbit.NormalizeSlashPath(m.Path) + "/" + strings.TrimLeft(qbit.NormalizeSlashPath(m.MovieFile.RelativePath), "/")
				}
				if rawPath != "" {
					mapped := qbit.ApplyPathMappings(rawPath, inst.PathMappings)
					out.files = append(out.files, ArrFileEntry{
						InstanceID:   inst.ID,
						InstanceName: inst.Name,
						AppType:      "radarr",
						ItemID:       m.ID,
						Title:        m.Title,
						Year:         m.Year,
						Monitored:    m.Monitored,
						RemotePath:   qbit.NormalizeSlashPath(rawPath),
						MappedPath:   mapped,
						Filename:     path.Base(mapped),
						Size:         m.MovieFile.Size,
					})
				}
			}
		}

		var qPage arrQueuePageJSON
		if qErr := c.doJSON(ctx, http.MethodGet, baseURL+"/api/v3/queue?page=1&pageSize=1000&includeUnknownMovieItems=true", inst.APIKey, nil, &qPage); qErr == nil {
			for _, rec := range qPage.Records {
				if strings.TrimSpace(rec.DownloadID) != "" {
					out.queue = append(out.queue, summarizeQueueRecord(inst, rec))
				}
			}
		}

		out.report = InstanceReport{
			InstanceID:   inst.ID,
			InstanceName: inst.Name,
			AppType:      inst.Type,
			URL:          inst.URL,
			OK:           true,
			ItemCount:    len(movies),
			FileCount:    len(out.files),
			QueueCount:   len(out.queue),
		}
		return out
	}

	// Sonarr
	var series []sonarrSeriesJSON
	if err := c.doJSON(ctx, http.MethodGet, baseURL+"/api/v3/series", inst.APIKey, nil, &series); err != nil {
		msg := err.Error()
		out.report = InstanceReport{
			InstanceID: inst.ID, InstanceName: inst.Name, AppType: inst.Type, URL: inst.URL, OK: false, Error: &msg,
		}
		return out
	}

	seriesByID := make(map[int]sonarrSeriesJSON, len(series))
	for _, s := range series {
		seriesByID[s.ID] = s
		mappedRoot := qbit.ApplyPathMappings(s.Path, inst.PathMappings)
		out.libraryRoots = append(out.libraryRoots, ArrLibraryEntry{
			InstanceID:   inst.ID,
			InstanceName: inst.Name,
			AppType:      "sonarr",
			ItemID:       s.ID,
			Title:        s.Title,
			Year:         s.Year,
			Monitored:    s.Monitored,
			HasFile:      false,
			RemotePath:   qbit.NormalizeSlashPath(s.Path),
			MappedPath:   mappedRoot,
		})
		out.sonarrSeries = append(out.sonarrSeries, sonarrSeriesItem{
			InstanceID:   inst.ID,
			InstanceName: inst.Name,
			SeriesID:     s.ID,
			Title:        s.Title,
			NormTitle:    normalizeTitle(s.Title),
			Year:         s.Year,
			Monitored:    s.Monitored,
			RootPath:     mappedRoot,
		})
	}

	// Fetch episode files concurrently across series (max 10 workers)
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var epErrs []string

	for _, s := range series {
		wg.Add(1)
		go func(sr sonarrSeriesJSON) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			var epFiles []sonarrEpisodeFileJSON
			epURL := fmt.Sprintf("%s/api/v3/episodefile?seriesId=%d", baseURL, sr.ID)
			if err := c.doJSON(ctx, http.MethodGet, epURL, inst.APIKey, nil, &epFiles); err != nil {
				mu.Lock()
				if len(epErrs) < 3 {
					epErrs = append(epErrs, err.Error())
				}
				mu.Unlock()
				return
			}
			localEntries := make([]ArrFileEntry, 0, len(epFiles))
			for _, ef := range epFiles {
				rawPath := ef.Path
				if rawPath == "" && sr.Path != "" && ef.RelativePath != "" {
					rawPath = qbit.NormalizeSlashPath(sr.Path) + "/" + strings.TrimLeft(qbit.NormalizeSlashPath(ef.RelativePath), "/")
				}
				if rawPath == "" {
					continue
				}
				mapped := qbit.ApplyPathMappings(rawPath, inst.PathMappings)
				localEntries = append(localEntries, ArrFileEntry{
					InstanceID:   inst.ID,
					InstanceName: inst.Name,
					AppType:      "sonarr",
					ItemID:       sr.ID,
					Title:        sr.Title,
					Year:         sr.Year,
					SeasonNumber: ef.SeasonNumber,
					Monitored:    sr.Monitored,
					RemotePath:   qbit.NormalizeSlashPath(rawPath),
					MappedPath:   mapped,
					Filename:     path.Base(mapped),
					Size:         ef.Size,
				})
			}
			mu.Lock()
			out.files = append(out.files, localEntries...)
			mu.Unlock()
		}(s)
	}
	wg.Wait()

	// Fetch wanted/missing episodes (up to 5 pages of 1000)
	for page := 1; page <= 5; page++ {
		var wPage sonarrWantedPageJSON
		wURL := fmt.Sprintf("%s/api/v3/wanted/missing?page=%d&pageSize=1000&includeSeries=true", baseURL, page)
		if err := c.doJSON(ctx, http.MethodGet, wURL, inst.APIKey, nil, &wPage); err != nil {
			break
		}
		for _, rec := range wPage.Records {
			wk := wantedEpisodeKey{
				InstanceID: inst.ID,
				SeriesID:   rec.SeriesID,
				Season:     rec.SeasonNumber,
				Episode:    rec.EpisodeNumber,
			}
			out.wantedEpisodes[wk] = wantedEpisodeInfo{
				EpisodeID: rec.ID,
				Title:     rec.Title,
				Monitored: rec.Monitored,
			}
		}
		if len(wPage.Records) == 0 || page*1000 >= wPage.TotalRecords {
			break
		}
	}

	var qPage arrQueuePageJSON
	if qErr := c.doJSON(ctx, http.MethodGet, baseURL+"/api/v3/queue?page=1&pageSize=1000&includeUnknownSeriesItems=true", inst.APIKey, nil, &qPage); qErr == nil {
		for _, rec := range qPage.Records {
			if strings.TrimSpace(rec.DownloadID) != "" {
				out.queue = append(out.queue, summarizeQueueRecord(inst, rec))
			}
		}
	}

	var errPtr *string
	if len(epErrs) > 0 {
		joined := strings.Join(epErrs, "; ")
		errPtr = &joined
	}

	out.report = InstanceReport{
		InstanceID:   inst.ID,
		InstanceName: inst.Name,
		AppType:      inst.Type,
		URL:          inst.URL,
		OK:           len(epErrs) == 0,
		ItemCount:    len(series),
		FileCount:    len(out.files),
		QueueCount:   len(out.queue),
		Error:        errPtr,
	}
	return out
}

func (c *Client) BuildIndex(ctx context.Context, instances []config.ArrInstanceConfig) *Index {
	idx := NewIndex()
	if len(instances) == 0 {
		return idx
	}

	results := make([]fetchedInstanceData, len(instances))
	var wg sync.WaitGroup
	for i, inst := range instances {
		wg.Add(1)
		go func(pos int, cfg config.ArrInstanceConfig) {
			defer wg.Done()
			results[pos] = c.fetchInstanceData(ctx, cfg)
		}(i, inst)
	}
	wg.Wait()

	for _, r := range results {
		idx.Instances = append(idx.Instances, r.report)
		for _, f := range r.files {
			idx.addFileEntry(f)
		}
		for _, q := range r.queue {
			idx.QueueByHash[q.DownloadID] = append(idx.QueueByHash[q.DownloadID], q)
		}
		idx.LibraryRoots = append(idx.LibraryRoots, r.libraryRoots...)
		idx.RadarrTitles = append(idx.RadarrTitles, r.radarrTitles...)
		idx.SonarrSeries = append(idx.SonarrSeries, r.sonarrSeries...)
		for k, v := range r.wantedEpisodes {
			idx.WantedEpisodes[k] = v
		}
	}
	return idx
}

func (c *Client) PreviewManualImport(ctx context.Context, inst config.ArrInstanceConfig, remoteFolder string) ([]ManualImportItem, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(inst.URL), "/")
	if baseURL == "" {
		return nil, errors.New("empty Radarr/Sonarr URL")
	}
	fullURL := fmt.Sprintf("%s/api/v3/manualimport?folder=%s&filterExistingFiles=false", baseURL, url.QueryEscape(remoteFolder))
	var items []ManualImportItem
	if err := c.doJSON(ctx, http.MethodGet, fullURL, inst.APIKey, nil, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func (c *Client) TriggerDownloadScan(ctx context.Context, inst config.ArrInstanceConfig, remotePath, downloadClientID string) (CommandResponse, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(inst.URL), "/")
	cmdName := "DownloadedMoviesScan"
	if strings.ToLower(inst.Type) == "sonarr" {
		cmdName = "DownloadedEpisodesScan"
	}
	payload := map[string]any{
		"name":       cmdName,
		"path":       remotePath,
		"importMode": "Auto",
	}
	if strings.TrimSpace(downloadClientID) != "" {
		payload["downloadClientId"] = strings.ToUpper(strings.TrimSpace(downloadClientID))
	}
	var res CommandResponse
	err := c.doJSON(ctx, http.MethodPost, baseURL+"/api/v3/command", inst.APIKey, payload, &res)
	return res, err
}

func (c *Client) TriggerManualImport(ctx context.Context, inst config.ArrInstanceConfig, files []ManualImportFilePayload) (CommandResponse, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(inst.URL), "/")
	payload := map[string]any{
		"name":       "ManualImport",
		"importMode": "auto",
		"files":      files,
	}
	var res CommandResponse
	err := c.doJSON(ctx, http.MethodPost, baseURL+"/api/v3/command", inst.APIKey, payload, &res)
	return res, err
}

func (c *Client) TriggerRescan(ctx context.Context, inst config.ArrInstanceConfig, itemID int) (CommandResponse, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(inst.URL), "/")
	var payload map[string]any
	if strings.ToLower(inst.Type) == "sonarr" {
		payload = map[string]any{
			"name":     "RescanSeries",
			"seriesId": itemID,
		}
	} else {
		payload = map[string]any{
			"name":     "RefreshMovie",
			"movieIds": []int{itemID},
		}
	}
	var res CommandResponse
	err := c.doJSON(ctx, http.MethodPost, baseURL+"/api/v3/command", inst.APIKey, payload, &res)
	return res, err
}
