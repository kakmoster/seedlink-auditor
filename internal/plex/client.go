package plex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"seedlink-auditor/internal/config"
	"seedlink-auditor/internal/qbit"
)

var (
	seasonEpisodeRe = regexp.MustCompile(`(?i)\bS(\d{1,2})E(\d{1,3})\b`)
	safeRatingKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

	ErrPosterNotFound = errors.New("plex poster not found")
)

type TestResult struct {
	OK           bool   `json:"ok"`
	ServerName   string `json:"server_name,omitempty"`
	Version      string `json:"version,omitempty"`
	LibraryCount int    `json:"library_count,omitempty"`
	ItemCount    int    `json:"item_count,omitempty"`
	Error        string `json:"error,omitempty"`
}

type InstanceReport struct {
	InstanceID   string  `json:"instance_id"`
	InstanceName string  `json:"instance_name"`
	URL          string  `json:"url"`
	OK           bool    `json:"ok"`
	LibraryCount int     `json:"library_count"`
	ItemCount    int     `json:"item_count"`
	FileCount    int     `json:"file_count"`
	Error        *string `json:"error"`
}

type PlexFileEntry struct {
	InstanceID    string   `json:"instance_id"`
	InstanceName  string   `json:"instance_name"`
	LibraryTitle  string   `json:"library_title,omitempty"`
	MediaType     string   `json:"media_type"` // "movie" or "show"
	RatingKey     string   `json:"rating_key"` // movie key or parent show key for poster
	EpisodeKey    string   `json:"episode_key,omitempty"`
	Title         string   `json:"title"`
	Year          int      `json:"year,omitempty"`
	SeasonNumber  int      `json:"season_number,omitempty"`
	EpisodeNumber int      `json:"episode_number,omitempty"`
	RemotePath    string   `json:"remote_path"`
	MappedPath    string   `json:"mapped_path"`
	Filename      string   `json:"-"`
	Size          int64    `json:"size"`
	Genres        []string `json:"genres,omitempty"`
	HasPoster     bool     `json:"has_poster,omitempty"`
}

type PlexFileMatch struct {
	Entry     PlexFileEntry
	MatchType string // "path" or "name_size"
}

type PlexTitleMatch struct {
	InstanceID       string   `json:"instance_id"`
	InstanceName     string   `json:"instance_name"`
	LibraryTitle     string   `json:"library_title,omitempty"`
	MediaType        string   `json:"media_type"` // "movie" or "show"
	RatingKey        string   `json:"rating_key"`
	Title            string   `json:"title"`
	Year             int      `json:"year,omitempty"`
	SeasonNumber     int      `json:"season_number,omitempty"`
	EpisodeNumber    int      `json:"episode_number,omitempty"`
	HasMatchingFile  bool     `json:"has_matching_file"`
	ExistingFileName string   `json:"existing_file_name,omitempty"`
	ExistingFilePath string   `json:"existing_file_path,omitempty"`
	StatusMessage    string   `json:"status_message,omitempty"`
	Genres           []string `json:"genres,omitempty"`
	HasPoster        bool     `json:"has_poster,omitempty"`
}

type plexMovieItem struct {
	InstanceID       string
	InstanceName     string
	LibraryTitle     string
	RatingKey        string
	Title            string
	NormTitles       []string
	Year             int
	ExistingFileName string
	ExistingFilePath string
	Genres           []string
	HasPoster        bool
}

type plexShowItem struct {
	InstanceID    string
	InstanceName  string
	LibraryTitle  string
	RatingKey     string
	Title         string
	NormTitle     string
	BaseNormTitle string
	TitleHasYear  bool
	Year          int
	Genres        []string
	HasPoster     bool
}

type plexEpisodeKey struct {
	InstanceID    string
	ShowRatingKey string
	Season        int
	Episode       int
}

type nameSizeKey struct {
	NameLower string
	Size      int64
}

type Index struct {
	ByPath           map[string][]PlexFileEntry
	ByPathCI         map[string][]PlexFileEntry
	ByNameSize       map[nameSizeKey][]PlexFileEntry
	Movies           []plexMovieItem
	Shows            []plexShowItem
	ExistingEpisodes map[plexEpisodeKey]PlexFileEntry
	PosterMeta       map[string]string
	Instances        []InstanceReport
}

