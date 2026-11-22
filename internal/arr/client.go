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
	InstanceID   string   `json:"instance_id"`
	InstanceName string   `json:"instance_name"`
	AppType      string   `json:"app_type"`
	ItemID       int      `json:"item_id"`
	Title        string   `json:"title"`
	Year         int      `json:"year,omitempty"`
	SeasonNumber int      `json:"season_number,omitempty"`
	Monitored    bool     `json:"monitored"`
	RemotePath   string   `json:"remote_path"`
	MappedPath   string   `json:"mapped_path"`
	Filename     string   `json:"-"`
	Size         int64    `json:"size"`
	Genres       []string `json:"genres,omitempty"`
	HasPoster    bool     `json:"has_poster,omitempty"`
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
	InstanceID   string   `json:"instance_id"`
	InstanceName string   `json:"instance_name"`
	AppType      string   `json:"app_type"`
	ItemID       int      `json:"item_id"`
	Title        string   `json:"title"`
	Year         int      `json:"year,omitempty"`
	Monitored    bool     `json:"monitored"`
	HasFile      bool     `json:"has_file"`
	RemotePath   string   `json:"remote_path"`
	MappedPath   string   `json:"mapped_path"`
	Genres       []string `json:"genres,omitempty"`
	HasPoster    bool     `json:"has_poster,omitempty"`
}

type ArrTitleMatch struct {
	InstanceID       string   `json:"instance_id"`
	InstanceName     string   `json:"instance_name"`
	AppType          string   `json:"app_type"`
	ItemID           int      `json:"item_id"`
	EpisodeIDs       []int    `json:"episode_ids,omitempty"`
	Title            string   `json:"title"`
	Year             int      `json:"year,omitempty"`
	SeasonNumber     int      `json:"season_number,omitempty"`
	EpisodeNumber    int      `json:"episode_number,omitempty"`
	Monitored        bool     `json:"monitored"`
	HasFile          bool     `json:"has_file"`
	RootPath         string   `json:"root_path"`
	ExistingFileName string   `json:"existing_file_name,omitempty"`
	ExistingFilePath string   `json:"existing_file_path,omitempty"`
	Genres           []string `json:"genres,omitempty"`
	HasPoster        bool     `json:"has_poster,omitempty"`
}

type radarrTitleItem struct {
	InstanceID       string
	InstanceName     string
	ItemID           int
	Title            string
	NormTitles       []string
	Year             int
	Monitored        bool
	HasFile          bool
	RootPath         string
	ExistingFileName string
	ExistingFilePath string
	Genres           []string
	HasPoster        bool
}

type sonarrSeriesItem struct {
	InstanceID    string
	InstanceName  string
	SeriesID      int
	Title         string
	NormTitle     string
	BaseNormTitle string
	TitleHasYear  bool
	Year          int
	Monitored     bool
	RootPath      string
	Genres        []string
	HasPoster     bool
}

type PosterInfo struct {
	LocalURL  string
	RemoteURL string
}

var ErrPosterNotFound = errors.New("poster not found")

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
	ByPath           map[string][]ArrFileEntry
	ByPathCI         map[string][]ArrFileEntry
	ByNameSize       map[nameSizeKey][]ArrFileEntry
	QueueByHash      map[string][]ArrQueueEntry
	LibraryRoots     []ArrLibraryEntry
	RadarrTitles     []radarrTitleItem
	SonarrSeries     []sonarrSeriesItem
	WantedEpisodes   map[wantedEpisodeKey]wantedEpisodeInfo
	ExistingEpisodes map[wantedEpisodeKey]ArrFileEntry
	PosterMeta       map[string]PosterInfo
	Instances        []InstanceReport
}

func NewIndex() *Index {
	return &Index{
		ByPath:           make(map[string][]ArrFileEntry),
		ByPathCI:         make(map[string][]ArrFileEntry),
		ByNameSize:       make(map[nameSizeKey][]ArrFileEntry),
		QueueByHash:      make(map[string][]ArrQueueEntry),
		LibraryRoots:     make([]ArrLibraryEntry, 0),
		RadarrTitles:     make([]radarrTitleItem, 0),
		SonarrSeries:     make([]sonarrSeriesItem, 0),
		WantedEpisodes:   make(map[wantedEpisodeKey]wantedEpisodeInfo),
		ExistingEpisodes: make(map[wantedEpisodeKey]ArrFileEntry),
		PosterMeta:       make(map[string]PosterInfo),
		Instances:        make([]InstanceReport, 0),
	}
}