func NewIndex() *Index {
	return &Index{
		ByPath:           make(map[string][]PlexFileEntry),
		ByPathCI:         make(map[string][]PlexFileEntry),
		ByNameSize:       make(map[nameSizeKey][]PlexFileEntry),
		Movies:           make([]plexMovieItem, 0),
		Shows:            make([]plexShowItem, 0),
		ExistingEpisodes: make(map[plexEpisodeKey]PlexFileEntry),
		PosterMeta:       make(map[string]string),
		Instances:        make([]InstanceReport, 0),
	}
}

func posterMetaKey(instID, ratingKey string) string {
	return fmt.Sprintf("%s:%s", instID, ratingKey)
}

func isSafeThumbPath(p string) bool {
	trimmed := strings.TrimSpace(p)
	return strings.HasPrefix(trimmed, "/") &&
		!strings.HasPrefix(trimmed, "//") &&
		!strings.ContainsAny(trimmed, "\r\n")
}

func IsSafeRatingKey(key string) bool {
	trimmed := strings.TrimSpace(key)
	return trimmed != "" && safeRatingKeyRe.MatchString(trimmed)
}

func mapRemotePath(remotePath string, mappings []config.PathMapping) string {
	if len(mappings) == 0 {
		return qbit.NormalizeSlashPath(remotePath)
	}
	sorted := make([]config.PathMapping, len(mappings))
	copy(sorted, mappings)
	sort.SliceStable(sorted, func(i, j int) bool {
		return len(qbit.NormalizeSlashPath(sorted[i].RemotePrefix)) > len(qbit.NormalizeSlashPath(sorted[j].RemotePrefix))
	})
	return qbit.ApplyPathMappings(remotePath, sorted)
}

func normalizeGenres(genres []plexGenreJSON) []string {
	if len(genres) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(genres))
	out := make([]string, 0, len(genres))
	for _, g := range genres {
		t := strings.TrimSpace(g.Tag)
		if t == "" {
			continue
		}
		lk := strings.ToLower(t)
		if _, dup := seen[lk]; dup {
			continue
		}
		seen[lk] = struct{}{}
		out = append(out, t)
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

func buildTitleVariants(title, originalTitle string, year int) []string {
	rawVariants := []string{title, originalTitle}
	seen := make(map[string]struct{}, 4)
	out := make([]string, 0, 4)
	yearStr := ""
	if year > 0 {
		yearStr = strconv.Itoa(year)
	}
	for _, raw := range rawVariants {
		n := normalizeTitle(raw)
		if n == "" {
			continue
		}
		if _, dup := seen[n]; !dup {
			seen[n] = struct{}{}
			out = append(out, n)
		}
		if yearStr != "" && strings.HasSuffix(n, " "+yearStr) {
			trimmed := strings.TrimSpace(strings.TrimSuffix(n, " "+yearStr))
			if trimmed != "" {
				if _, dup := seen[trimmed]; !dup {
					seen[trimmed] = struct{}{}
					out = append(out, trimmed)
				}
			}
		}
	}
	return out
}

func (idx *Index) addFileEntry(e PlexFileEntry) {
	norm := qbit.NormalizeSlashPath(e.MappedPath)
	idx.ByPath[norm] = append(idx.ByPath[norm], e)
	idx.ByPathCI[strings.ToLower(norm)] = append(idx.ByPathCI[strings.ToLower(norm)], e)
	k := nameSizeKey{NameLower: strings.ToLower(e.Filename), Size: e.Size}
	idx.ByNameSize[k] = append(idx.ByNameSize[k], e)
	if e.MediaType == "show" && e.RatingKey != "" && (e.SeasonNumber > 0 || e.EpisodeNumber > 0) {
		ek := plexEpisodeKey{
			InstanceID:    e.InstanceID,
			ShowRatingKey: e.RatingKey,
			Season:        e.SeasonNumber,
			Episode:       e.EpisodeNumber,
		}
		if _, exists := idx.ExistingEpisodes[ek]; !exists {
			idx.ExistingEpisodes[ek] = e
		}
	}
}

func (idx *Index) FindFileMatches(absPath, filename string, size int64) []PlexFileMatch {
	if idx == nil {
		return nil
	}
	norm := qbit.NormalizeSlashPath(absPath)
	if list, ok := idx.ByPath[norm]; ok {
		out := make([]PlexFileMatch, 0, len(list))
		for _, e := range list {
			out = append(out, PlexFileMatch{Entry: e, MatchType: "path"})
		}
		return out
	}
	if list, ok := idx.ByPathCI[strings.ToLower(norm)]; ok {
		out := make([]PlexFileMatch, 0, len(list))
		for _, e := range list {
			out = append(out, PlexFileMatch{Entry: e, MatchType: "path"})
		}
		return out
	}
	k := nameSizeKey{NameLower: strings.ToLower(filename), Size: size}
	if list, ok := idx.ByNameSize[k]; ok {
		out := make([]PlexFileMatch, 0, len(list))
		for _, e := range list {
			out = append(out, PlexFileMatch{Entry: e, MatchType: "name_size"})
		}
		return out
	}
	return nil
}

func (idx *Index) FindTitleMatches(filename, torrentName string) []PlexTitleMatch {
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
	out := make([]PlexTitleMatch, 0)

	// 1. Check TV episodes first if an SxxExx token is present
	type showScoredKey struct {
		ratingKey string
		season    int
		episode   int
	}
	type showCandidateMatch struct {
		sr      plexShowItem
		season  int
		episode int
		tier    int
	}
	showsByInst := make(map[string]map[showScoredKey]showCandidateMatch)
	var showInstOrder []string
	anyEpisodeToken := false

	for _, raw := range candidates {
		seasonNum, epNum, prefix, ok := extractSeasonEpisode(raw)
		if !ok || prefix == "" {
			continue
		}
		anyEpisodeToken = true
		prefixBase, prefixYear, prefixHasYear := splitTrailingYear(prefix)
		for _, sr := range idx.Shows {
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
			instMap, exists := showsByInst[sr.InstanceID]
			if !exists {
				instMap = make(map[showScoredKey]showCandidateMatch)
				showsByInst[sr.InstanceID] = instMap
				showInstOrder = append(showInstOrder, sr.InstanceID)
			}
			sk := showScoredKey{ratingKey: sr.RatingKey, season: seasonNum, episode: epNum}
			if prev, dup := instMap[sk]; !dup || tier > prev.tier {
				instMap[sk] = showCandidateMatch{sr: sr, season: seasonNum, episode: epNum, tier: tier}
			}
		}
	}

	for _, instID := range showInstOrder {
		instMap := showsByInst[instID]
		maxTier := 0
		for _, cm := range instMap {
			if cm.tier > maxTier {
				maxTier = cm.tier
			}
		}
		if maxTier == 1 {
			distinctShows := make(map[string]struct{})
			for _, cm := range instMap {
				if cm.tier == 1 {
					distinctShows[cm.sr.RatingKey] = struct{}{}
				}
			}
			if len(distinctShows) > 1 {
				continue
			}
		}
		for _, cm := range instMap {
			if cm.tier != maxTier {
				continue
			}
			sr := cm.sr
			ek := plexEpisodeKey{
				InstanceID:    sr.InstanceID,
				ShowRatingKey: sr.RatingKey,
				Season:        cm.season,
				Episode:       cm.episode,
			}
			key := fmt.Sprintf("show:%s:%s:S%dE%d", sr.InstanceID, sr.RatingKey, cm.season, cm.episode)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}

			if existingEp, ok := idx.ExistingEpisodes[ek]; ok {
				msg := "Plex already has another version of this episode"
				if existingEp.Filename != "" {
					msg = "Plex already has: " + existingEp.Filename
				}
				out = append(out, PlexTitleMatch{
					InstanceID:       sr.InstanceID,
					InstanceName:     sr.InstanceName,
					LibraryTitle:     sr.LibraryTitle,
					MediaType:        "show",
					RatingKey:        sr.RatingKey,
					Title:            sr.Title,
					Year:             sr.Year,
					SeasonNumber:     cm.season,
					EpisodeNumber:    cm.episode,
					HasMatchingFile:  true,
					ExistingFileName: existingEp.Filename,
					ExistingFilePath: existingEp.MappedPath,
					StatusMessage:    msg,
					Genres:           sr.Genres,
					HasPoster:        sr.HasPoster,
				})
			} else {
				out = append(out, PlexTitleMatch{
					InstanceID:      sr.InstanceID,
					InstanceName:    sr.InstanceName,
					LibraryTitle:    sr.LibraryTitle,
					MediaType:       "show",
					RatingKey:       sr.RatingKey,
					Title:           sr.Title,
					Year:            sr.Year,
					SeasonNumber:    cm.season,
					EpisodeNumber:   cm.episode,
					HasMatchingFile: false,
					StatusMessage:   fmt.Sprintf("Series is in Plex, but S%02dE%02d is missing from Plex", cm.season, cm.episode),
					Genres:          sr.Genres,
					HasPoster:       sr.HasPoster,
				})
			}
		}
	}

	// Only check movie titles when no SxxExx token was present (prevents false movie hits on episode filenames)
	if anyEpisodeToken {
		return out
	}

	type movieCandidateMatch struct {
		item     plexMovieItem
		tier     int
		titleLen int
	}
	moviesByInst := make(map[string]map[string]movieCandidateMatch)
	var movieInstOrder []string

	for _, m := range idx.Movies {
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
			instMap, exists := moviesByInst[m.InstanceID]
			if !exists {
				instMap = make(map[string]movieCandidateMatch)
				moviesByInst[m.InstanceID] = instMap
				movieInstOrder = append(movieInstOrder, m.InstanceID)
			}
			if prev, dup := instMap[m.RatingKey]; !dup || bestTier > prev.tier || (bestTier == prev.tier && bestLen > prev.titleLen) {
				instMap[m.RatingKey] = movieCandidateMatch{item: m, tier: bestTier, titleLen: bestLen}
			}
		}
	}

	for _, instID := range movieInstOrder {
		instMap := moviesByInst[instID]
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
			key := fmt.Sprintf("movie:%s:%s", m.InstanceID, m.RatingKey)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			msg := "Plex already has another version of this movie"
			if m.ExistingFileName != "" {
				msg = "Plex already has: " + m.ExistingFileName
			}
			out = append(out, PlexTitleMatch{
				InstanceID:       m.InstanceID,
				InstanceName:     m.InstanceName,
				LibraryTitle:     m.LibraryTitle,
				MediaType:        "movie",
				RatingKey:        m.RatingKey,
				Title:            m.Title,
				Year:             m.Year,
				HasMatchingFile:  true,
				ExistingFileName: m.ExistingFileName,
				ExistingFilePath: m.ExistingFilePath,
				StatusMessage:    msg,
				Genres:           m.Genres,
				HasPoster:        m.HasPoster,
			})
		}
	}

	return out
}