func extractSeasonEpisode(raw string) (season int, episode int, prefix string, ok bool) {
	spaced := strings.NewReplacer(".", " ", "_", " ", "-", " ", "[", " ", "]", " ", "(", " ", ")", " ").Replace(raw)
	loc := seasonEpisodeRe.FindStringSubmatchIndex(spaced)
	if loc == nil {
		return 0, 0, "", false
	}
	seasonNum, sErr := strconv.Atoi(spaced[loc[2]:loc[3]])
	epNum, eErr := strconv.Atoi(spaced[loc[4]:loc[5]])
	if sErr != nil || eErr != nil {
		return 0, 0, "", false
	}
	return seasonNum, epNum, normalizeTitle(spaced[:loc[0]]), true
}

func (idx *Index) addFileEntry(e ArrFileEntry) {
	norm := qbit.NormalizeSlashPath(e.MappedPath)
	idx.ByPath[norm] = append(idx.ByPath[norm], e)
	idx.ByPathCI[strings.ToLower(norm)] = append(idx.ByPathCI[strings.ToLower(norm)], e)
	k := nameSizeKey{NameLower: strings.ToLower(e.Filename), Size: e.Size}
	idx.ByNameSize[k] = append(idx.ByNameSize[k], e)
	if e.AppType == "sonarr" && e.ItemID > 0 {
		seasonNum, epNum, _, ok := extractSeasonEpisode(e.Filename)
		if !ok {
			seasonNum, epNum, _, ok = extractSeasonEpisode(e.MappedPath)
		}
		if ok {
			wk := wantedEpisodeKey{
				InstanceID: e.InstanceID,
				SeriesID:   e.ItemID,
				Season:     seasonNum,
				Episode:    epNum,
			}
			idx.ExistingEpisodes[wk] = e
		}
	}
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

func splitTrailingYear(norm string) (base string, year int, ok bool) {
	idx := strings.LastIndexByte(norm, ' ')
	if idx <= 0 {
		return norm, 0, false
	}
	tail := norm[idx+1:]
	if len(tail) != 4 {
		return norm, 0, false
	}
	y, err := strconv.Atoi(tail)
	if err != nil || y < 1900 || y > 2099 {
		return norm, 0, false
	}
	return norm[:idx], y, true
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

	// 1. Radarr title + year matching (prefer exact title+year adjacency, then longest base title)
	type radarrCandidateMatch struct {
		item     radarrTitleItem
		tier     int
		titleLen int
	}
	radarrByInst := make(map[string]map[int]radarrCandidateMatch)
	var radarrInstOrder []string

	for _, m := range idx.RadarrTitles {
		if m.Year <= 0 {
			continue
		}
		yearStr := strconv.Itoa(m.Year)
		bestTier := 0
		bestLen := 0
		for _, nc := range normCandidates {
			if !containsWordToken(nc, yearStr) {
				continue
			}
			for _, nt := range m.NormTitles {
				if nt == "" {
					continue
				}
				tier := 0
				if nc == nt+" "+yearStr || strings.HasPrefix(nc, nt+" "+yearStr+" ") {
					tier = 2
				} else if nc == nt || strings.HasPrefix(nc, nt+" ") {
					tier = 1
				}
				if tier > bestTier || (tier == bestTier && tier > 0 && len(nt) > bestLen) {
					bestTier = tier
					bestLen = len(nt)
				}
			}
		}
		if bestTier > 0 {
			instMap, exists := radarrByInst[m.InstanceID]
			if !exists {
				instMap = make(map[int]radarrCandidateMatch)
				radarrByInst[m.InstanceID] = instMap
				radarrInstOrder = append(radarrInstOrder, m.InstanceID)
			}
			if prev, dup := instMap[m.ItemID]; !dup || bestTier > prev.tier || (bestTier == prev.tier && bestLen > prev.titleLen) {
				instMap[m.ItemID] = radarrCandidateMatch{item: m, tier: bestTier, titleLen: bestLen}
			}
		}
	}

	for _, instID := range radarrInstOrder {
		instMap := radarrByInst[instID]
		maxTier := 0
		maxLenAtBest := 0
		for _, cm := range instMap {
			if cm.tier > maxTier {
				maxTier = cm.tier
				maxLenAtBest = cm.titleLen
			} else if cm.tier == maxTier && cm.titleLen > maxLenAtBest {
				maxLenAtBest = cm.titleLen
			}
		}
		for _, cm := range instMap {
			if cm.tier != maxTier {
				continue
			}
			if maxTier == 1 && cm.titleLen < maxLenAtBest {
				continue
			}
			m := cm.item
			key := fmt.Sprintf("radarr:%s:%d", m.InstanceID, m.ItemID)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, ArrTitleMatch{
				InstanceID:       m.InstanceID,
				InstanceName:     m.InstanceName,
				AppType:          "radarr",
				ItemID:           m.ItemID,
				Title:            m.Title,
				Year:             m.Year,
				Monitored:        m.Monitored,
				HasFile:          m.HasFile,
				RootPath:         m.RootPath,
				ExistingFileName: m.ExistingFileName,
				ExistingFilePath: m.ExistingFilePath,
				Genres:           m.Genres,
				HasPoster:        m.HasPoster,
			})
		}
	}

	// 2. Sonarr SxxExx + exact/year-aware series title matching
	type sonarrScoredKey struct {
		seriesID int
		season   int
		episode  int
	}
	type sonarrCandidateMatch struct {
		sr      sonarrSeriesItem
		season  int
		episode int
		tier    int
	}
	sonarrByInst := make(map[string]map[sonarrScoredKey]sonarrCandidateMatch)
	var sonarrInstOrder []string

	for _, raw := range candidates {
		seasonNum, epNum, prefix, ok := extractSeasonEpisode(raw)
		if !ok || prefix == "" {
			continue
		}
		prefixBase, prefixYear, prefixHasYear := splitTrailingYear(prefix)
		for _, sr := range idx.SonarrSeries {
			if sr.NormTitle == "" {
				continue
			}
			tier := 0
			switch {
			case (prefix == sr.NormTitle && sr.TitleHasYear) ||
				(prefixHasYear && sr.Year > 0 && prefixYear == sr.Year && prefixBase == sr.BaseNormTitle):
				tier = 3
			case prefix == sr.NormTitle:
				tier = 2
			case !prefixHasYear && sr.TitleHasYear && prefix == sr.BaseNormTitle:
				tier = 1
			}
			if tier == 0 {
				continue
			}

			instMap, exists := sonarrByInst[sr.InstanceID]
			if !exists {
				instMap = make(map[sonarrScoredKey]sonarrCandidateMatch)
				sonarrByInst[sr.InstanceID] = instMap
				sonarrInstOrder = append(sonarrInstOrder, sr.InstanceID)
			}
			sk := sonarrScoredKey{seriesID: sr.SeriesID, season: seasonNum, episode: epNum}
			if prev, dup := instMap[sk]; !dup || tier > prev.tier {
				instMap[sk] = sonarrCandidateMatch{sr: sr, season: seasonNum, episode: epNum, tier: tier}
			}
		}
	}

	for _, instID := range sonarrInstOrder {
		instMap := sonarrByInst[instID]
		maxTier := 0
		for _, cm := range instMap {
			if cm.tier > maxTier {
				maxTier = cm.tier
			}
		}
		if maxTier == 1 {
			distinctSeries := make(map[int]struct{})
			for _, cm := range instMap {
				if cm.tier == 1 {
					distinctSeries[cm.sr.SeriesID] = struct{}{}
				}
			}
			if len(distinctSeries) > 1 {
				continue
			}
		}

		for _, cm := range instMap {
			if cm.tier != maxTier {
				continue
			}
			sr := cm.sr
			seasonNum := cm.season
			epNum := cm.episode
			wk := wantedEpisodeKey{
				InstanceID: sr.InstanceID,
				SeriesID:   sr.SeriesID,
				Season:     seasonNum,
				Episode:    epNum,
			}
			if wantedEp, isWanted := idx.WantedEpisodes[wk]; isWanted {
				key := fmt.Sprintf("sonarr:%s:%d:%d", sr.InstanceID, sr.SeriesID, wantedEp.EpisodeID)
				if _, dup := seen[key]; dup {
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
					Genres:        sr.Genres,
					HasPoster:     sr.HasPoster,
				})
			} else if existingEp, hasExisting := idx.ExistingEpisodes[wk]; hasExisting {
				key := fmt.Sprintf("sonarr:%s:%d:S%dE%d", sr.InstanceID, sr.SeriesID, seasonNum, epNum)
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}
				out = append(out, ArrTitleMatch{
					InstanceID:       sr.InstanceID,
					InstanceName:     sr.InstanceName,
					AppType:          "sonarr",
					ItemID:           sr.SeriesID,
					Title:            sr.Title,
					Year:             sr.Year,
					SeasonNumber:     seasonNum,
					EpisodeNumber:    epNum,
					Monitored:        sr.Monitored,
					HasFile:          true,
					RootPath:         sr.RootPath,
					ExistingFileName: existingEp.Filename,
					ExistingFilePath: existingEp.MappedPath,
					Genres:           sr.Genres,
					HasPoster:        sr.HasPoster,
				})
			}
		}
	}

	return out
}