// flexString accepts either a JSON string or JSON number cleanly across Plex server versions.
type flexString string

func (f *flexString) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*f = ""
		return nil
	}
	if strings.HasPrefix(trimmed, "\"") {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	*f = flexString(trimmed)
	return nil
}

type plexGenreJSON struct {
	Tag string `json:"tag"`
}

type plexPartJSON struct {
	File string `json:"file"`
	Size int64  `json:"size"`
}

type plexMediaJSON struct {
	Part []plexPartJSON `json:"Part"`
}

type plexDirectoryJSON struct {
	Key   flexString `json:"key"`
	Type  string     `json:"type"`
	Title string     `json:"title"`
}

type plexMetadataJSON struct {
	RatingKey            flexString      `json:"ratingKey"`
	GrandparentRatingKey flexString      `json:"grandparentRatingKey"`
	GrandparentTitle     string          `json:"grandparentTitle"`
	GrandparentThumb     string          `json:"grandparentThumb"`
	Title                string          `json:"title"`
	OriginalTitle        string          `json:"originalTitle"`
	Year                 int             `json:"year"`
	ParentIndex          int             `json:"parentIndex"` // season number on episodes
	Index                int             `json:"index"`       // episode number on episodes
	Thumb                string          `json:"thumb"`
	Genre                []plexGenreJSON `json:"Genre"`
	Media                []plexMediaJSON `json:"Media"`
}