func ToRemotePath(localPath string, mappings []config.PathMapping) string {
	return qbit.ToRemotePath(localPath, mappings)
}

type systemStatusJSON struct {
	AppName string `json:"appName"`
	Version string `json:"version"`
}

type arrImageJSON struct {
	CoverType string `json:"coverType"`
	URL       string `json:"url"`
	RemoteURL string `json:"remoteUrl"`
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
	Genres        []string             `json:"genres"`
	Images        []arrImageJSON       `json:"images"`
	MovieFile     *radarrMovieFileJSON `json:"movieFile"`
}

type sonarrSeriesJSON struct {
	ID        int            `json:"id"`
	Title     string         `json:"title"`
	Year      int            `json:"year"`
	Path      string         `json:"path"`
	Monitored bool           `json:"monitored"`
	Genres    []string       `json:"genres"`
	Images    []arrImageJSON `json:"images"`
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
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type Client struct {
	httpClient *http.Client
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
		httpClient: &http.Client{
			Timeout:   timeout,
			Transport: transport,
		},
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
	posterMeta     map[string]PosterInfo
}

func normalizeGenres(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		g := strings.TrimSpace(raw)
		if g == "" {
			continue
		}
		lk := strings.ToLower(g)
		if _, dup := seen[lk]; dup {
			continue
		}
		seen[lk] = struct{}{}
		out = append(out, g)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func extractPosterInfo(images []arrImageJSON) (PosterInfo, bool) {
	for _, img := range images {
		if !strings.EqualFold(strings.TrimSpace(img.CoverType), "poster") {
			continue
		}
		info := PosterInfo{
			LocalURL:  strings.TrimSpace(img.URL),
			RemoteURL: strings.TrimSpace(img.RemoteURL),
		}
		if info.LocalURL != "" || info.RemoteURL != "" {
			return info, true
		}
	}
	return PosterInfo{}, false
}

func posterMetaKey(instID string, itemID int) string {
	return fmt.Sprintf("%s:%d", instID, itemID)
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
		posterMeta:     make(map[string]PosterInfo),
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
			libEntry, titleItem, fileEntry, posterInfo, hasPoster := radarrMovieToData(inst, m)
			out.libraryRoots = append(out.libraryRoots, libEntry)
			out.radarrTitles = append(out.radarrTitles, titleItem)
			if hasPoster {
				out.posterMeta[posterMetaKey(inst.ID, m.ID)] = posterInfo
			}
			if fileEntry != nil {
				out.files = append(out.files, *fileEntry)
			}
		}

		out.queue = c.fetchQueueEntries(ctx, baseURL, inst)

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

	for _, s := range series {
		libEntry, srItem, posterInfo, hasPoster := sonarrSeriesToData(inst, s)
		out.libraryRoots = append(out.libraryRoots, libEntry)
		out.sonarrSeries = append(out.sonarrSeries, srItem)
		if hasPoster {
			out.posterMeta[posterMetaKey(inst.ID, s.ID)] = posterInfo
		}
	}

	// Fetch episode files concurrently across series (max 16 workers)
	sem := make(chan struct{}, 16)
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
			localEntries := sonarrEpisodeFilesToEntries(inst, sr, epFiles)
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

	out.queue = c.fetchQueueEntries(ctx, baseURL, inst)

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

func radarrMovieToData(inst config.ArrInstanceConfig, m radarrMovieJSON) (ArrLibraryEntry, radarrTitleItem, *ArrFileEntry, PosterInfo, bool) {
	mappedRoot := qbit.ApplyPathMappings(m.Path, inst.PathMappings)
	genres := normalizeGenres(m.Genres)
	posterInfo, hasPoster := extractPosterInfo(m.Images)

	libEntry := ArrLibraryEntry{
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
		Genres:       genres,
		HasPoster:    hasPoster,
	}

	normTitles := make([]string, 0, 2)
	if nt := normalizeTitle(m.Title); nt != "" {
		normTitles = append(normTitles, nt)
	}
	if ot := normalizeTitle(m.OriginalTitle); ot != "" && (len(normTitles) == 0 || ot != normTitles[0]) {
		normTitles = append(normTitles, ot)
	}
	var fileEntry *ArrFileEntry
	var existingFileName string
	var existingFilePath string
	if m.MovieFile != nil {
		rawPath := m.MovieFile.Path
		if rawPath == "" && m.Path != "" && m.MovieFile.RelativePath != "" {
			rawPath = qbit.NormalizeSlashPath(m.Path) + "/" + strings.TrimLeft(qbit.NormalizeSlashPath(m.MovieFile.RelativePath), "/")
		}
		if rawPath != "" {
			mapped := qbit.ApplyPathMappings(rawPath, inst.PathMappings)
			existingFilePath = mapped
			existingFileName = path.Base(mapped)
			fileEntry = &ArrFileEntry{
				InstanceID:   inst.ID,
				InstanceName: inst.Name,
				AppType:      "radarr",
				ItemID:       m.ID,
				Title:        m.Title,
				Year:         m.Year,
				Monitored:    m.Monitored,
				RemotePath:   qbit.NormalizeSlashPath(rawPath),
				MappedPath:   mapped,
				Filename:     existingFileName,
				Size:         m.MovieFile.Size,
				Genres:       genres,
				HasPoster:    hasPoster,
			}
		} else if m.MovieFile.RelativePath != "" {
			existingFileName = path.Base(qbit.NormalizeSlashPath(m.MovieFile.RelativePath))
		}
	}

	titleItem := radarrTitleItem{
		InstanceID:       inst.ID,
		InstanceName:     inst.Name,
		ItemID:           m.ID,
		Title:            m.Title,
		NormTitles:       normTitles,
		Year:             m.Year,
		Monitored:        m.Monitored,
		HasFile:          m.HasFile,
		RootPath:         mappedRoot,
		ExistingFileName: existingFileName,
		ExistingFilePath: existingFilePath,
		Genres:           genres,
		HasPoster:        hasPoster,
	}
	return libEntry, titleItem, fileEntry, posterInfo, hasPoster
}

func sonarrSeriesToData(inst config.ArrInstanceConfig, s sonarrSeriesJSON) (ArrLibraryEntry, sonarrSeriesItem, PosterInfo, bool) {
	mappedRoot := qbit.ApplyPathMappings(s.Path, inst.PathMappings)
	genres := normalizeGenres(s.Genres)
	posterInfo, hasPoster := extractPosterInfo(s.Images)

	libEntry := ArrLibraryEntry{
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
		Genres:       genres,
		HasPoster:    hasPoster,
	}
	normTitle := normalizeTitle(s.Title)
	baseNormTitle := normTitle
	titleHasYear := false
	year := s.Year
	if b, y, ok := splitTrailingYear(normTitle); ok {
		if (s.Year > 0 && y == s.Year) || (s.Year == 0 && strings.HasSuffix(strings.TrimSpace(s.Title), fmt.Sprintf("(%d)", y))) {
			baseNormTitle = b
			titleHasYear = true
			if year == 0 {
				year = y
			}
		}
	}
	srItem := sonarrSeriesItem{
		InstanceID:    inst.ID,
		InstanceName:  inst.Name,
		SeriesID:      s.ID,
		Title:         s.Title,
		NormTitle:     normTitle,
		BaseNormTitle: baseNormTitle,
		TitleHasYear:  titleHasYear,
		Year:          year,
		Monitored:     s.Monitored,
		RootPath:      mappedRoot,
		Genres:        genres,
		HasPoster:     hasPoster,
	}
	return libEntry, srItem, posterInfo, hasPoster
}

func sonarrEpisodeFilesToEntries(inst config.ArrInstanceConfig, sr sonarrSeriesJSON, epFiles []sonarrEpisodeFileJSON) []ArrFileEntry {
	genres := normalizeGenres(sr.Genres)
	_, hasPoster := extractPosterInfo(sr.Images)
	out := make([]ArrFileEntry, 0, len(epFiles))
	for _, ef := range epFiles {
		rawPath := ef.Path
		if rawPath == "" && sr.Path != "" && ef.RelativePath != "" {
			rawPath = qbit.NormalizeSlashPath(sr.Path) + "/" + strings.TrimLeft(qbit.NormalizeSlashPath(ef.RelativePath), "/")
		}
		if rawPath == "" {
			continue
		}
		mapped := qbit.ApplyPathMappings(rawPath, inst.PathMappings)
		out = append(out, ArrFileEntry{
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
			Genres:       genres,
			HasPoster:    hasPoster,
		})
	}
	return out
}

func (c *Client) fetchQueueEntries(ctx context.Context, baseURL string, inst config.ArrInstanceConfig) []ArrQueueEntry {
	var qURL string
	if inst.Type == "sonarr" {
		qURL = baseURL + "/api/v3/queue?page=1&pageSize=1000&includeUnknownSeriesItems=true"
	} else {
		qURL = baseURL + "/api/v3/queue?page=1&pageSize=1000&includeUnknownMovieItems=true"
	}
	var qPage arrQueuePageJSON
	if err := c.doJSON(ctx, http.MethodGet, qURL, inst.APIKey, nil, &qPage); err != nil {
		return nil
	}
	out := make([]ArrQueueEntry, 0, len(qPage.Records))
	for _, rec := range qPage.Records {
		if strings.TrimSpace(rec.DownloadID) != "" {
			out = append(out, summarizeQueueRecord(inst, rec))
		}
	}
	return out
}

func mergeFetchedToIndex(idx *Index, results []fetchedInstanceData, includeEmptyReport bool) {
	for _, r := range results {
		if includeEmptyReport || r.report.InstanceID != "" {
			idx.Instances = append(idx.Instances, r.report)
		}
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
		for k, v := range r.posterMeta {
			idx.PosterMeta[k] = v
		}
	}
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

	mergeFetchedToIndex(idx, results, true)
	return idx
}

func uniquePositiveIDs(ids []int) []int {
	seen := make(map[int]struct{}, len(ids))
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func (c *Client) fetchInstanceItemsData(ctx context.Context, inst config.ArrInstanceConfig, itemIDs []int, includeQueue bool) (fetchedInstanceData, []string) {
	out := fetchedInstanceData{
		wantedEpisodes: make(map[wantedEpisodeKey]wantedEpisodeInfo),
		posterMeta:     make(map[string]PosterInfo),
	}
	if !inst.Enabled {
		return out, nil
	}
	baseURL := strings.TrimRight(strings.TrimSpace(inst.URL), "/")
	if baseURL == "" {
		msg := "empty URL"
		out.report = InstanceReport{
			InstanceID: inst.ID, InstanceName: inst.Name, AppType: inst.Type, URL: inst.URL, OK: false, Error: &msg,
		}
		return out, nil
	}

	uniqueIDs := uniquePositiveIDs(itemIDs)
	var discovered []string
	var mu sync.Mutex
	var errs []string
	var wg sync.WaitGroup

	if includeQueue {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q := c.fetchQueueEntries(ctx, baseURL, inst)
			mu.Lock()
			out.queue = append(out.queue, q...)
			mu.Unlock()
		}()
	}

	sem := make(chan struct{}, 16)
	if inst.Type == "radarr" {
		for _, id := range uniqueIDs {
			wg.Add(1)
			go func(movieID int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				var m radarrMovieJSON
				if err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("%s/api/v3/movie/%d", baseURL, movieID), inst.APIKey, nil, &m); err != nil {
					mu.Lock()
					if len(errs) < 3 {
						errs = append(errs, err.Error())
					}
					mu.Unlock()
					return
				}
				libEntry, titleItem, fileEntry, posterInfo, hasPoster := radarrMovieToData(inst, m)
				mu.Lock()
				out.libraryRoots = append(out.libraryRoots, libEntry)
				out.radarrTitles = append(out.radarrTitles, titleItem)
				if hasPoster {
					out.posterMeta[posterMetaKey(inst.ID, m.ID)] = posterInfo
				}
				if fileEntry != nil {
					out.files = append(out.files, *fileEntry)
					if fileEntry.MappedPath != "" {
						discovered = append(discovered, fileEntry.MappedPath)
					}
				}
				mu.Unlock()
			}(id)
		}
		wg.Wait()
	} else {
		var wantedFallbackOnce sync.Once
		var missingFallbackNeeded bool

		for _, id := range uniqueIDs {
			wg.Add(1)
			go func(seriesID int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				var sr sonarrSeriesJSON
				if err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("%s/api/v3/series/%d", baseURL, seriesID), inst.APIKey, nil, &sr); err != nil {
					mu.Lock()
					if len(errs) < 3 {
						errs = append(errs, err.Error())
					}
					mu.Unlock()
					return
				}

				libEntry, srItem, posterInfo, hasPoster := sonarrSeriesToData(inst, sr)
				mu.Lock()
				out.libraryRoots = append(out.libraryRoots, libEntry)
				out.sonarrSeries = append(out.sonarrSeries, srItem)
				if hasPoster {
					out.posterMeta[posterMetaKey(inst.ID, sr.ID)] = posterInfo
				}
				mu.Unlock()

				var epFiles []sonarrEpisodeFileJSON
				if err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("%s/api/v3/episodefile?seriesId=%d", baseURL, seriesID), inst.APIKey, nil, &epFiles); err != nil {
					mu.Lock()
					if len(errs) < 3 {
						errs = append(errs, err.Error())
					}
					mu.Unlock()
				} else {
					entries := sonarrEpisodeFilesToEntries(inst, sr, epFiles)
					mu.Lock()
					out.files = append(out.files, entries...)
					for _, e := range entries {
						if e.MappedPath != "" {
							discovered = append(discovered, e.MappedPath)
						}
					}
					mu.Unlock()
				}

				var episodes []sonarrWantedRecordJSON
				if err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("%s/api/v3/episode?seriesId=%d", baseURL, seriesID), inst.APIKey, nil, &episodes); err == nil {
					mu.Lock()
					for _, ep := range episodes {
						if !ep.HasFile && ep.Monitored {
							wk := wantedEpisodeKey{
								InstanceID: inst.ID,
								SeriesID:   ep.SeriesID,
								Season:     ep.SeasonNumber,
								Episode:    ep.EpisodeNumber,
							}
							out.wantedEpisodes[wk] = wantedEpisodeInfo{
								EpisodeID: ep.ID,
								Title:     ep.Title,
								Monitored: ep.Monitored,
							}
						}
					}
					mu.Unlock()
				} else {
					mu.Lock()
					missingFallbackNeeded = true
					mu.Unlock()
				}
			}(id)
		}
		wg.Wait()

		if missingFallbackNeeded {
			wantedFallbackOnce.Do(func() {
				targetSet := make(map[int]struct{}, len(uniqueIDs))
				for _, id := range uniqueIDs {
					targetSet[id] = struct{}{}
				}
				var wPage sonarrWantedPageJSON
				wURL := baseURL + "/api/v3/wanted/missing?page=1&pageSize=1000&includeSeries=true"
				if err := c.doJSON(ctx, http.MethodGet, wURL, inst.APIKey, nil, &wPage); err == nil {
					for _, rec := range wPage.Records {
						if _, ok := targetSet[rec.SeriesID]; !ok {
							continue
						}
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
				}
			})
		}
	}

	var errPtr *string
	if len(errs) > 0 {
		joined := strings.Join(errs, "; ")
		errPtr = &joined
	}
	out.report = InstanceReport{
		InstanceID:   inst.ID,
		InstanceName: inst.Name,
		AppType:      inst.Type,
		URL:          inst.URL,
		OK:           len(errs) == 0,
		ItemCount:    len(uniqueIDs),
		FileCount:    len(out.files),
		QueueCount:   len(out.queue),
		Error:        errPtr,
	}
	return out, discovered
}

// BuildIndexForItems builds a targeted Arr index for only the specified item IDs per instance
// and returns discovered mapped media paths from those items.
func (c *Client) BuildIndexForItems(ctx context.Context, instances []config.ArrInstanceConfig, itemIDsByInstance map[string][]int, includeQueue bool) (*Index, []string) {
	idx := NewIndex()
	if len(instances) == 0 || len(itemIDsByInstance) == 0 {
		return idx, nil
	}

	results := make([]fetchedInstanceData, len(instances))
	discoveredAll := make([][]string, len(instances))
	var wg sync.WaitGroup

	for i, inst := range instances {
		itemIDs, ok := itemIDsByInstance[inst.ID]
		if !ok || !inst.Enabled {
			continue
		}
		wg.Add(1)
		go func(pos int, cfg config.ArrInstanceConfig, ids []int) {
			defer wg.Done()
			res, paths := c.fetchInstanceItemsData(ctx, cfg, ids, includeQueue)
			results[pos] = res
			discoveredAll[pos] = paths
		}(i, inst, itemIDs)
	}
	wg.Wait()

	mergeFetchedToIndex(idx, results, false)
	seenPaths := make(map[string]struct{})
	discovered := make([]string, 0)
	for _, list := range discoveredAll {
		for _, p := range list {
			norm := qbit.NormalizeSlashPath(p)
			if norm == "" {
				continue
			}
			if _, ok := seenPaths[norm]; ok {
				continue
			}
			seenPaths[norm] = struct{}{}
			discovered = append(discovered, norm)
		}
	}
	return idx, discovered
}