type plexContainerEnvelope struct {
	MediaContainer struct {
		FriendlyName string              `json:"friendlyName"`
		Version      string              `json:"version"`
		Size         int                 `json:"size"`
		TotalSize    int                 `json:"totalSize"`
		Directory    []plexDirectoryJSON `json:"Directory"`
		Metadata     []plexMetadataJSON  `json:"Metadata"`
	} `json:"MediaContainer"`
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

func (c *Client) doGetJSON(ctx context.Context, rawURL, token string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(token) != "" {
		req.Header.Set("X-Plex-Token", strings.TrimSpace(token))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("Plex authentication failed (HTTP %d — check X-Plex-Token)", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 180 {
			snippet = snippet[:180] + "..."
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet)
	}

	return json.Unmarshal(body, dst)
}

func (c *Client) fetchSectionContainer(ctx context.Context, baseURL, token, sectionKey string, plexType int, start, size int) (plexContainerEnvelope, error) {
	u := fmt.Sprintf("%s/library/sections/%s/all?type=%d&X-Plex-Container-Start=%d&X-Plex-Container-Size=%d",
		baseURL, url.PathEscape(sectionKey), plexType, start, size)
	var env plexContainerEnvelope
	err := c.doGetJSON(ctx, u, token, &env)
	return env, err
}

func (c *Client) fetchSectionAll(ctx context.Context, baseURL, token, sectionKey string, plexType int) ([]plexMetadataJSON, error) {
	const pageSize = 2000
	const maxPages = 500
	var all []plexMetadataJSON
	start := 0

	for page := 0; page < maxPages; page++ {
		env, err := c.fetchSectionContainer(ctx, baseURL, token, sectionKey, plexType, start, pageSize)
		if err != nil {
			return nil, err
		}
		batch := env.MediaContainer.Metadata
		if len(batch) == 0 {
			break
		}
		all = append(all, batch...)
		total := env.MediaContainer.TotalSize
		if (total > 0 && start+len(batch) >= total) || (total <= 0 && len(batch) < pageSize) {
			break
		}
		start += len(batch)
	}
	return all, nil
}

func (c *Client) TestConnection(ctx context.Context, inst config.PlexInstanceConfig) TestResult {
	baseURL := strings.TrimRight(strings.TrimSpace(inst.URL), "/")
	if baseURL == "" {
		return TestResult{OK: false, Error: "Plex server URL is empty"}
	}

	var rootEnv plexContainerEnvelope
	if err := c.doGetJSON(ctx, baseURL+"/", inst.Token, &rootEnv); err != nil {
		return TestResult{OK: false, Error: err.Error()}
	}

	var secEnv plexContainerEnvelope
	if err := c.doGetJSON(ctx, baseURL+"/library/sections", inst.Token, &secEnv); err != nil {
		return TestResult{OK: false, Error: err.Error()}
	}

	libCount := 0
	itemCount := 0
	for _, dir := range secEnv.MediaContainer.Directory {
		secType := strings.ToLower(strings.TrimSpace(dir.Type))
		secKey := string(dir.Key)
		if secKey == "" || (secType != "movie" && secType != "show") {
			continue
		}
		libCount++
		plexType := 1
		if secType == "show" {
			plexType = 2
		}
		countEnv, countErr := c.fetchSectionContainer(ctx, baseURL, inst.Token, secKey, plexType, 0, 0)
		if countErr != nil {
			continue
		}
		n := countEnv.MediaContainer.TotalSize
		if n <= 0 && len(countEnv.MediaContainer.Metadata) > 0 {
			n = len(countEnv.MediaContainer.Metadata)
		}
		itemCount += n
	}

	return TestResult{
		OK:           true,
		ServerName:   rootEnv.MediaContainer.FriendlyName,
		Version:      rootEnv.MediaContainer.Version,
		LibraryCount: libCount,
		ItemCount:    itemCount,
	}
}

type fetchedInstanceData struct {
	report     InstanceReport
	files      []PlexFileEntry
	movies     []plexMovieItem
	shows      []plexShowItem
	posterMeta map[string]string
}

func (c *Client) fetchInstanceData(ctx context.Context, inst config.PlexInstanceConfig) fetchedInstanceData {
	rep := InstanceReport{
		InstanceID:   inst.ID,
		InstanceName: inst.Name,
		URL:          inst.URL,
	}
	if !inst.Enabled {
		msg := "Disabled"
		rep.OK = true
		rep.Error = &msg
		return fetchedInstanceData{report: rep}
	}

	baseURL := strings.TrimRight(strings.TrimSpace(inst.URL), "/")
	if baseURL == "" {
		msg := "Plex server URL is empty"
		rep.Error = &msg
		return fetchedInstanceData{report: rep}
	}

	var secEnv plexContainerEnvelope
	if err := c.doGetJSON(ctx, baseURL+"/library/sections", inst.Token, &secEnv); err != nil {
		msg := err.Error()
		rep.Error = &msg
		return fetchedInstanceData{report: rep}
	}

	out := fetchedInstanceData{
		posterMeta: make(map[string]string),
	}

	for _, dir := range secEnv.MediaContainer.Directory {
		secType := strings.ToLower(strings.TrimSpace(dir.Type))
		secKey := string(dir.Key)
		if secKey == "" || (secType != "movie" && secType != "show") {
			continue
		}
		rep.LibraryCount++

		if secType == "movie" {
			items, err := c.fetchSectionAll(ctx, baseURL, inst.Token, secKey, 1)
			if err != nil {
				msg := err.Error()
				rep.Error = &msg
				return fetchedInstanceData{report: rep}
			}
			rep.ItemCount += len(items)
			for _, mv := range items {
				rk := strings.TrimSpace(string(mv.RatingKey))
				if rk == "" {
					continue
				}
				genres := normalizeGenres(mv.Genre)
				hasPoster := false
				if isSafeThumbPath(mv.Thumb) {
					out.posterMeta[posterMetaKey(inst.ID, rk)] = strings.TrimSpace(mv.Thumb)
					hasPoster = true
				}

				firstFileName := ""
				firstFilePath := ""
				for _, media := range mv.Media {
					for _, part := range media.Part {
						if strings.TrimSpace(part.File) == "" {
							continue
						}
						mapped := mapRemotePath(part.File, inst.PathMappings)
						fname := path.Base(mapped)
						if firstFileName == "" {
							firstFileName = fname
							firstFilePath = mapped
						}
						out.files = append(out.files, PlexFileEntry{
							InstanceID:   inst.ID,
							InstanceName: inst.Name,
							LibraryTitle: dir.Title,
							MediaType:    "movie",
							RatingKey:    rk,
							Title:        mv.Title,
							Year:         mv.Year,
							RemotePath:   part.File,
							MappedPath:   mapped,
							Filename:     fname,
							Size:         part.Size,
							Genres:       genres,
							HasPoster:    hasPoster,
						})
					}
				}
				out.movies = append(out.movies, plexMovieItem{
					InstanceID:       inst.ID,
					InstanceName:     inst.Name,
					LibraryTitle:     dir.Title,
					RatingKey:        rk,
					Title:            mv.Title,
					NormTitles:       buildTitleVariants(mv.Title, mv.OriginalTitle, mv.Year),
					Year:             mv.Year,
					ExistingFileName: firstFileName,
					ExistingFilePath: firstFilePath,
					Genres:           genres,
					HasPoster:        hasPoster,
				})
			}
			continue
		}

		// TV show section: bulk fetch all shows (type=2) then all episodes (type=4)
		shows, err := c.fetchSectionAll(ctx, baseURL, inst.Token, secKey, 2)
		if err != nil {
			msg := err.Error()
			rep.Error = &msg
			return fetchedInstanceData{report: rep}
		}
		rep.ItemCount += len(shows)

		showByKey := make(map[string]plexShowItem, len(shows))
		for _, sh := range shows {
			rk := strings.TrimSpace(string(sh.RatingKey))
			if rk == "" {
				continue
			}
			genres := normalizeGenres(sh.Genre)
			hasPoster := false
			if isSafeThumbPath(sh.Thumb) {
				out.posterMeta[posterMetaKey(inst.ID, rk)] = strings.TrimSpace(sh.Thumb)
				hasPoster = true
			}
			normTitle := normalizeTitle(sh.Title)
			baseNormTitle := normTitle
			titleHasYear := false
			showYear := sh.Year
			if b, y, ok := splitTrailingYear(normTitle); ok {
				if (sh.Year > 0 && y == sh.Year) || (sh.Year == 0 && strings.HasSuffix(strings.TrimSpace(sh.Title), fmt.Sprintf("(%d)", y))) {
					baseNormTitle = b
					titleHasYear = true
					if showYear == 0 {
						showYear = y
					}
				}
			}
			item := plexShowItem{
				InstanceID:    inst.ID,
				InstanceName:  inst.Name,
				LibraryTitle:  dir.Title,
				RatingKey:     rk,
				Title:         sh.Title,
				NormTitle:     normTitle,
				BaseNormTitle: baseNormTitle,
				TitleHasYear:  titleHasYear,
				Year:          showYear,
				Genres:        genres,
				HasPoster:     hasPoster,
			}
			showByKey[rk] = item
			out.shows = append(out.shows, item)
		}

		episodes, err := c.fetchSectionAll(ctx, baseURL, inst.Token, secKey, 4)
		if err != nil {
			msg := err.Error()
			rep.Error = &msg
			return fetchedInstanceData{report: rep}
		}
		for _, ep := range episodes {
			epKey := strings.TrimSpace(string(ep.RatingKey))
			showKey := strings.TrimSpace(string(ep.GrandparentRatingKey))
			if showKey == "" {
				showKey = epKey
			}
			showInfo, hasShow := showByKey[showKey]
			title := ep.GrandparentTitle
			if title == "" {
				title = ep.Title
			}
			year := ep.Year
			var genres []string
			hasPoster := false
			if hasShow {
				title = showInfo.Title
				year = showInfo.Year
				genres = showInfo.Genres
				hasPoster = showInfo.HasPoster
			} else if showKey != "" && isSafeThumbPath(ep.GrandparentThumb) {
				out.posterMeta[posterMetaKey(inst.ID, showKey)] = strings.TrimSpace(ep.GrandparentThumb)
				hasPoster = true
			}

			for _, media := range ep.Media {
				for _, part := range media.Part {
					if strings.TrimSpace(part.File) == "" {
						continue
					}
					mapped := mapRemotePath(part.File, inst.PathMappings)
					fname := path.Base(mapped)
					seasonNum := ep.ParentIndex
					epNum := ep.Index
					if seasonNum == 0 && epNum == 0 {
						if s, e, _, ok := extractSeasonEpisode(fname); ok {
							seasonNum = s
							epNum = e
						}
					}
					out.files = append(out.files, PlexFileEntry{
						InstanceID:    inst.ID,
						InstanceName:  inst.Name,
						LibraryTitle:  dir.Title,
						MediaType:     "show",
						RatingKey:     showKey,
						EpisodeKey:    epKey,
						Title:         title,
						Year:          year,
						SeasonNumber:  seasonNum,
						EpisodeNumber: epNum,
						RemotePath:    part.File,
						MappedPath:    mapped,
						Filename:      fname,
						Size:          part.Size,
						Genres:        genres,
						HasPoster:     hasPoster,
					})
				}
			}
		}
	}

	rep.OK = true
	rep.FileCount = len(out.files)
	out.report = rep
	return out
}

func (c *Client) BuildIndex(ctx context.Context, instances []config.PlexInstanceConfig) *Index {
	idx := NewIndex()
	if len(instances) == 0 {
		return idx
	}

	results := make([]fetchedInstanceData, len(instances))
	var wg sync.WaitGroup
	for i, inst := range instances {
		wg.Add(1)
		go func(slot int, cfg config.PlexInstanceConfig) {
			defer wg.Done()
			results[slot] = c.fetchInstanceData(ctx, cfg)
		}(i, inst)
	}
	wg.Wait()

	for _, res := range results {
		idx.Instances = append(idx.Instances, res.report)
		if !res.report.OK {
			continue
		}
		for _, f := range res.files {
			idx.addFileEntry(f)
		}
		idx.Movies = append(idx.Movies, res.movies...)
		idx.Shows = append(idx.Shows, res.shows...)
		for k, v := range res.posterMeta {
			idx.PosterMeta[k] = v
		}
	}

	return idx
}

func (c *Client) tryFetchImage(ctx context.Context, rawURL, token string) ([]byte, string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", false
	}
	if strings.TrimSpace(token) != "" {
		req.Header.Set("X-Plex-Token", strings.TrimSpace(token))
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

// FetchPoster proxies a movie/show poster image from Plex without exposing X-Plex-Token to browsers.
func (c *Client) FetchPoster(ctx context.Context, inst config.PlexInstanceConfig, ratingKey, thumbPath string) ([]byte, string, error) {
	if !inst.Enabled {
		return nil, "", ErrPosterNotFound
	}
	baseURL := strings.TrimRight(strings.TrimSpace(inst.URL), "/")
	if baseURL == "" || !IsSafeRatingKey(ratingKey) {
		return nil, "", ErrPosterNotFound
	}

	resolvedThumb := strings.TrimSpace(thumbPath)
	if !isSafeThumbPath(resolvedThumb) {
		resolvedThumb = fmt.Sprintf("/library/metadata/%s/thumb", strings.TrimSpace(ratingKey))
	}

	candidates := []string{
		fmt.Sprintf("%s/photo/:/transcode?width=300&height=450&minSize=1&upscale=1&url=%s", baseURL, url.QueryEscape(resolvedThumb)),
		baseURL + resolvedThumb,
	}

	for _, u := range candidates {
		if data, ct, ok := c.tryFetchImage(ctx, u, inst.Token); ok {
			return data, ct, nil
		}
	}

	return nil, "", ErrPosterNotFound
}