// GetCommandStatus queries Radarr/Sonarr /api/v3/command/{id} to check background command progress.
func (c *Client) GetCommandStatus(ctx context.Context, inst config.ArrInstanceConfig, commandID int) (CommandResponse, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(inst.URL), "/")
	if baseURL == "" {
		return CommandResponse{}, errors.New("empty Radarr/Sonarr URL")
	}
	var res CommandResponse
	err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("%s/api/v3/command/%d", baseURL, commandID), inst.APIKey, nil, &res)
	return res, err
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

func normalizeImportMode(raw string) (scanMode string, manualMode string) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "copy":
		return "Copy", "copy"
	case "move":
		return "Move", "move"
	default:
		return "Auto", "auto"
	}
}

func (c *Client) TriggerDownloadScan(ctx context.Context, inst config.ArrInstanceConfig, remotePath, downloadClientID, importMode string) (CommandResponse, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(inst.URL), "/")
	cmdName := "DownloadedMoviesScan"
	if strings.ToLower(inst.Type) == "sonarr" {
		cmdName = "DownloadedEpisodesScan"
	}
	scanMode, _ := normalizeImportMode(importMode)
	payload := map[string]any{
		"name":       cmdName,
		"path":       remotePath,
		"importMode": scanMode,
	}
	if strings.TrimSpace(downloadClientID) != "" {
		payload["downloadClientId"] = strings.ToUpper(strings.TrimSpace(downloadClientID))
	}
	var res CommandResponse
	err := c.doJSON(ctx, http.MethodPost, baseURL+"/api/v3/command", inst.APIKey, payload, &res)
	return res, err
}

func (c *Client) TriggerManualImport(ctx context.Context, inst config.ArrInstanceConfig, files []ManualImportFilePayload, importMode string) (CommandResponse, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(inst.URL), "/")
	_, manualMode := normalizeImportMode(importMode)
	payload := map[string]any{
		"name":       "ManualImport",
		"importMode": manualMode,
		"files":      files,
	}
	var res CommandResponse
	err := c.doJSON(ctx, http.MethodPost, baseURL+"/api/v3/command", inst.APIKey, payload, &res)
	return res, err
}

func (c *Client) tryFetchImage(ctx context.Context, rawURL, apiKey string, sendAPIKey bool) ([]byte, string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", false
	}
	if sendAPIKey && strings.TrimSpace(apiKey) != "" {
		req.Header.Set("X-Api-Key", apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", false
	}

	const maxPosterBytes = 5 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPosterBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxPosterBytes {
		return nil, "", false
	}

	ct := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	if ct == "" || !strings.HasPrefix(strings.ToLower(ct), "image/") {
		ct = strings.TrimSpace(strings.Split(http.DetectContentType(data), ";")[0])
	}
	if !strings.HasPrefix(strings.ToLower(ct), "image/") {
		return nil, "", false
	}
	return data, ct, true
}

// FetchPoster proxies an item's poster artwork from Radarr/Sonarr (with remote fallback if needed)
// without exposing the instance API key to browser clients.
func (c *Client) FetchPoster(ctx context.Context, inst config.ArrInstanceConfig, itemID int, fallback PosterInfo) ([]byte, string, error) {
	if !inst.Enabled {
		return nil, "", ErrPosterNotFound
	}
	baseURL := strings.TrimRight(strings.TrimSpace(inst.URL), "/")
	if baseURL == "" || itemID <= 0 {
		return nil, "", ErrPosterNotFound
	}

	seen := make(map[string]struct{}, 4)
	tryCandidate := func(u string, sendKey bool) ([]byte, string, bool) {
		if u == "" {
			return nil, "", false
		}
		if _, dup := seen[u]; dup {
			return nil, "", false
		}
		seen[u] = struct{}{}
		return c.tryFetchImage(ctx, u, inst.APIKey, sendKey)
	}

	candidates := []string{
		fmt.Sprintf("%s/api/v3/mediacover/%d/poster-250.jpg", baseURL, itemID),
		fmt.Sprintf("%s/api/v3/mediacover/%d/poster.jpg", baseURL, itemID),
	}
	if strings.HasPrefix(fallback.LocalURL, "/") {
		resolvedLocal := baseURL + fallback.LocalURL
		if parsed, err := url.Parse(baseURL); err == nil && parsed.Scheme != "" && parsed.Host != "" {
			basePath := strings.TrimRight(parsed.Path, "/")
			if basePath != "" && strings.HasPrefix(fallback.LocalURL, basePath+"/") {
				resolvedLocal = parsed.Scheme + "://" + parsed.Host + fallback.LocalURL
			}
		}
		candidates = append(candidates, resolvedLocal)
	}

	for _, u := range candidates {
		if data, ct, ok := tryCandidate(u, true); ok {
			return data, ct, nil
		}
	}

	lowerRemote := strings.ToLower(fallback.RemoteURL)
	if strings.HasPrefix(lowerRemote, "https://") || strings.HasPrefix(lowerRemote, "http://") {
		if data, ct, ok := tryCandidate(fallback.RemoteURL, false); ok {
			return data, ct, nil
		}
	}

	return nil, "", ErrPosterNotFound
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
