package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"seedlink-auditor/internal/actions"
	"seedlink-auditor/internal/arr"
	"seedlink-auditor/internal/config"
	"seedlink-auditor/internal/qbit"
	"seedlink-auditor/internal/scanner"
	"seedlink-auditor/internal/server"
)

type mockTracker struct {
	URL    string `json:"url"`
	Status int    `json:"status"`
	Msg    string `json:"msg"`
}

type mockTorrent struct {
	Hash     string
	Name     string
	SavePath string
	State    string
	Ratio    float64
	Tracker  string
	Trackers []mockTracker
	Files    []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
}

type mockQbitServer struct {
	mu                 sync.Mutex
	torrents           map[string]mockTorrent
	server             *httptest.Server
	localMap           map[string]string // remote prefix -> local root for optional deleteFiles simulation
	omitInlineTrackers bool
	relocateOps        []string
	autoManagement     map[string]bool
	rechecked          []string
}

func newMockQbitServer(t *testing.T, initial []mockTorrent) *mockQbitServer {
	t.Helper()
	mq := &mockQbitServer{
		torrents:       make(map[string]mockTorrent),
		localMap:       make(map[string]string),
		autoManagement: make(map[string]bool),
	}
	for _, mt := range initial {
		mq.torrents[mt.Hash] = mt
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("username") != "admin" || r.FormValue("password") != "secret" {
			http.Error(w, "Fails.", http.StatusOK)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: "test-sid-123", Path: "/"})
		_, _ = w.Write([]byte("Ok."))
	})

	requireAuth := func(w http.ResponseWriter, r *http.Request) bool {
		c, err := r.Cookie("SID")
		if err != nil || c.Value != "test-sid-123" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return false
		}
		return true
	}

	mux.HandleFunc("/api/v2/app/version", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		_, _ = w.Write([]byte("v4.6.7"))
	})

	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		includeTrackers := r.URL.Query().Get("includeTrackers") == "true"
		var filterHashes map[string]struct{}
		if hashesParam := strings.TrimSpace(r.URL.Query().Get("hashes")); hashesParam != "" {
			filterHashes = make(map[string]struct{})
			for _, part := range strings.Split(hashesParam, "|") {
				if p := strings.ToLower(strings.TrimSpace(part)); p != "" {
					filterHashes[p] = struct{}{}
				}
			}
		}
		mq.mu.Lock()
		defer mq.mu.Unlock()
		list := make([]map[string]any, 0, len(mq.torrents))
		for _, mt := range mq.torrents {
			if filterHashes != nil {
				if _, ok := filterHashes[strings.ToLower(strings.TrimSpace(mt.Hash))]; !ok {
					continue
				}
			}
			row := map[string]any{
				"hash":      mt.Hash,
				"name":      mt.Name,
				"save_path": mt.SavePath,
				"state":     mt.State,
				"ratio":     mt.Ratio,
				"tracker":   mt.Tracker,
			}
			if includeTrackers && !mq.omitInlineTrackers && len(mt.Trackers) > 0 {
				row["trackers"] = mt.Trackers
			}
			list = append(list, row)
		}
		_ = json.NewEncoder(w).Encode(list)
	})

	mux.HandleFunc("/api/v2/torrents/trackers", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		h := r.URL.Query().Get("hash")
		mq.mu.Lock()
		mt, ok := mq.torrents[h]
		mq.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if mt.Trackers == nil {
			_ = json.NewEncoder(w).Encode([]mockTracker{})
			return
		}
		_ = json.NewEncoder(w).Encode(mt.Trackers)
	})

	mux.HandleFunc("/api/v2/torrents/files", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		h := r.URL.Query().Get("hash")
		mq.mu.Lock()
		mt, ok := mq.torrents[h]
		mq.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(mt.Files)
	})

	mux.HandleFunc("/api/v2/torrents/delete", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		_ = r.ParseForm()
		hashes := strings.Split(r.FormValue("hashes"), "|")
		deleteFiles := r.FormValue("deleteFiles") == "true"

		mq.mu.Lock()
		defer mq.mu.Unlock()
		for _, h := range hashes {
			mt, ok := mq.torrents[h]
			if !ok {
				continue
			}
			if deleteFiles {
				for _, f := range mt.Files {
					full := filepath.Join(mt.SavePath, filepath.FromSlash(f.Name))
					for rem, loc := range mq.localMap {
						if strings.HasPrefix(mt.SavePath, rem) {
							suffix := strings.TrimPrefix(mt.SavePath, rem)
							full = filepath.Join(loc, filepath.FromSlash(suffix), filepath.FromSlash(f.Name))
							break
						}
					}
					_ = os.Remove(full)
				}
			}
			delete(mq.torrents, h)
		}
		w.WriteHeader(http.StatusOK)
	})

	handleLifecycle := func(op string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !requireAuth(w, r) {
				return
			}
			_ = r.ParseForm()
			mq.mu.Lock()
			for _, h := range strings.Split(r.FormValue("hashes"), "|") {
				if strings.TrimSpace(h) != "" {
					mq.relocateOps = append(mq.relocateOps, op+":"+h)
				}
			}
			mq.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}
	}

	mux.HandleFunc("/api/v2/torrents/stop", handleLifecycle("stop"))
	mux.HandleFunc("/api/v2/torrents/pause", handleLifecycle("pause"))
	mux.HandleFunc("/api/v2/torrents/start", handleLifecycle("start"))
	mux.HandleFunc("/api/v2/torrents/resume", handleLifecycle("resume"))

	mux.HandleFunc("/api/v2/torrents/setAutoManagement", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		_ = r.ParseForm()
		enable := r.FormValue("enable") == "true"
		mq.mu.Lock()
		for _, h := range strings.Split(r.FormValue("hashes"), "|") {
			if strings.TrimSpace(h) != "" {
				mq.autoManagement[h] = enable
				mq.relocateOps = append(mq.relocateOps, "autoMgmt:"+h+":"+r.FormValue("enable"))
			}
		}
		mq.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/api/v2/torrents/setLocation", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		_ = r.ParseForm()
		loc := r.FormValue("location")
		mq.mu.Lock()
		for _, h := range strings.Split(r.FormValue("hashes"), "|") {
			if mt, ok := mq.torrents[h]; ok {
				mt.SavePath = loc
				mq.torrents[h] = mt
				mq.relocateOps = append(mq.relocateOps, "setLocation:"+h+":"+loc)
			}
		}
		mq.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/api/v2/torrents/renameFile", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		_ = r.ParseForm()
		h := r.FormValue("hash")
		oldP := r.FormValue("oldPath")
		newP := r.FormValue("newPath")
		mq.mu.Lock()
		defer mq.mu.Unlock()
		mt, ok := mq.torrents[h]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if oldP != newP {
			for i := range mt.Files {
				if mt.Files[i].Name == newP {
					http.Error(w, "target already exists", http.StatusConflict)
					return
				}
			}
		}
		found := false
		for i := range mt.Files {
			if mt.Files[i].Name == oldP {
				mt.Files[i].Name = newP
				found = true
			}
		}
		if !found {
			http.Error(w, "oldPath not found", http.StatusConflict)
			return
		}
		mq.torrents[h] = mt
		mq.relocateOps = append(mq.relocateOps, "renameFile:"+h+":"+oldP+"->"+newP)
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/api/v2/torrents/recheck", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		_ = r.ParseForm()
		mq.mu.Lock()
		for _, h := range strings.Split(r.FormValue("hashes"), "|") {
			if strings.TrimSpace(h) != "" {
				mq.rechecked = append(mq.rechecked, h)
				mq.relocateOps = append(mq.relocateOps, "recheck:"+h)
			}
		}
		mq.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	mq.server = httptest.NewServer(mux)
	t.Cleanup(mq.server.Close)
	return mq
}

func makePayload(label string, size int) []byte {
	h := sha256.Sum256([]byte(label))
	out := make([]byte, size)
	for i := 0; i < size; i++ {
		out[i] = h[i%len(h)]
	}
	return out
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func apiDoJSON(t *testing.T, ts *httptest.Server, method, path string, body any, dst any) int {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, ts.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if dst != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, dst); err != nil {
			t.Fatalf("failed to unmarshal response (%d): %v\nbody: %s", resp.StatusCode, err, string(raw))
		}
	}
	return resp.StatusCode
}

type fixtureEnv struct {
	ts       *httptest.Server
	folderA  string
	folderB  string
	qbitMain *mockQbitServer
	qbitX    *mockQbitServer
}

func setupFixture(t *testing.T) fixtureEnv {
	t.Helper()
	root := t.TempDir()
	folderA := filepath.Join(root, "folder_a")
	folderB := filepath.Join(root, "folder_b")

	// 1. Healthy hardlink (200 KiB)
	hlSize := 200 * 1024
	hlA := filepath.Join(folderA, "movies", "Dune.mkv")
	hlB := filepath.Join(folderB, "movies", "Dune.mkv")
	writeTestFile(t, hlA, makePayload("dune", hlSize))
	if err := os.MkdirAll(filepath.Dir(hlB), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(hlA, hlB); err != nil {
		t.Fatal(err)
	}

	// 2. Duplicate copy (220 KiB, identical content, different inodes)
	dupSize := 220 * 1024
	dupPayload := makePayload("interstellar", dupSize)
	dupA := filepath.Join(folderA, "movies", "Interstellar.mkv")
	dupB := filepath.Join(folderB, "movies", "Interstellar.mkv")
	writeTestFile(t, dupA, dupPayload)
	writeTestFile(t, dupB, dupPayload)

	// 3. Orphaned cross-seed in Folder B only (250 KiB, seeding in qbitX)
	orpSize := 250 * 1024
	orpB := filepath.Join(folderB, "tv", "Andor.S01E06.mkv")
	writeTestFile(t, orpB, makePayload("andor", orpSize))

	// 4. Untracked file in Folder A (100 KiB)
	untrackedSize := 100 * 1024
	untrackedA := filepath.Join(folderA, "misc", "Leftover.mkv")
	writeTestFile(t, untrackedA, makePayload("leftover", untrackedSize))

	// 5. Intra-folder collision test: sample.mkv in two subfolders in Folder A, one hardlinked to Folder B
	s1Size := 64 * 1024
	s2Size := 96 * 1024
	s1A := filepath.Join(folderA, "Movie1", "sample.mkv")
	s1B := filepath.Join(folderB, "Movie1", "sample.mkv")
	s2A := filepath.Join(folderA, "Movie2", "sample.mkv")
	writeTestFile(t, s1A, makePayload("sample1", s1Size))
	if err := os.MkdirAll(filepath.Dir(s1B), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(s1A, s1B); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, s2A, makePayload("sample2", s2Size))

	qbitMain := newMockQbitServer(t, []mockTorrent{
		{
			Hash:     "h_main_dune",
			Name:     "Dune",
			SavePath: "/downloads",
			State:    "uploading",
			Ratio:    2.5,
			Tracker:  "https://alpha.tracker.org/announce",
			Trackers: []mockTracker{{URL: "https://alpha.tracker.org/announce", Status: 2, Msg: ""}},
			Files: []struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
			}{{Name: "movies/Dune.mkv", Size: int64(hlSize)}},
		},
		{
			Hash:     "h_main_interstellar",
			Name:     "Interstellar",
			SavePath: "/downloads",
			State:    "stalledUP",
			Ratio:    1.2,
			Tracker:  "https://alpha.tracker.org/announce",
			Trackers: []mockTracker{{URL: "https://alpha.tracker.org/announce", Status: 2, Msg: ""}},
			Files: []struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
			}{{Name: "movies/Interstellar.mkv", Size: int64(dupSize)}},
		},
	})
	qbitMain.localMap["/downloads"] = folderA

	qbitX := newMockQbitServer(t, []mockTorrent{
		{
			Hash:     "h_x_dune",
			Name:     "Dune [XS]",
			SavePath: qbit.NormalizeSlashPath(folderB),
			State:    "stalledUP",
			Ratio:    0.9,
			Tracker:  "https://beta.tracker.org/announce",
			Trackers: []mockTracker{{URL: "https://beta.tracker.org/announce", Status: 4, Msg: "Torrent not registered with this tracker"}},
			Files: []struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
			}{{Name: "movies/Dune.mkv", Size: int64(hlSize)}},
		},
		{
			Hash:     "h_x_andor",
			Name:     "Andor.S01E06",
			SavePath: qbit.NormalizeSlashPath(folderB),
			State:    "uploading",
			Ratio:    3.1,
			Tracker:  "https://beta.tracker.org/announce",
			Trackers: []mockTracker{{URL: "https://beta.tracker.org/announce", Status: 2, Msg: ""}},
			Files: []struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
			}{{Name: "tv/Andor.S01E06.mkv", Size: int64(orpSize)}},
		},
	})

	cfgMgr, err := config.NewManager(filepath.Join(root, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = cfgMgr.Update(config.AppSettings{
		ReadOnlyMode: true,
		Folders: []config.FolderConfig{
			{ID: "fa", Label: "Folder A", Path: folderA, Role: "primary"},
			{ID: "fb", Label: "Folder B", Path: folderB, Role: "secondary"},
		},
		QbitInstances: []config.QbitInstanceConfig{
			{
				ID:       "qm",
				Name:     "qBit Main",
				URL:      qbitMain.server.URL,
				Username: "admin",
				Password: "secret",
				Enabled:  true,
				PathMappings: []config.PathMapping{
					{RemotePrefix: "/downloads", LocalPrefix: qbit.NormalizeSlashPath(folderA)},
				},
			},
			{
				ID:       "qx",
				Name:     "qBit CrossSeed",
				URL:      qbitX.server.URL,
				Username: "admin",
				Password: "secret",
				Enabled:  true,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	srv := server.New(cfgMgr, qbit.NewClient(5*time.Second), arr.NewClient(5*time.Second), nil)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	return fixtureEnv{
		ts:       ts,
		folderA:  folderA,
		folderB:  folderB,
		qbitMain: qbitMain,
		qbitX:    qbitX,
	}
}

func TestScanCorrelationAndDisambiguation(t *testing.T) {
	env := setupFixture(t)

	var scan scanner.ScanResult
	status := apiDoJSON(t, env.ts, http.MethodPost, "/api/scan", nil, &scan)
	if status != http.StatusOK {
		t.Fatalf("expected 200 from /api/scan, got %d", status)
	}

	if scan.Summary.HardlinkedCount != 2 {
		t.Fatalf("expected 2 hardlinked groups (Dune + Movie1/sample.mkv), got %d", scan.Summary.HardlinkedCount)
	}
	if scan.Summary.DuplicateCopyCount != 1 {
		t.Fatalf("expected 1 duplicate copy group, got %d", scan.Summary.DuplicateCopyCount)
	}
	if scan.Summary.OrphanedInBCount != 1 {
		t.Fatalf("expected 1 orphaned_in_b group, got %d", scan.Summary.OrphanedInBCount)
	}
	if scan.Summary.OnlyInACount != 2 {
		t.Fatalf("expected 2 only_in_a groups (Leftover + Movie2/sample.mkv), got %d", scan.Summary.OnlyInACount)
	}

	var dune, andor, dup *scanner.ComparisonGroup
	for i := range scan.Groups {
		g := &scan.Groups[i]
		switch g.Filename {
		case "Dune.mkv":
			dune = g
		case "Andor.S01E06.mkv":
			andor = g
		case "Interstellar.mkv":
			dup = g
		}
	}

	if dune == nil || dune.Status != "HARDLINKED" || dune.SeedingSummary != "SEEDING" || len(dune.Torrents) != 2 {
		t.Fatalf("unexpected Dune group state: %+v", dune)
	}
	if !dune.HasUnregistered || !dune.HasTrackerIssue {
		t.Fatalf("expected Dune group to flag unregistered cross-seed tracker, got %+v", dune)
	}
	if scan.Summary.UnregisteredCount != 1 || scan.Summary.TrackerErrorCount != 1 {
		t.Fatalf("expected UnregisteredCount=1 TrackerErrorCount=1, got %d / %d", scan.Summary.UnregisteredCount, scan.Summary.TrackerErrorCount)
	}
	var xsDune *scanner.TorrentMatch
	for i := range dune.Torrents {
		if dune.Torrents[i].TorrentHash == "h_x_dune" {
			xsDune = &dune.Torrents[i]
		}
	}
	if xsDune == nil || xsDune.TrackerHealth != "UNREGISTERED" || xsDune.TrackerMsg != "Torrent not registered with this tracker" {
		t.Fatalf("expected unregistered tracker details on h_x_dune, got %+v", xsDune)
	}
	if andor == nil || andor.Status != "ORPHANED_IN_B" || !andor.SoleCopyOnDisk || andor.SeedingSummary != "SEEDING" {
		t.Fatalf("unexpected Andor group state: %+v", andor)
	}
	if dup == nil || dup.Status != "DUPLICATE_COPY" || !dup.CanHardlink || len(dup.HardlinkPairs) != 1 {
		t.Fatalf("unexpected Interstellar duplicate group state: %+v", dup)
	}
}

func TestTrackerErrorAndFallbackEndpoint(t *testing.T) {
	root := t.TempDir()
	folderA := filepath.Join(root, "folder_a")
	folderB := filepath.Join(root, "folder_b")
	writeTestFile(t, filepath.Join(folderB, "unreg.mkv"), makePayload("unreg", 32*1024))
	writeTestFile(t, filepath.Join(folderB, "passkey.mkv"), makePayload("passkey", 32*1024))
	writeTestFile(t, filepath.Join(folderA, "localerr.mkv"), makePayload("localerr", 32*1024))

	mq := newMockQbitServer(t, []mockTorrent{
		{
			Hash:     "h1",
			Name:     "unreg",
			SavePath: qbit.NormalizeSlashPath(folderB),
			State:    "stalledUP",
			Tracker:  "https://t.example/announce",
			Trackers: []mockTracker{{URL: "https://t.example/announce", Status: 4, Msg: "Unregistered torrent"}},
			Files: []struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
			}{{Name: "unreg.mkv", Size: 32 * 1024}},
		},
		{
			Hash:     "h2",
			Name:     "passkey",
			SavePath: qbit.NormalizeSlashPath(folderB),
			State:    "stalledUP",
			Tracker:  "https://t.example/announce",
			Trackers: []mockTracker{{URL: "https://t.example/announce", Status: 4, Msg: "Passkey does not exist"}},
			Files: []struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
			}{{Name: "passkey.mkv", Size: 32 * 1024}},
		},
		{
			Hash:     "h3",
			Name:     "localerr",
			SavePath: qbit.NormalizeSlashPath(folderA),
			State:    "missingFiles",
			Tracker:  "https://t.example/announce",
			Files: []struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
			}{{Name: "localerr.mkv", Size: 32 * 1024}},
		},
	})
	mq.omitInlineTrackers = true // force fallback to /api/v2/torrents/trackers

	cfgMgr, err := config.NewManager(filepath.Join(root, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = cfgMgr.Update(config.AppSettings{
		ReadOnlyMode: true,
		Folders: []config.FolderConfig{
			{ID: "fa", Label: "A", Path: folderA, Role: "primary"},
			{ID: "fb", Label: "B", Path: folderB, Role: "secondary"},
		},
		QbitInstances: []config.QbitInstanceConfig{
			{ID: "q1", Name: "q1", URL: mq.server.URL, Username: "admin", Password: "secret", Enabled: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(server.New(cfgMgr, qbit.NewClient(5*time.Second), arr.NewClient(5*time.Second), nil))
	t.Cleanup(ts.Close)

	var scan scanner.ScanResult
	if code := apiDoJSON(t, ts, http.MethodPost, "/api/scan", nil, &scan); code != http.StatusOK {
		t.Fatalf("expected 200 from /api/scan, got %d", code)
	}
	if scan.Summary.UnregisteredCount != 1 || scan.Summary.TrackerErrorCount != 3 {
		t.Fatalf("expected UnregisteredCount=1 TrackerErrorCount=3, got %d / %d", scan.Summary.UnregisteredCount, scan.Summary.TrackerErrorCount)
	}

	byName := make(map[string]scanner.ComparisonGroup)
	for _, g := range scan.Groups {
		byName[g.Filename] = g
	}
	if byName["unreg.mkv"].SeedingSummary != "UNREGISTERED" || byName["unreg.mkv"].Torrents[0].TrackerHealth != "UNREGISTERED" {
		t.Fatalf("unexpected unreg.mkv state: %+v", byName["unreg.mkv"])
	}
	if byName["passkey.mkv"].SeedingSummary != "ERROR" || byName["passkey.mkv"].Torrents[0].TrackerHealth != "TRACKER_ERROR" {
		t.Fatalf("expected passkey message to be TRACKER_ERROR (not UNREGISTERED), got %+v", byName["passkey.mkv"])
	}
	if byName["localerr.mkv"].SeedingSummary != "ERROR" || byName["localerr.mkv"].Torrents[0].TrackerHealth != "LOCAL_ERROR" {
		t.Fatalf("expected missingFiles torrent to be LOCAL_ERROR, got %+v", byName["localerr.mkv"])
	}
}

func TestQbitConnectionEndpoint(t *testing.T) {
	env := setupFixture(t)

	var okRes qbit.TestResult
	code := apiDoJSON(t, env.ts, http.MethodPost, "/api/qbit/test", config.QbitInstanceConfig{
		ID:       "qm",
		Name:     "qBit Main",
		URL:      env.qbitMain.server.URL,
		Username: "admin",
		Password: "secret",
		Enabled:  true,
	}, &okRes)
	if code != http.StatusOK || !okRes.OK || okRes.Version != "v4.6.7" || okRes.TorrentCount != 2 {
		t.Fatalf("unexpected qBit test result: code=%d res=%+v", code, okRes)
	}

	var badRes qbit.TestResult
	code = apiDoJSON(t, env.ts, http.MethodPost, "/api/qbit/test", config.QbitInstanceConfig{
		ID:       "qm",
		Name:     "qBit Main",
		URL:      env.qbitMain.server.URL,
		Username: "admin",
		Password: "wrong",
		Enabled:  true,
	}, &badRes)
	if code != http.StatusOK || badRes.OK {
		t.Fatalf("expected failed auth on wrong password, got code=%d res=%+v", code, badRes)
	}
}

func TestReadOnlyBlocksMutationsButAllowsDryRun(t *testing.T) {
	env := setupFixture(t)

	var scan scanner.ScanResult
	apiDoJSON(t, env.ts, http.MethodPost, "/api/scan", nil, &scan)

	var dup, orphan, untracked scanner.ComparisonGroup
	for _, g := range scan.Groups {
		if g.Status == "DUPLICATE_COPY" {
			dup = g
		} else if g.Status == "ORPHANED_IN_B" {
			orphan = g
		} else if g.Filename == "Leftover.mkv" {
			untracked = g
		}
	}

	var dryRes actions.ConvertHardlinksResult
	code := apiDoJSON(t, env.ts, http.MethodPost, "/api/actions/convert-hardlinks", map[string]any{
		"pairs":   dup.HardlinkPairs,
		"dry_run": true,
	}, &dryRes)
	if code != http.StatusOK || dryRes.ConvertedCount != 1 {
		t.Fatalf("expected dry_run convert to succeed, got code=%d res=%+v", code, dryRes)
	}

	// Non-dry-run mutations must be blocked with HTTP 403
	if c := apiDoJSON(t, env.ts, http.MethodPost, "/api/actions/convert-hardlinks", map[string]any{
		"pairs":   dup.HardlinkPairs,
		"dry_run": false,
	}, nil); c != http.StatusForbidden {
		t.Fatalf("expected 403 for convert-hardlinks in read-only mode, got %d", c)
	}

	if c := apiDoJSON(t, env.ts, http.MethodPost, "/api/actions/delete-torrents", map[string]any{
		"items": []map[string]any{{
			"instance_id":    orphan.Torrents[0].InstanceID,
			"torrent_hash":   orphan.Torrents[0].TorrentHash,
			"expected_paths": orphan.Torrents[0].MatchedPaths,
		}},
		"delete_files": true,
		"dry_run":      false,
	}, nil); c != http.StatusForbidden {
		t.Fatalf("expected 403 for delete-torrents in read-only mode, got %d", c)
	}

	if c := apiDoJSON(t, env.ts, http.MethodPost, "/api/actions/delete-files", map[string]any{
		"file_paths": untracked.AllPaths,
		"dry_run":    false,
	}, nil); c != http.StatusForbidden {
		t.Fatalf("expected 403 for delete-files in read-only mode, got %d", c)
	}
}

func TestConvertHardlinksAndContentMismatchGuard(t *testing.T) {
	env := setupFixture(t)

	// Unlock write mode
	apiDoJSON(t, env.ts, http.MethodPatch, "/api/config/read-only", map[string]bool{"read_only_mode": false}, nil)

	var scan scanner.ScanResult
	apiDoJSON(t, env.ts, http.MethodPost, "/api/scan", nil, &scan)

	var dup scanner.ComparisonGroup
	for _, g := range scan.Groups {
		if g.Status == "DUPLICATE_COPY" {
			dup = g
			break
		}
	}

	var convRes actions.ConvertHardlinksResult
	code := apiDoJSON(t, env.ts, http.MethodPost, "/api/actions/convert-hardlinks", map[string]any{
		"pairs":   dup.HardlinkPairs,
		"dry_run": false,
	}, &convRes)
	if code != http.StatusOK || convRes.ConvertedCount != 1 || convRes.BytesReclaimed != dup.Size || len(convRes.Errors) != 0 {
		t.Fatalf("unexpected conversion result: code=%d res=%+v", code, convRes)
	}

	srcInfo, err := os.Stat(dup.HardlinkPairs[0].SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	tgtInfo, err := os.Stat(dup.HardlinkPairs[0].TargetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(srcInfo, tgtInfo) {
		t.Fatalf("expected os.SameFile to be true after hardlink conversion")
	}
	st, err := scanner.StatPath(dup.HardlinkPairs[0].SourcePath, srcInfo)
	if err != nil || st.Nlink != 2 {
		t.Fatalf("expected Nlink==2 after hardlink conversion, got %d (err=%v)", st.Nlink, err)
	}

	// Content mismatch protection test
	fakeA := filepath.Join(env.folderA, "mismatch.mkv")
	fakeB := filepath.Join(env.folderB, "mismatch.mkv")
	writeTestFile(t, fakeA, bytes.Repeat([]byte("A"), 200*1024))
	writeTestFile(t, fakeB, bytes.Repeat([]byte("B"), 200*1024))

	var mismatchRes actions.ConvertHardlinksResult
	code = apiDoJSON(t, env.ts, http.MethodPost, "/api/actions/convert-hardlinks", map[string]any{
		"pairs":   []map[string]string{{"source_path": fakeA, "target_path": fakeB}},
		"dry_run": false,
	}, &mismatchRes)
	if code != http.StatusOK || mismatchRes.ConvertedCount != 0 || len(mismatchRes.Errors) != 1 {
		t.Fatalf("expected mismatch error, got code=%d res=%+v", code, mismatchRes)
	}
	if !strings.Contains(mismatchRes.Errors[0].Error, "Content mismatch") {
		t.Fatalf("expected Content mismatch error message, got %q", mismatchRes.Errors[0].Error)
	}
}

func TestDeleteOrphanedTorrentsAndUntrackedFiles(t *testing.T) {
	env := setupFixture(t)
	apiDoJSON(t, env.ts, http.MethodPatch, "/api/config/read-only", map[string]bool{"read_only_mode": false}, nil)

	var scan scanner.ScanResult
	apiDoJSON(t, env.ts, http.MethodPost, "/api/scan", nil, &scan)

	var orphan, untracked scanner.ComparisonGroup
	for _, g := range scan.Groups {
		if g.Status == "ORPHANED_IN_B" {
			orphan = g
		} else if g.Filename == "Leftover.mkv" {
			untracked = g
		}
	}

	// Delete orphaned torrent + data via qBittorrent API
	var delTorRes actions.DeleteTorrentsResult
	code := apiDoJSON(t, env.ts, http.MethodPost, "/api/actions/delete-torrents", map[string]any{
		"items": []map[string]any{{
			"instance_id":    orphan.Torrents[0].InstanceID,
			"torrent_hash":   orphan.Torrents[0].TorrentHash,
			"expected_paths": orphan.Torrents[0].MatchedPaths,
		}},
		"delete_files": true,
		"dry_run":      false,
	}, &delTorRes)
	if code != http.StatusOK || delTorRes.DeletedCount != 1 || delTorRes.BytesReclaimed != orphan.Size {
		t.Fatalf("unexpected delete-torrents result: code=%d res=%+v", code, delTorRes)
	}
	if _, err := os.Stat(orphan.FolderBFiles[0].AbsPath); !os.IsNotExist(err) {
		t.Fatalf("expected orphaned torrent file to be deleted on disk")
	}

	// Delete untracked file + path traversal guard
	outside := filepath.Join(filepath.Dir(env.folderA), "outside.txt")
	writeTestFile(t, outside, []byte("keep me"))

	var delFileRes actions.DeleteFilesResult
	code = apiDoJSON(t, env.ts, http.MethodPost, "/api/actions/delete-files", map[string]any{
		"file_paths":         []string{untracked.AllPaths[0], outside},
		"cleanup_empty_dirs": true,
		"dry_run":            false,
	}, &delFileRes)
	if code != http.StatusOK || delFileRes.DeletedCount != 1 || len(delFileRes.Errors) != 1 {
		t.Fatalf("unexpected delete-files result: code=%d res=%+v", code, delFileRes)
	}
	if _, err := os.Stat(untracked.AllPaths[0]); !os.IsNotExist(err) {
		t.Fatalf("expected untracked file to be deleted")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("expected outside file to remain untouched")
	}
}

func TestExportAndBrowseEndpoints(t *testing.T) {
	env := setupFixture(t)
	apiDoJSON(t, env.ts, http.MethodPost, "/api/scan", nil, nil)

	// Export CSV
	csvResp, err := http.Get(env.ts.URL + "/api/export?format=csv")
	if err != nil {
		t.Fatal(err)
	}
	defer csvResp.Body.Close()
	if csvResp.StatusCode != http.StatusOK || !strings.Contains(csvResp.Header.Get("Content-Type"), "text/csv") {
		t.Fatalf("unexpected CSV response: %d %s", csvResp.StatusCode, csvResp.Header.Get("Content-Type"))
	}
	records, err := csv.NewReader(csvResp.Body).ReadAll()
	if err != nil || len(records) != 7 { // header + 6 groups
		t.Fatalf("expected 7 CSV rows, got %d (err=%v)", len(records), err)
	}

	// Export JSON
	jsonResp, err := http.Get(env.ts.URL + "/api/export?format=json")
	if err != nil {
		t.Fatal(err)
	}
	defer jsonResp.Body.Close()
	var exported scanner.ScanResult
	if err := json.NewDecoder(jsonResp.Body).Decode(&exported); err != nil {
		t.Fatal(err)
	}
	if exported.Summary.TotalGroups != 6 {
		t.Fatalf("expected 6 groups in exported JSON, got %d", exported.Summary.TotalGroups)
	}

	// Browse filesystem endpoint
	browseResp, err := http.Get(env.ts.URL + "/api/fs/browse?path=" + filepath.Dir(env.folderA))
	if err != nil {
		t.Fatal(err)
	}
	defer browseResp.Body.Close()
	var browseBody struct {
		Directories []struct {
			Name string `json:"name"`
		} `json:"directories"`
	}
	if err := json.NewDecoder(browseResp.Body).Decode(&browseBody); err != nil {
		t.Fatal(err)
	}
	foundA := false
	for _, d := range browseBody.Directories {
		if d.Name == "folder_a" {
			foundA = true
			break
		}
	}
	if !foundA {
		t.Fatalf("expected folder_a in browse response: %+v", browseBody)
	}
}

type mockArrServer struct {
	mu              sync.Mutex
	commands        []map[string]any
	commandStatuses map[int]string
	server          *httptest.Server
	movies          []map[string]any
	series          []map[string]any
	episodeFiles    map[string][]map[string]any
	wanted          []map[string]any
	queue           []map[string]any
	manualItems     []map[string]any
	posters         map[int][]byte
}

func newMockArrServer(t *testing.T, appType string) *mockArrServer {
	t.Helper()
	ma := &mockArrServer{
		commands:        make([]map[string]any, 0),
		commandStatuses: make(map[int]string),
		episodeFiles:    make(map[string][]map[string]any),
		posters:         make(map[int][]byte),
	}

	mux := http.NewServeMux()
	requireKey := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("X-Api-Key") != "arr-secret-key" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return false
		}
		return true
	}

	mux.HandleFunc("/api/v3/system/status", func(w http.ResponseWriter, r *http.Request) {
		if !requireKey(w, r) {
			return
		}
		name := "Radarr"
		ver := "5.10.0"
		if appType == "sonarr" {
			name = "Sonarr"
			ver = "4.0.9"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"appName": name, "version": ver})
	})

	mux.HandleFunc("/api/v3/movie", func(w http.ResponseWriter, r *http.Request) {
		if !requireKey(w, r) {
			return
		}
		ma.mu.Lock()
		defer ma.mu.Unlock()
		_ = json.NewEncoder(w).Encode(ma.movies)
	})

	mux.HandleFunc("GET /api/v3/movie/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !requireKey(w, r) {
			return
		}
		idVal, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		ma.mu.Lock()
		defer ma.mu.Unlock()
		for _, m := range ma.movies {
			if id, ok := m["id"].(int); ok && id == idVal {
				_ = json.NewEncoder(w).Encode(m)
				return
			}
		}
		http.NotFound(w, r)
	})

	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, r *http.Request) {
		if !requireKey(w, r) {
			return
		}
		ma.mu.Lock()
		defer ma.mu.Unlock()
		_ = json.NewEncoder(w).Encode(ma.series)
	})

	mux.HandleFunc("GET /api/v3/series/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !requireKey(w, r) {
			return
		}
		idVal, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		ma.mu.Lock()
		defer ma.mu.Unlock()
		for _, s := range ma.series {
			if id, ok := s["id"].(int); ok && id == idVal {
				_ = json.NewEncoder(w).Encode(s)
				return
			}
		}
		http.NotFound(w, r)
	})

	mux.HandleFunc("/api/v3/episodefile", func(w http.ResponseWriter, r *http.Request) {
		if !requireKey(w, r) {
			return
		}
		sid := r.URL.Query().Get("seriesId")
		ma.mu.Lock()
		files := ma.episodeFiles[sid]
		ma.mu.Unlock()
		if files == nil {
			files = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(files)
	})

	mux.HandleFunc("/api/v3/wanted/missing", func(w http.ResponseWriter, r *http.Request) {
		if !requireKey(w, r) {
			return
		}
		ma.mu.Lock()
		recs := ma.wanted
		ma.mu.Unlock()
		if recs == nil {
			recs = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"page":         1,
			"pageSize":     1000,
			"totalRecords": len(recs),
			"records":      recs,
		})
	})

	mux.HandleFunc("/api/v3/queue", func(w http.ResponseWriter, r *http.Request) {
		if !requireKey(w, r) {
			return
		}
		ma.mu.Lock()
		recs := ma.queue
		ma.mu.Unlock()
		if recs == nil {
			recs = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"page":         1,
			"pageSize":     1000,
			"totalRecords": len(recs),
			"records":      recs,
		})
	})

	mux.HandleFunc("/api/v3/manualimport", func(w http.ResponseWriter, r *http.Request) {
		if !requireKey(w, r) {
			return
		}
		ma.mu.Lock()
		items := ma.manualItems
		ma.mu.Unlock()
		if items == nil {
			items = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(items)
	})

	mux.HandleFunc("/api/v3/command", func(w http.ResponseWriter, r *http.Request) {
		if !requireKey(w, r) {
			return
		}
		var cmd map[string]any
		if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ma.mu.Lock()
		ma.commands = append(ma.commands, cmd)
		cmdID := len(ma.commands)
		ma.commandStatuses[cmdID] = "queued"
		ma.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     cmdID,
			"name":   cmd["name"],
			"status": "queued",
		})
	})

	mux.HandleFunc("GET /api/v3/command/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !requireKey(w, r) {
			return
		}
		cmdID, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		ma.mu.Lock()
		defer ma.mu.Unlock()
		if cmdID <= 0 || cmdID > len(ma.commands) {
			http.NotFound(w, r)
			return
		}
		status := ma.commandStatuses[cmdID]
		if status == "" {
			status = "queued"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     cmdID,
			"name":   ma.commands[cmdID-1]["name"],
			"status": status,
		})
	})

	mux.HandleFunc("GET /api/v3/mediacover/{id}/{file}", func(w http.ResponseWriter, r *http.Request) {
		if !requireKey(w, r) {
			return
		}
		idVal, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		ma.mu.Lock()
		img := ma.posters[idVal]
		ma.mu.Unlock()
		if len(img) == 0 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(img)
	})

	ma.server = httptest.NewServer(mux)
	t.Cleanup(ma.server.Close)
	return ma
}

func TestRadarrAndSonarrIntegration(t *testing.T) {
	root := t.TempDir()
	plexDir := filepath.Join(root, "plex")
	dlDir := filepath.Join(root, "downloads")

	// 1. Healthy Radarr movie (hardlinked Plex <-> Downloads)
	impSize := 64 * 1024
	impPlex := filepath.Join(plexDir, "movies", "Inception (2010)", "Inception (2010).mkv")
	impDl := filepath.Join(dlDir, "movies", "Inception.2010.1080p.mkv")
	writeTestFile(t, impPlex, makePayload("inception", impSize))
	if err := os.MkdirAll(filepath.Dir(impDl), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(impPlex, impDl); err != nil {
		t.Fatal(err)
	}

	// 2. Healthy Sonarr episode (hardlinked Plex <-> Downloads)
	epSize := 48 * 1024
	epPlex := filepath.Join(plexDir, "tv", "Severance", "Season 01", "Severance - S01E01.mkv")
	epDl := filepath.Join(dlDir, "tv", "Severance.S01E01.1080p.mkv")
	writeTestFile(t, epPlex, makePayload("sev1", epSize))
	if err := os.MkdirAll(filepath.Dir(epDl), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(epPlex, epDl); err != nil {
		t.Fatal(err)
	}

	// 3. Wanted Radarr movie stuck in Downloads + qBit (NOT in Plex)
	wantedSize := 80 * 1024
	wantedDl := filepath.Join(dlDir, "movies", "The.Matrix.1999.2160p.mkv")
	writeTestFile(t, wantedDl, makePayload("matrix", wantedSize))

	// 4. Queue-blocked Radarr movie in Downloads + qBit
	blockedSize := 72 * 1024
	blockedDl := filepath.Join(dlDir, "movies", "Heat.1995.BluRay.mkv")
	writeTestFile(t, blockedDl, makePayload("heat", blockedSize))

	// 5. Unlinked movie file sitting inside Radarr's movie directory in Plex, but hasFile=false in Radarr
	unlinkedSize := 56 * 1024
	unlinkedPlex := filepath.Join(plexDir, "movies", "Memento (2000)", "Memento (2000).mkv")
	writeTestFile(t, unlinkedPlex, makePayload("memento", unlinkedSize))

	// 6. Replaced/upgraded orphaned download in Downloads (Radarr already has another file for Blade Runner 2049)
	replacedSize := 60 * 1024
	replacedDl := filepath.Join(dlDir, "movies", "Blade.Runner.2049.2017.720p.mkv")
	writeTestFile(t, replacedDl, makePayload("br2049", replacedSize))

	// 7. Wanted Sonarr episode in Downloads (NOT in Plex)
	wantedEpSize := 40 * 1024
	wantedEpDl := filepath.Join(dlDir, "tv", "Severance.S01E02.1080p.mkv")
	writeTestFile(t, wantedEpDl, makePayload("sev2", wantedEpSize))

	// 8. Replaced/older Sonarr episode in Downloads (Sonarr already has S01E01)
	replacedEpSize := 36 * 1024
	replacedEpDl := filepath.Join(dlDir, "tv", "Severance.S01E01.720p.mkv")
	writeTestFile(t, replacedEpDl, makePayload("sev1-old", replacedEpSize))

	mq := newMockQbitServer(t, []mockTorrent{
		{
			Hash:     "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111",
			Name:     "The.Matrix.1999.2160p",
			SavePath: qbit.NormalizeSlashPath(dlDir),
			State:    "uploading",
			Ratio:    1.4,
			Tracker:  "https://tracker.org/announce",
			Trackers: []mockTracker{{URL: "https://tracker.org/announce", Status: 2}},
			Files: []struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
			}{{Name: "movies/The.Matrix.1999.2160p.mkv", Size: int64(wantedSize)}},
		},
		{
			Hash:     "bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222",
			Name:     "Heat.1995.BluRay",
			SavePath: qbit.NormalizeSlashPath(dlDir),
			State:    "stalledUP",
			Ratio:    0.5,
			Tracker:  "https://tracker.org/announce",
			Trackers: []mockTracker{{URL: "https://tracker.org/announce", Status: 2}},
			Files: []struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
			}{{Name: "movies/Heat.1995.BluRay.mkv", Size: int64(blockedSize)}},
		},
	})

	radarrMock := newMockArrServer(t, "radarr")
	radarrMock.movies = []map[string]any{
		{
			"id":        10,
			"title":     "Inception",
			"year":      2010,
			"path":      "/media/movies/Inception (2010)",
			"hasFile":   true,
			"monitored": true,
			"movieFile": map[string]any{
				"id":           101,
				"path":         "/media/movies/Inception (2010)/Inception (2010).mkv",
				"relativePath": "Inception (2010).mkv",
				"size":         impSize,
			},
		},
		{
			"id":        20,
			"title":     "The Matrix",
			"year":      1999,
			"path":      "/media/movies/The Matrix (1999)",
			"hasFile":   false,
			"monitored": true,
		},
		{
			"id":        30,
			"title":     "Heat",
			"year":      1995,
			"path":      "/media/movies/Heat (1995)",
			"hasFile":   false,
			"monitored": true,
		},
		{
			"id":        40,
			"title":     "Memento",
			"year":      2000,
			"path":      "/media/movies/Memento (2000)",
			"hasFile":   false,
			"monitored": true,
		},
		{
			"id":        50,
			"title":     "Blade Runner 2049",
			"year":      2017,
			"path":      "/media/movies/Blade Runner 2049 (2017)",
			"hasFile":   true,
			"monitored": true,
			"movieFile": map[string]any{
				"id":           501,
				"path":         "/media/movies/Blade Runner 2049 (2017)/Blade Runner 2049 (2017) 2160p.mkv",
				"relativePath": "Blade Runner 2049 (2017) 2160p.mkv",
				"size":         999999,
			},
		},
	}
	radarrMock.queue = []map[string]any{
		{
			"id":                    901,
			"movieId":               30,
			"title":                 "Heat.1995.BluRay",
			"status":                "completed",
			"trackedDownloadStatus": "warning",
			"trackedDownloadState":  "importBlocked",
			"downloadId":            "BBBB2222BBBB2222BBBB2222BBBB2222BBBB2222",
			"errorMessage":          "Sample file or upgrade blocked",
		},
	}
	radarrMock.manualItems = []map[string]any{
		{
			"id":         1,
			"path":       "/arr_downloads/movies/The.Matrix.1999.2160p.mkv",
			"name":       "The.Matrix.1999.2160p.mkv",
			"size":       wantedSize,
			"movie":      map[string]any{"id": 20, "title": "The Matrix", "year": 1999},
			"quality":    map[string]any{"quality": map[string]any{"id": 19, "name": "Bluray-2160p"}},
			"languages":  []map[string]any{{"id": 1, "name": "English"}},
			"rejections": []map[string]any{{"reason": "Not an upgrade", "type": "permanent"}},
		},
	}

	sonarrMock := newMockArrServer(t, "sonarr")
	sonarrMock.series = []map[string]any{
		{
			"id":        70,
			"title":     "Severance",
			"year":      2022,
			"path":      "/media/tv/Severance",
			"monitored": true,
		},
	}
	sonarrMock.episodeFiles["70"] = []map[string]any{
		{
			"id":           701,
			"seriesId":     70,
			"seasonNumber": 1,
			"path":         "/media/tv/Severance/Season 01/Severance - S01E01.mkv",
			"relativePath": "Season 01/Severance - S01E01.mkv",
			"size":         epSize,
		},
	}
	sonarrMock.wanted = []map[string]any{
		{
			"id":            7002,
			"seriesId":      70,
			"seasonNumber":  1,
			"episodeNumber": 2,
			"title":         "Half Loop",
			"monitored":     true,
			"hasFile":       false,
		},
	}

	cfgMgr, err := config.NewManager(filepath.Join(root, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = cfgMgr.Update(config.AppSettings{
		ReadOnlyMode: true,
		Folders: []config.FolderConfig{
			{ID: "plex", Label: "Plex", Path: plexDir, Role: "primary"},
			{ID: "dl", Label: "Downloads", Path: dlDir, Role: "secondary"},
		},
		QbitInstances: []config.QbitInstanceConfig{
			{ID: "q1", Name: "qBit", URL: mq.server.URL, Username: "admin", Password: "secret", Enabled: true},
		},
		ArrInstances: []config.ArrInstanceConfig{
			{
				ID:      "rad1",
				Name:    "Radarr",
				Type:    "radarr",
				URL:     radarrMock.server.URL,
				APIKey:  "arr-secret-key",
				Enabled: true,
				PathMappings: []config.PathMapping{
					{RemotePrefix: "/media", LocalPrefix: qbit.NormalizeSlashPath(plexDir)},
					{RemotePrefix: "/arr_downloads", LocalPrefix: qbit.NormalizeSlashPath(dlDir)},
				},
			},
			{
				ID:      "son1",
				Name:    "Sonarr",
				Type:    "sonarr",
				URL:     sonarrMock.server.URL,
				APIKey:  "arr-secret-key",
				Enabled: true,
				PathMappings: []config.PathMapping{
					{RemotePrefix: "/media", LocalPrefix: qbit.NormalizeSlashPath(plexDir)},
					{RemotePrefix: "/arr_downloads", LocalPrefix: qbit.NormalizeSlashPath(dlDir)},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(server.New(cfgMgr, qbit.NewClient(5*time.Second), arr.NewClient(5*time.Second), nil))
	t.Cleanup(ts.Close)

	// 1. Test /api/arr/test (valid and invalid key)
	var testOK arr.TestResult
	code := apiDoJSON(t, ts, http.MethodPost, "/api/arr/test", config.ArrInstanceConfig{
		ID: "rad1", Name: "Radarr", Type: "radarr", URL: radarrMock.server.URL, APIKey: "arr-secret-key", Enabled: true,
	}, &testOK)
	if code != http.StatusOK || !testOK.OK || testOK.AppName != "Radarr" || testOK.ItemCount != 5 {
		t.Fatalf("unexpected /api/arr/test valid response: code=%d res=%+v", code, testOK)
	}

	var testBad arr.TestResult
	code = apiDoJSON(t, ts, http.MethodPost, "/api/arr/test", config.ArrInstanceConfig{
		ID: "rad1", Name: "Radarr", Type: "radarr", URL: radarrMock.server.URL, APIKey: "wrong", Enabled: true,
	}, &testBad)
	if code != http.StatusOK || testBad.OK {
		t.Fatalf("expected failed /api/arr/test on invalid key, got code=%d res=%+v", code, testBad)
	}

	// 2. Scan and verify all ArrSummary states
	var scan scanner.ScanResult
	if code := apiDoJSON(t, ts, http.MethodPost, "/api/scan", nil, &scan); code != http.StatusOK {
		t.Fatalf("expected 200 from /api/scan, got %d", code)
	}

	byName := make(map[string]scanner.ComparisonGroup)
	for _, g := range scan.Groups {
		byName[g.Filename] = g
	}

	if byName["Inception (2010).mkv"].ArrSummary != "IMPORTED" {
		t.Fatalf("expected Inception to be IMPORTED, got %+v", byName["Inception (2010).mkv"])
	}
	if byName["Severance - S01E01.mkv"].ArrSummary != "IMPORTED" {
		t.Fatalf("expected Severance S01E01 to be IMPORTED, got %+v", byName["Severance - S01E01.mkv"])
	}
	matrixGrp := byName["The.Matrix.1999.2160p.mkv"]
	if matrixGrp.ArrSummary != "WANTED_NOT_IMPORTED" || !matrixGrp.HasArrIssue || len(matrixGrp.ArrMatches) == 0 || !matrixGrp.ArrMatches[0].CanImport {
		t.Fatalf("expected Matrix to be WANTED_NOT_IMPORTED with CanImport=true, got %+v", matrixGrp)
	}
	if heatGrp := byName["Heat.1995.BluRay.mkv"]; heatGrp.ArrSummary != "IN_QUEUE_BLOCKED" || !heatGrp.HasArrIssue || len(heatGrp.ArrMatches) == 0 || heatGrp.ArrMatches[0].RootPath == "" {
		t.Fatalf("expected Heat to be IN_QUEUE_BLOCKED with RootPath populated, got %+v", heatGrp)
	}
	mementoGrp := byName["Memento (2000).mkv"]
	if mementoGrp.ArrSummary != "UNLINKED_IN_LIBRARY" || !mementoGrp.HasArrIssue || len(mementoGrp.ArrMatches) == 0 || !mementoGrp.ArrMatches[0].CanRescan {
		t.Fatalf("expected Memento to be UNLINKED_IN_LIBRARY with CanRescan=true, got %+v", mementoGrp)
	}
	brGrp := byName["Blade.Runner.2049.2017.720p.mkv"]
	if brGrp.ArrSummary != "REPLACED_IN_ARR" || len(brGrp.ArrMatches) == 0 ||
		brGrp.ArrMatches[0].ExistingFileName != "Blade Runner 2049 (2017) 2160p.mkv" ||
		!strings.HasSuffix(brGrp.ArrMatches[0].ExistingFilePath, "/Blade Runner 2049 (2017) 2160p.mkv") {
		t.Fatalf("expected Blade Runner 2049 REPLACED_IN_ARR with ExistingFileName/ExistingFilePath, got %+v", brGrp)
	}
	if byName["Severance.S01E02.1080p.mkv"].ArrSummary != "WANTED_NOT_IMPORTED" {
		t.Fatalf("expected Severance S01E02 to be WANTED_NOT_IMPORTED, got %+v", byName["Severance.S01E02.1080p.mkv"])
	}
	sevOldGrp := byName["Severance.S01E01.720p.mkv"]
	if sevOldGrp.ArrSummary != "REPLACED_IN_ARR" || len(sevOldGrp.ArrMatches) == 0 ||
		sevOldGrp.ArrMatches[0].ExistingFileName != "Severance - S01E01.mkv" ||
		!strings.HasSuffix(sevOldGrp.ArrMatches[0].ExistingFilePath, "/Severance - S01E01.mkv") {
		t.Fatalf("expected Severance.S01E01.720p REPLACED_IN_ARR with ExistingFileName 'Severance - S01E01.mkv', got %+v", sevOldGrp)
	}

	// 3. Verify Read-Only Mode blocks arr-import & arr-rescan (403) while dry_run=true returns 200
	importPayload := map[string]any{
		"items": []map[string]any{{
			"instance_id":        "rad1",
			"local_path":         wantedDl,
			"download_client_id": "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111",
			"item_id":            20,
			"target_root_path":   matrixGrp.ArrMatches[0].RootPath,
		}},
		"force_manual_import": false,
		"dry_run":             true,
	}
	var dryImp actions.ArrImportResult
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/actions/arr-import", importPayload, &dryImp); c != http.StatusOK || dryImp.TriggeredCount != 1 {
		t.Fatalf("expected dry_run arr-import to succeed, got code=%d res=%+v", c, dryImp)
	}
	if dryImp.ImportMode != "auto" || dryImp.Results[0].TargetRootPath != matrixGrp.ArrMatches[0].RootPath {
		t.Fatalf("expected dry_run auto mode and target_root_path echoed, got %+v", dryImp.Results[0])
	}
	if len(dryImp.Results[0].Rejections) != 1 || dryImp.Results[0].Rejections[0] != "Not an upgrade" {
		t.Fatalf("expected manualimport preview rejection to be surfaced, got %+v", dryImp.Results[0])
	}

	importPayload["dry_run"] = false
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/actions/arr-import", importPayload, nil); c != http.StatusForbidden {
		t.Fatalf("expected 403 for arr-import in read-only mode, got %d", c)
	}

	rescanPayload := map[string]any{
		"items":   []map[string]any{{"instance_id": "rad1", "item_id": 40, "title": "Memento"}},
		"dry_run": false,
	}
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/actions/arr-rescan", rescanPayload, nil); c != http.StatusForbidden {
		t.Fatalf("expected 403 for arr-rescan in read-only mode, got %d", c)
	}

	// 4. Unlock write mode and execute DownloadedMoviesScan, ManualImport, RefreshMovie, and explicit import_mode="copy"
	apiDoJSON(t, ts, http.MethodPatch, "/api/config/read-only", map[string]bool{"read_only_mode": false}, nil)

	var execImp actions.ArrImportResult
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/actions/arr-import", importPayload, &execImp); c != http.StatusOK || execImp.TriggeredCount != 1 {
		t.Fatalf("expected arr-import execution to succeed, got code=%d res=%+v", c, execImp)
	}

	importPayload["force_manual_import"] = true
	var execManual actions.ArrImportResult
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/actions/arr-import", importPayload, &execManual); c != http.StatusOK || execManual.TriggeredCount != 1 {
		t.Fatalf("expected force manual arr-import execution to succeed, got code=%d res=%+v", c, execManual)
	}

	var execRescan actions.ArrRescanResult
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/actions/arr-rescan", rescanPayload, &execRescan); c != http.StatusOK || execRescan.TriggeredCount != 1 {
		t.Fatalf("expected arr-rescan execution to succeed, got code=%d res=%+v", c, execRescan)
	}

	importPayload["force_manual_import"] = false
	importPayload["import_mode"] = "copy"
	var execCopyScan actions.ArrImportResult
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/actions/arr-import", importPayload, &execCopyScan); c != http.StatusOK || execCopyScan.TriggeredCount != 1 || execCopyScan.ImportMode != "copy" {
		t.Fatalf("expected copy import scan to succeed, got code=%d res=%+v", c, execCopyScan)
	}

	importPayload["force_manual_import"] = true
	var execCopyManual actions.ArrImportResult
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/actions/arr-import", importPayload, &execCopyManual); c != http.StatusOK || execCopyManual.TriggeredCount != 1 || execCopyManual.ImportMode != "copy" {
		t.Fatalf("expected copy manual import to succeed, got code=%d res=%+v", c, execCopyManual)
	}

	radarrMock.mu.Lock()
	cmds := append([]map[string]any(nil), radarrMock.commands...)
	radarrMock.mu.Unlock()
	if len(cmds) != 5 {
		t.Fatalf("expected 5 Radarr commands received, got %d: %+v", len(cmds), cmds)
	}
	if cmds[0]["name"] != "DownloadedMoviesScan" || cmds[0]["path"] != "/arr_downloads/movies/The.Matrix.1999.2160p.mkv" || cmds[0]["importMode"] != "Auto" {
		t.Fatalf("unexpected DownloadedMoviesScan payload: %+v", cmds[0])
	}
	if cmds[1]["name"] != "ManualImport" || cmds[1]["importMode"] != "auto" {
		t.Fatalf("unexpected ManualImport payload: %+v", cmds[1])
	}
	if cmds[2]["name"] != "RefreshMovie" {
		t.Fatalf("unexpected RefreshMovie payload: %+v", cmds[2])
	}
	if cmds[3]["name"] != "DownloadedMoviesScan" || cmds[3]["importMode"] != "Copy" {
		t.Fatalf("unexpected explicit copy DownloadedMoviesScan payload: %+v", cmds[3])
	}
	if cmds[4]["name"] != "ManualImport" || cmds[4]["importMode"] != "copy" {
		t.Fatalf("unexpected explicit copy ManualImport payload: %+v", cmds[4])
	}
}

func TestRefreshGroupsAndArrCommandStatus(t *testing.T) {
	root := t.TempDir()
	plexDir := filepath.Join(root, "plex")
	dlDir := filepath.Join(root, "downloads")
	if err := os.MkdirAll(filepath.Join(plexDir, "movies"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dlDir, "movies"), 0o755); err != nil {
		t.Fatal(err)
	}

	// 1. DUPLICATE_COPY between Plex & Downloads (separate writes => distinct inodes)
	dupSize := 32 * 1024
	dupPayload := makePayload("dup-movie", dupSize)
	dupPlex := filepath.Join(plexDir, "movies", "Dune (2021)", "Dune.2021.1080p.mkv")
	dupDl := filepath.Join(dlDir, "movies", "Dune.2021.1080p.mkv")
	writeTestFile(t, dupPlex, dupPayload)
	writeTestFile(t, dupDl, dupPayload)

	// 2. ORPHANED_IN_B tracked in mock qBit (later deleted via wait_for_deleted_paths)
	orphanSize := 28 * 1024
	orphanDl := filepath.Join(dlDir, "movies", "Orphan.Sample.2020.mkv")
	writeTestFile(t, orphanDl, makePayload("orphan-seed", orphanSize))

	// 3. ORPHANED_IN_B + WANTED_NOT_IMPORTED in Radarr + tracked in qBit
	wantedSize := 40 * 1024
	wantedDl := filepath.Join(dlDir, "movies", "The.Matrix.1999.2160p.mkv")
	writeTestFile(t, wantedDl, makePayload("matrix-wanted", wantedSize))

	mq := newMockQbitServer(t, []mockTorrent{
		{
			Hash:     "1111222233334444555566667777888899990000",
			Name:     "Orphan.Sample.2020",
			SavePath: qbit.NormalizeSlashPath(dlDir),
			State:    "stalledUP",
			Ratio:    2.0,
			Tracker:  "https://tracker.example/announce",
			Trackers: []mockTracker{{URL: "https://tracker.example/announce", Status: 2}},
			Files: []struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
			}{{Name: "movies/Orphan.Sample.2020.mkv", Size: int64(orphanSize)}},
		},
		{
			Hash:     "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111",
			Name:     "The.Matrix.1999.2160p",
			SavePath: qbit.NormalizeSlashPath(dlDir),
			State:    "uploading",
			Ratio:    1.2,
			Tracker:  "https://tracker.example/announce",
			Trackers: []mockTracker{{URL: "https://tracker.example/announce", Status: 2}},
			Files: []struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
			}{{Name: "movies/The.Matrix.1999.2160p.mkv", Size: int64(wantedSize)}},
		},
	})

	radarrMock := newMockArrServer(t, "radarr")
	radarrMock.movies = []map[string]any{
		{
			"id":        20,
			"title":     "The Matrix",
			"year":      1999,
			"path":      "/media/movies/The Matrix (1999)",
			"hasFile":   false,
			"monitored": true,
		},
	}

	cfgMgr, err := config.NewManager(filepath.Join(root, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = cfgMgr.Update(config.AppSettings{
		ReadOnlyMode: false,
		Folders: []config.FolderConfig{
			{ID: "plex", Label: "Plex", Path: plexDir, Role: "primary"},
			{ID: "dl", Label: "Downloads", Path: dlDir, Role: "secondary"},
		},
		QbitInstances: []config.QbitInstanceConfig{
			{ID: "q1", Name: "qBit", URL: mq.server.URL, Username: "admin", Password: "secret", Enabled: true},
		},
		ArrInstances: []config.ArrInstanceConfig{
			{
				ID:      "rad1",
				Name:    "Radarr",
				Type:    "radarr",
				URL:     radarrMock.server.URL,
				APIKey:  "arr-secret-key",
				Enabled: true,
				PathMappings: []config.PathMapping{
					{RemotePrefix: "/media", LocalPrefix: qbit.NormalizeSlashPath(plexDir)},
					{RemotePrefix: "/arr_downloads", LocalPrefix: qbit.NormalizeSlashPath(dlDir)},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(server.New(cfgMgr, qbit.NewClient(5*time.Second), arr.NewClient(5*time.Second), nil))
	t.Cleanup(ts.Close)

	// Full initial scan
	var scanRes scanner.ScanResult
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/scan", nil, &scanRes); c != http.StatusOK {
		t.Fatalf("expected 200 on /api/scan, got %d", c)
	}
	if scanRes.Summary.TotalGroups != 3 || scanRes.Summary.DuplicateCopyCount != 1 || scanRes.Summary.OrphanedInBCount != 2 {
		t.Fatalf("unexpected initial scan summary: %+v", scanRes.Summary)
	}

	findGroup := func(groups []scanner.ComparisonGroup, substr string) (scanner.ComparisonGroup, bool) {
		for _, g := range groups {
			if strings.Contains(g.Filename, substr) {
				return g, true
			}
		}
		return scanner.ComparisonGroup{}, false
	}

	dupGrp, ok := findGroup(scanRes.Groups, "Dune.2021")
	if !ok || dupGrp.Status != "DUPLICATE_COPY" || len(dupGrp.HardlinkPairs) != 1 {
		t.Fatalf("expected DUPLICATE_COPY group for Dune, got ok=%v %+v", ok, dupGrp)
	}
	orphanGrp, ok := findGroup(scanRes.Groups, "Orphan.Sample")
	if !ok || orphanGrp.Status != "ORPHANED_IN_B" || len(orphanGrp.Torrents) != 1 {
		t.Fatalf("expected ORPHANED_IN_B group for Orphan.Sample, got ok=%v %+v", ok, orphanGrp)
	}
	wantedGrp, ok := findGroup(scanRes.Groups, "The.Matrix.1999")
	if !ok || wantedGrp.Status != "ORPHANED_IN_B" || wantedGrp.ArrSummary != "WANTED_NOT_IMPORTED" {
		t.Fatalf("expected WANTED_NOT_IMPORTED orphaned group for Matrix, got ok=%v %+v", ok, wantedGrp)
	}

	type refreshResponse struct {
		Summary         scanner.Summary           `json:"summary"`
		UpdatedGroups   []scanner.ComparisonGroup `json:"updated_groups"`
		RemovedGroupIDs []string                  `json:"removed_group_ids"`
		DurationMS      float64                   `json:"duration_ms"`
	}

	// A. Convert DUPLICATE_COPY -> HARDLINKED and refresh just dupGrp
	var hlRes actions.ConvertHardlinksResult
	hlPayload := map[string]any{
		"pairs":   dupGrp.HardlinkPairs,
		"dry_run": false,
	}
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/actions/convert-hardlinks", hlPayload, &hlRes); c != http.StatusOK || hlRes.ConvertedCount != 1 {
		t.Fatalf("expected convert-hardlinks to succeed, got code=%d res=%+v", c, hlRes)
	}

	var refHL refreshResponse
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/scan/refresh-groups", map[string]any{
		"group_ids":   []string{dupGrp.ID},
		"include_arr": false,
	}, &refHL); c != http.StatusOK {
		t.Fatalf("expected 200 on refresh-groups after hardlink, got %d", c)
	}
	if len(refHL.UpdatedGroups) != 1 || refHL.UpdatedGroups[0].Status != "HARDLINKED" || refHL.UpdatedGroups[0].ReclaimableBytes != 0 {
		t.Fatalf("expected Dune group to flip to HARDLINKED with 0 reclaimable, got %+v", refHL.UpdatedGroups)
	}
	if refHL.Summary.DuplicateCopyCount != 0 || refHL.Summary.HardlinkedCount != 1 {
		t.Fatalf("expected summary counts to update after targeted hardlink refresh, got %+v", refHL.Summary)
	}

	// B. Refresh wantedGrp with include_arr: false preserves ArrSummary & ArrMatches
	var refNoArr refreshResponse
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/scan/refresh-groups", map[string]any{
		"group_ids":   []string{wantedGrp.ID},
		"include_arr": false,
	}, &refNoArr); c != http.StatusOK || len(refNoArr.UpdatedGroups) != 1 {
		t.Fatalf("expected 200 on include_arr=false refresh, got %d %+v", c, refNoArr)
	}
	if refNoArr.UpdatedGroups[0].ArrSummary != "WANTED_NOT_IMPORTED" || len(refNoArr.UpdatedGroups[0].ArrMatches) == 0 {
		t.Fatalf("expected include_arr=false to preserve ArrSummary/ArrMatches, got %+v", refNoArr.UpdatedGroups[0])
	}

	// C. Simulate Radarr import (hardlink into Plex + movieFile in radarrMock) and refresh with include_arr: true
	wantedPlex := filepath.Join(plexDir, "movies", "The Matrix (1999)", "The Matrix (1999).mkv")
	if err := os.MkdirAll(filepath.Dir(wantedPlex), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(wantedDl, wantedPlex); err != nil {
		t.Fatal(err)
	}
	radarrMock.mu.Lock()
	radarrMock.movies[0]["hasFile"] = true
	radarrMock.movies[0]["movieFile"] = map[string]any{
		"id":           201,
		"path":         "/media/movies/The Matrix (1999)/The Matrix (1999).mkv",
		"relativePath": "The Matrix (1999).mkv",
		"size":         wantedSize,
	}
	radarrMock.mu.Unlock()

	var refArr refreshResponse
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/scan/refresh-groups", map[string]any{
		"group_ids":   []string{wantedGrp.ID},
		"include_arr": true,
	}, &refArr); c != http.StatusOK || len(refArr.UpdatedGroups) != 1 {
		t.Fatalf("expected 200 on include_arr=true refresh, got %d %+v", c, refArr)
	}
	if refArr.UpdatedGroups[0].Status != "HARDLINKED" || refArr.UpdatedGroups[0].ArrSummary != "IMPORTED" {
		t.Fatalf("expected Matrix group to become HARDLINKED & IMPORTED, got %+v", refArr.UpdatedGroups[0])
	}
	if refArr.Summary.HardlinkedCount != 2 || refArr.Summary.ArrImportedCount != 1 || refArr.Summary.ArrWantedNotImportedCount != 0 {
		t.Fatalf("unexpected summary after Matrix import refresh: %+v", refArr.Summary)
	}

	// D. Async file deletion + wait_for_deleted_paths removes group & updates summary
	go func() {
		time.Sleep(120 * time.Millisecond)
		_ = os.Remove(orphanDl)
	}()

	var refDel refreshResponse
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/scan/refresh-groups", map[string]any{
		"group_ids":              []string{orphanGrp.ID},
		"wait_for_deleted_paths": []string{qbit.NormalizeSlashPath(orphanDl)},
		"include_arr":            false,
	}, &refDel); c != http.StatusOK {
		t.Fatalf("expected 200 on wait_for_deleted_paths refresh, got %d", c)
	}
	if len(refDel.RemovedGroupIDs) != 1 || refDel.RemovedGroupIDs[0] != orphanGrp.ID {
		t.Fatalf("expected removed_group_ids=[%s], got %+v", orphanGrp.ID, refDel.RemovedGroupIDs)
	}
	if refDel.Summary.TotalGroups != 2 || refDel.Summary.OrphanedInBCount != 0 {
		t.Fatalf("expected TotalGroups=2 and OrphanedInBCount=0 after deletion, got %+v", refDel.Summary)
	}

	// E. Arr command status polling: queued -> completed
	var impRes actions.ArrImportResult
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/actions/arr-import", map[string]any{
		"items": []map[string]any{{
			"instance_id":        "rad1",
			"local_path":         wantedDl,
			"download_client_id": "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111",
			"item_id":            20,
		}},
		"force_manual_import": false,
		"dry_run":             false,
	}, &impRes); c != http.StatusOK || impRes.TriggeredCount != 1 || impRes.Results[0].CommandID <= 0 {
		t.Fatalf("expected arr-import with command_id, got code=%d res=%+v", c, impRes)
	}
	cmdID := impRes.Results[0].CommandID

	type cmdStatusResp struct {
		AllDone  bool `json:"all_done"`
		Commands []struct {
			InstanceID string `json:"instance_id"`
			CommandID  int    `json:"command_id"`
			Status     string `json:"status"`
			Done       bool   `json:"done"`
		} `json:"commands"`
	}

	var st1 cmdStatusResp
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/arr/commands/status", map[string]any{
		"commands": []map[string]any{{"instance_id": "rad1", "command_id": cmdID}},
	}, &st1); c != http.StatusOK || st1.AllDone || len(st1.Commands) != 1 || st1.Commands[0].Status != "queued" {
		t.Fatalf("expected queued/not-done command status, got code=%d %+v", c, st1)
	}

	radarrMock.mu.Lock()
	radarrMock.commandStatuses[cmdID] = "completed"
	radarrMock.mu.Unlock()

	var st2 cmdStatusResp
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/arr/commands/status", map[string]any{
		"commands": []map[string]any{{"instance_id": "rad1", "command_id": cmdID}},
	}, &st2); c != http.StatusOK || !st2.AllDone || len(st2.Commands) != 1 || st2.Commands[0].Status != "completed" {
		t.Fatalf("expected completed/done command status, got code=%d %+v", c, st2)
	}
}

func TestQbitRelocateAfterArrMove(t *testing.T) {
	root := t.TempDir()
	plexDir := filepath.Join(root, "plex_series")
	dlDir := filepath.Join(root, "downloads")
	if err := os.MkdirAll(plexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dlDir, 0o755); err != nil {
		t.Fatal(err)
	}

	epSize := 44 * 1024
	oldTaskmasterPlex := filepath.Join(plexDir, "taskmaster SE", "Season 13", "Taskmaster.SE.S13E01.mkv")
	writeTestFile(t, oldTaskmasterPlex, makePayload("taskmaster-s13e01", epSize))

	wrappedDir := filepath.Join(dlDir, "Taskmaster.S13E01")
	wrappedDlFile := filepath.Join(wrappedDir, "Taskmaster.SE.S13E01.mkv")
	if err := os.MkdirAll(wrappedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(oldTaskmasterPlex, wrappedDlFile); err != nil {
		t.Fatal(err)
	}

	packDl := filepath.Join(dlDir, "Season13Pack", "Episode02.mkv")
	writeTestFile(t, packDl, makePayload("pack-ep02", 24*1024))

	flatHash := "1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa"
	wrappedHash := "2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb"
	multiHash := "3333cccc3333cccc3333cccc3333cccc3333cccc"

	mq := newMockQbitServer(t, []mockTorrent{
		{
			Hash:     flatHash,
			Name:     "Taskmaster.SE.S13E01.mkv",
			SavePath: "/qbit_plex/taskmaster SE/Season 13",
			State:    "uploading",
			Ratio:    1.8,
			Tracker:  "https://tracker.test/announce",
			Trackers: []mockTracker{{URL: "https://tracker.test/announce", Status: 2}},
			Files: []struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
			}{{Name: "Taskmaster.SE.S13E01.mkv", Size: int64(epSize)}},
		},
		{
			Hash:     wrappedHash,
			Name:     "Taskmaster.S13E01",
			SavePath: "/qbit_dl",
			State:    "stalledUP",
			Ratio:    1.1,
			Tracker:  "https://tracker.test/announce",
			Trackers: []mockTracker{{URL: "https://tracker.test/announce", Status: 2}},
			Files: []struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
			}{{Name: "Taskmaster.S13E01/Taskmaster.SE.S13E01.mkv", Size: int64(epSize)}},
		},
		{
			Hash:     multiHash,
			Name:     "Season13Pack",
			SavePath: "/qbit_dl",
			State:    "uploading",
			Ratio:    0.9,
			Tracker:  "https://tracker.test/announce",
			Trackers: []mockTracker{{URL: "https://tracker.test/announce", Status: 2}},
			Files: []struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
			}{
				{Name: "Season13Pack/Episode02.mkv", Size: int64(24 * 1024)},
				{Name: "Season13Pack/Season13.nfo", Size: 512},
			},
		},
	})

	sonarrMock := newMockArrServer(t, "sonarr")
	sonarrMock.series = []map[string]any{
		{
			"id":        88,
			"title":     "Taskmaster SE",
			"year":      2017,
			"path":      "/tv/Bäst i Test",
			"monitored": true,
		},
	}
	sonarrMock.wanted = []map[string]any{
		{
			"id":            8800,
			"seriesId":      88,
			"seasonNumber":  13,
			"episodeNumber": 1,
			"title":         "Premiere",
			"monitored":     true,
			"hasFile":       false,
		},
	}

	cfgMgr, err := config.NewManager(filepath.Join(root, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = cfgMgr.Update(config.AppSettings{
		ReadOnlyMode: true,
		Folders: []config.FolderConfig{
			{ID: "plex", Label: "Plex Series", Path: plexDir, Role: "primary"},
			{ID: "dl", Label: "Downloads", Path: dlDir, Role: "secondary"},
		},
		QbitInstances: []config.QbitInstanceConfig{
			{
				ID:       "q1",
				Name:     "Series Qbit",
				URL:      mq.server.URL,
				Username: "admin",
				Password: "secret",
				Enabled:  true,
				PathMappings: []config.PathMapping{
					{RemotePrefix: "/qbit_plex", LocalPrefix: qbit.NormalizeSlashPath(plexDir)},
					{RemotePrefix: "/qbit_dl", LocalPrefix: qbit.NormalizeSlashPath(dlDir)},
				},
			},
		},
		ArrInstances: []config.ArrInstanceConfig{
			{
				ID:      "son1",
				Name:    "Sonarr",
				Type:    "sonarr",
				URL:     sonarrMock.server.URL,
				APIKey:  "arr-secret-key",
				Enabled: true,
				PathMappings: []config.PathMapping{
					{RemotePrefix: "/tv", LocalPrefix: qbit.NormalizeSlashPath(plexDir)},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(server.New(cfgMgr, qbit.NewClient(5*time.Second), arr.NewClient(5*time.Second), nil))
	t.Cleanup(ts.Close)

	// 1. Scan reports torrent_file_count == 1 vs 2
	var scanRes scanner.ScanResult
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/scan", nil, &scanRes); c != http.StatusOK {
		t.Fatalf("expected 200 on initial scan, got %d", c)
	}
	var taskGrp, packGrp scanner.ComparisonGroup
	for _, g := range scanRes.Groups {
		if g.Filename == "Taskmaster.SE.S13E01.mkv" {
			taskGrp = g
		}
		if g.Filename == "Episode02.mkv" {
			packGrp = g
		}
	}
	if taskGrp.ID == "" || len(taskGrp.Torrents) != 2 {
		t.Fatalf("expected Taskmaster group with 2 matched torrents, got %+v", taskGrp)
	}
	for _, tm := range taskGrp.Torrents {
		if tm.TorrentFileCount != 1 {
			t.Fatalf("expected TorrentFileCount=1 on single-file torrent %s, got %+v", tm.TorrentHash, tm)
		}
	}
	if packGrp.ID == "" || len(packGrp.Torrents) != 1 || packGrp.Torrents[0].TorrentFileCount != 2 {
		t.Fatalf("expected Episode02 pack group with TorrentFileCount=2, got %+v", packGrp)
	}

	// Simulate Sonarr moving + renaming the file into Bäst i Test/Season 13/
	movedPlex := filepath.Join(plexDir, "Bäst i Test", "Season 13", "Bäst i Test - S13E01.mkv")
	if err := os.MkdirAll(filepath.Dir(movedPlex), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(oldTaskmasterPlex, movedPlex); err != nil {
		t.Fatal(err)
	}
	sonarrMock.mu.Lock()
	sonarrMock.wanted = nil
	sonarrMock.episodeFiles["88"] = []map[string]any{
		{
			"id":           8801,
			"seriesId":     88,
			"seasonNumber": 13,
			"path":         "/tv/Bäst i Test/Season 13/Bäst i Test - S13E01.mkv",
			"relativePath": "Season 13/Bäst i Test - S13E01.mkv",
			"size":         epSize,
		},
	}
	sonarrMock.mu.Unlock()

	movedNorm := qbit.NormalizeSlashPath(movedPlex)

	// 2. Read-only mode blocks execute (403) while dry_run=true returns 200 with planned save/rel path
	relPayload := map[string]any{
		"items": []map[string]any{
			{"instance_id": "q1", "torrent_hash": flatHash, "new_local_path": movedNorm},
			{"instance_id": "q1", "torrent_hash": wrappedHash, "new_local_path": movedNorm},
			{"instance_id": "q1", "torrent_hash": multiHash, "new_local_path": qbit.NormalizeSlashPath(packDl)},
		},
		"dry_run": true,
	}
	var dryRel actions.QbitRelocateResult
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/actions/qbit-relocate", relPayload, &dryRel); c != http.StatusOK {
		t.Fatalf("expected 200 on dry_run qbit-relocate, got %d", c)
	}
	if dryRel.RelocatedCount != 2 || len(dryRel.Errors) != 1 {
		t.Fatalf("expected 2 eligible single-file outcomes and 1 multi-file skip error, got %+v", dryRel)
	}
	if dryRel.Results[0].NewSavePath != "/qbit_plex/Bäst i Test/Season 13" ||
		dryRel.Results[0].NewRelPath != "Bäst i Test - S13E01.mkv" || !dryRel.Results[0].Renamed {
		t.Fatalf("unexpected flat torrent relocate outcome: %+v", dryRel.Results[0])
	}
	if dryRel.Results[1].NewSavePath != "/qbit_plex/Bäst i Test" ||
		dryRel.Results[1].NewRelPath != "Season 13/Bäst i Test - S13E01.mkv" || !dryRel.Results[1].Renamed {
		t.Fatalf("unexpected wrapped torrent relocate outcome: %+v", dryRel.Results[1])
	}

	relPayload["dry_run"] = false
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/actions/qbit-relocate", relPayload, nil); c != http.StatusForbidden {
		t.Fatalf("expected 403 for qbit-relocate in read-only mode, got %d", c)
	}

	// 3. Unlock write mode & execute qbit-relocate
	apiDoJSON(t, ts, http.MethodPatch, "/api/config/read-only", map[string]bool{"read_only_mode": false}, nil)
	var execRel actions.QbitRelocateResult
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/actions/qbit-relocate", relPayload, &execRel); c != http.StatusOK {
		t.Fatalf("expected 200 on qbit-relocate execute, got %d", c)
	}
	if execRel.RelocatedCount != 2 || len(execRel.Errors) != 1 {
		t.Fatalf("expected 2 relocated torrents and 1 skipped multi-file error, got %+v", execRel)
	}

	mq.mu.Lock()
	flatMT := mq.torrents[flatHash]
	wrappedMT := mq.torrents[wrappedHash]
	multiMT := mq.torrents[multiHash]
	autoMgmtFlat, hasMgmt := mq.autoManagement[flatHash]
	ops := append([]string(nil), mq.relocateOps...)
	rechecked := append([]string(nil), mq.rechecked...)
	mq.mu.Unlock()

	if !hasMgmt || autoMgmtFlat {
		t.Fatalf("expected setAutoManagement(false) on flat torrent, got hasMgmt=%v val=%v", hasMgmt, autoMgmtFlat)
	}
	if flatMT.SavePath != "/qbit_plex/Bäst i Test/Season 13" || flatMT.Files[0].Name != "Bäst i Test - S13E01.mkv" {
		t.Fatalf("expected flat torrent SavePath/File renamed, got %+v", flatMT)
	}
	if wrappedMT.SavePath != "/qbit_plex/Bäst i Test" || wrappedMT.Files[0].Name != "Season 13/Bäst i Test - S13E01.mkv" {
		t.Fatalf("expected wrapped torrent SavePath/File renamed, got %+v", wrappedMT)
	}
	if multiMT.SavePath != "/qbit_dl" || multiMT.Files[0].Name != "Season13Pack/Episode02.mkv" {
		t.Fatalf("expected multi-file torrent untouched, got %+v", multiMT)
	}
	if len(rechecked) != 2 {
		t.Fatalf("expected 2 recheck calls, got %+v", rechecked)
	}
	joinedOps := strings.Join(ops, ",")
	if !strings.Contains(joinedOps, "stop:"+flatHash) || !strings.Contains(joinedOps, "start:"+flatHash) {
		t.Fatalf("expected stop/start lifecycle around relocate, got ops=%v", ops)
	}

	// 4. Refresh group with extra_qbit_hashes discovers moved file in Arr and re-attaches relocated torrents
	var refRes struct {
		UpdatedGroups []scanner.ComparisonGroup `json:"updated_groups"`
	}
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/scan/refresh-groups", map[string]any{
		"group_ids":         []string{taskGrp.ID},
		"include_arr":       true,
		"extra_qbit_hashes": map[string][]string{"q1": {flatHash, wrappedHash}},
	}, &refRes); c != http.StatusOK || len(refRes.UpdatedGroups) != 1 {
		t.Fatalf("expected 1 updated group on refresh-groups, got code=%d %+v", c, refRes)
	}
	if refRes.UpdatedGroups[0].ArrSummary != "IMPORTED" || refRes.UpdatedGroups[0].SeedingSummary != "SEEDING" || len(refRes.UpdatedGroups[0].Torrents) != 2 {
		t.Fatalf("expected moved group to become IMPORTED + SEEDING with 2 torrents, got %+v", refRes.UpdatedGroups[0])
	}
}

func TestQbitMultiFileRelocateInspectAndCompanions(t *testing.T) {
	root := t.TempDir()
	plexDir := filepath.Join(root, "plex")
	dlDir := filepath.Join(root, "downloads")

	// Season pack with 2 identically sized episodes + .nfo + 2 .srt companions in Subs/
	const epSize = 24 * 1024
	ep1Path := filepath.Join(dlDir, "Season13Pack", "Bäst.i.Test.S13E01.mkv")
	ep2Path := filepath.Join(dlDir, "Season13Pack", "Bäst.i.Test.S13E02.mkv")
	nfoPath := filepath.Join(dlDir, "Season13Pack", "Season13.nfo")
	srt1Path := filepath.Join(dlDir, "Season13Pack", "Subs", "Episode01.srt")
	srt2Path := filepath.Join(dlDir, "Season13Pack", "Subs", "Episode02.srt")

	writeTestFile(t, ep1Path, makePayload("s13-ep01-body", epSize))
	writeTestFile(t, ep2Path, makePayload("s13-ep02-body", epSize))
	writeTestFile(t, nfoPath, []byte("NFO metadata for Season 13"))
	writeTestFile(t, srt1Path, []byte("1\n00:00:01,000 --> 00:00:02,000\nHello Ep1\n"))
	writeTestFile(t, srt2Path, []byte("1\n00:00:01,000 --> 00:00:02,000\nHello Ep2\n"))

	// Separate torrent with a Sample/ video file
	const movieSize = 32 * 1024
	const sampleSize = 8 * 1024
	movieDlPath := filepath.Join(dlDir, "MovieWithSample", "Movie.mkv")
	sampleDlPath := filepath.Join(dlDir, "MovieWithSample", "Sample", "sample.mkv")
	writeTestFile(t, movieDlPath, makePayload("movie-main-body", movieSize))
	writeTestFile(t, sampleDlPath, makePayload("movie-sample-body", sampleSize))

	packHash := "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111"
	xsPackHash := "bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222"
	sampleHash := "cccc3333cccc3333cccc3333cccc3333cccc3333"

	packFiles := []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}{
		{Name: "Season13Pack/Bäst.i.Test.S13E01.mkv", Size: epSize},
		{Name: "Season13Pack/Bäst.i.Test.S13E02.mkv", Size: epSize},
		{Name: "Season13Pack/Season13.nfo", Size: int64(len("NFO metadata for Season 13"))},
		{Name: "Season13Pack/Subs/Episode01.srt", Size: int64(len("1\n00:00:01,000 --> 00:00:02,000\nHello Ep1\n"))},
		{Name: "Season13Pack/Subs/Episode02.srt", Size: int64(len("1\n00:00:01,000 --> 00:00:02,000\nHello Ep2\n"))},
	}

	mq := newMockQbitServer(t, []mockTorrent{
		{
			Hash:     packHash,
			Name:     "Bäst.i.Test.S13.Pack",
			SavePath: "/qbit_dl",
			State:    "uploading",
			Ratio:    2.1,
			Tracker:  "https://tracker-a.test/announce",
			Trackers: []mockTracker{{URL: "https://tracker-a.test/announce", Status: 2}},
			Files:    append([]struct{ Name string `json:"name"`; Size int64 `json:"size"` }(nil), packFiles...),
		},
		{
			Hash:     xsPackHash,
			Name:     "Bäst.i.Test.S13.Pack [XS]",
			SavePath: "/qbit_dl",
			State:    "stalledUP",
			Ratio:    1.4,
			Tracker:  "https://tracker-b.test/announce",
			Trackers: []mockTracker{{URL: "https://tracker-b.test/announce", Status: 2}},
			Files:    append([]struct{ Name string `json:"name"`; Size int64 `json:"size"` }(nil), packFiles...),
		},
		{
			Hash:     sampleHash,
			Name:     "MovieWithSample",
			SavePath: "/qbit_dl",
			State:    "uploading",
			Ratio:    1.2,
			Tracker:  "https://tracker-a.test/announce",
			Trackers: []mockTracker{{URL: "https://tracker-a.test/announce", Status: 2}},
			Files: []struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
			}{
				{Name: "MovieWithSample/Movie.mkv", Size: movieSize},
				{Name: "MovieWithSample/Sample/sample.mkv", Size: sampleSize},
			},
		},
	})

	sonarrMock := newMockArrServer(t, "sonarr")
	sonarrMock.series = []map[string]any{
		{
			"id":        88,
			"title":     "Bäst i Test",
			"year":      2017,
			"path":      "/tv/Bäst i Test",
			"monitored": true,
		},
	}
	sonarrMock.wanted = []map[string]any{
		{"id": 8801, "seriesId": 88, "seasonNumber": 13, "episodeNumber": 1, "title": "Ep 1", "monitored": true, "hasFile": false},
		{"id": 8802, "seriesId": 88, "seasonNumber": 13, "episodeNumber": 2, "title": "Ep 2", "monitored": true, "hasFile": false},
	}

	cfgMgr, err := config.NewManager(filepath.Join(root, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = cfgMgr.Update(config.AppSettings{
		ReadOnlyMode:     true,
		IgnoreExtensions: []string{".nfo", ".srt"},
		Folders: []config.FolderConfig{
			{ID: "plex", Label: "Plex Media", Path: plexDir, Role: "primary"},
			{ID: "dl", Label: "Downloads", Path: dlDir, Role: "secondary"},
		},
		QbitInstances: []config.QbitInstanceConfig{
			{
				ID:       "q1",
				Name:     "Main Qbit",
				URL:      mq.server.URL,
				Username: "admin",
				Password: "secret",
				Enabled:  true,
				PathMappings: []config.PathMapping{
					{RemotePrefix: "/qbit_plex", LocalPrefix: qbit.NormalizeSlashPath(plexDir)},
					{RemotePrefix: "/qbit_dl", LocalPrefix: qbit.NormalizeSlashPath(dlDir)},
				},
			},
		},
		ArrInstances: []config.ArrInstanceConfig{
			{
				ID:      "son1",
				Name:    "Sonarr",
				Type:    "sonarr",
				URL:     sonarrMock.server.URL,
				APIKey:  "arr-secret-key",
				Enabled: true,
				PathMappings: []config.PathMapping{
					{RemotePrefix: "/tv", LocalPrefix: qbit.NormalizeSlashPath(plexDir)},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(server.New(cfgMgr, qbit.NewClient(5*time.Second), arr.NewClient(5*time.Second), nil))
	t.Cleanup(ts.Close)

	// 1. Initial scan verifies 64-bit inode_str population
	var scanRes scanner.ScanResult
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/scan", nil, &scanRes); c != http.StatusOK {
		t.Fatalf("expected 200 on initial scan, got %d", c)
	}
	var ep1Grp, ep2Grp, movieGrp scanner.ComparisonGroup
	for _, g := range scanRes.Groups {
		switch g.Filename {
		case "Bäst.i.Test.S13E01.mkv":
			ep1Grp = g
		case "Bäst.i.Test.S13E02.mkv":
			ep2Grp = g
		case "Movie.mkv":
			movieGrp = g
		}
	}
	if ep1Grp.ID == "" || ep2Grp.ID == "" || movieGrp.ID == "" {
		t.Fatalf("expected Episode01, Episode02, and Movie groups in scan, got %d groups", len(scanRes.Groups))
	}
	if len(ep1Grp.FolderBFiles) == 0 || ep1Grp.FolderBFiles[0].InodeStr != strconv.FormatUint(ep1Grp.FolderBFiles[0].Inode, 10) {
		t.Fatalf("expected exact inode_str on scanned file, got %+v", ep1Grp.FolderBFiles)
	}

	// 2. Inspect endpoint works in Read-Only mode for partial pack, full pack (.nfo/.srt), and sample torrent
	var partialInspect actions.QbitInspectResult
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/actions/qbit-relocate/inspect", map[string]any{
		"torrents": []map[string]any{{"instance_id": "q1", "torrent_hash": packHash}},
		"selected_local_paths": []string{qbit.NormalizeSlashPath(ep1Path)},
	}, &partialInspect); c != http.StatusOK || len(partialInspect.Torrents) != 1 {
		t.Fatalf("expected 200 with 1 inspected torrent, got code=%d res=%+v", c, partialInspect)
	}
	pt := partialInspect.Torrents[0]
	if pt.Status != "partial_pack" || pt.CanRelocate || pt.UnselectedVideoCount != 1 || pt.SelectedCount != 1 {
		t.Fatalf("expected partial_pack blocked on single episode selection, got %+v", pt)
	}

	var fullInspect actions.QbitInspectResult
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/actions/qbit-relocate/inspect", map[string]any{
		"torrents": []map[string]any{
			{"instance_id": "q1", "torrent_hash": packHash},
			{"instance_id": "q1", "torrent_hash": xsPackHash},
			{"instance_id": "q1", "torrent_hash": sampleHash},
		},
		"selected_local_paths": []string{
			qbit.NormalizeSlashPath(ep1Path),
			qbit.NormalizeSlashPath(ep2Path),
			qbit.NormalizeSlashPath(movieDlPath),
		},
	}, &fullInspect); c != http.StatusOK || len(fullInspect.Torrents) != 3 {
		t.Fatalf("expected 200 with 3 inspected torrents, got code=%d res=%+v", c, fullInspect)
	}
	inspPack := fullInspect.Torrents[0]
	inspSample := fullInspect.Torrents[2]
	if inspPack.Status != "ready_with_companions" || !inspPack.CanRelocate || inspPack.SelectedCount != 2 || inspPack.CompanionCount != 3 || len(inspPack.Warnings) == 0 {
		t.Fatalf("expected ready_with_companions with 3 companions (.nfo + 2 .srt), got %+v", inspPack)
	}
	if inspSample.Status != "has_samples" || !inspSample.CanRelocate || inspSample.SampleCount != 1 || len(inspSample.Warnings) == 0 {
		t.Fatalf("expected has_samples warning for Sample/sample.mkv, got %+v", inspSample)
	}

	// Capture pre-move inspected file info (including inode_str of Subs/Episode01.srt) BEFORE Sonarr moves it
	preFiles := make(map[string]actions.QbitInspectedFile, len(inspPack.Files))
	for _, f := range inspPack.Files {
		preFiles[f.RelPath] = f
	}

	// 3. Simulate Sonarr moving & renaming both identically sized episodes AND Subs/Episode01.srt
	movedSeasonDir := filepath.Join(plexDir, "Bäst i Test", "Season 13")
	if err := os.MkdirAll(movedSeasonDir, 0o755); err != nil {
		t.Fatal(err)
	}
	movedEp1 := filepath.Join(movedSeasonDir, "Bäst i Test - S13E01.mkv")
	movedEp2 := filepath.Join(movedSeasonDir, "Bäst i Test - S13E02.mkv")
	movedSrt1 := filepath.Join(movedSeasonDir, "Bäst i Test - S13E01.srt")
	if err := os.Rename(ep1Path, movedEp1); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(ep2Path, movedEp2); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(srt1Path, movedSrt1); err != nil {
		t.Fatal(err)
	}

	sonarrMock.mu.Lock()
	sonarrMock.wanted = nil
	sonarrMock.episodeFiles["88"] = []map[string]any{
		{
			"id":           8801,
			"seriesId":     88,
			"seasonNumber": 13,
			"path":         "/tv/Bäst i Test/Season 13/Bäst i Test - S13E01.mkv",
			"relativePath": "Season 13/Bäst i Test - S13E01.mkv",
			"size":         epSize,
		},
		{
			"id":           8802,
			"seriesId":     88,
			"seasonNumber": 13,
			"path":         "/tv/Bäst i Test/Season 13/Bäst i Test - S13E02.mkv",
			"relativePath": "Season 13/Bäst i Test - S13E02.mkv",
			"size":         epSize,
		},
	}
	sonarrMock.mu.Unlock()

	// 4. Verify refresh-groups tracks each episode distinctly by inode despite identical byte sizes
	var refRes struct {
		UpdatedGroups []scanner.ComparisonGroup `json:"updated_groups"`
	}
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/scan/refresh-groups", map[string]any{
		"group_ids":   []string{ep1Grp.ID, ep2Grp.ID},
		"include_arr": true,
	}, &refRes); c != http.StatusOK || len(refRes.UpdatedGroups) != 2 {
		t.Fatalf("expected 2 refreshed groups, got code=%d res=%+v", c, refRes)
	}
	refByID := map[string]scanner.ComparisonGroup{
		refRes.UpdatedGroups[0].ID: refRes.UpdatedGroups[0],
		refRes.UpdatedGroups[1].ID: refRes.UpdatedGroups[1],
	}
	if refByID[ep1Grp.ID].Filename != "Bäst i Test - S13E01.mkv" || refByID[ep2Grp.ID].Filename != "Bäst i Test - S13E02.mkv" {
		t.Fatalf("expected exact 1-to-1 inode tracking across identical-size episodes, got ep1=%s ep2=%s",
			refByID[ep1Grp.ID].Filename, refByID[ep2Grp.ID].Filename)
	}

	buildPackItem := func(hash string) map[string]any {
		mappings := make([]map[string]any, 0, len(inspPack.Files))
		for _, f := range inspPack.Files {
			newLocal := ""
			switch f.RelPath {
			case "Season13Pack/Bäst.i.Test.S13E01.mkv":
				newLocal = qbit.NormalizeSlashPath(movedEp1)
			case "Season13Pack/Bäst.i.Test.S13E02.mkv":
				newLocal = qbit.NormalizeSlashPath(movedEp2)
			}
			mappings = append(mappings, map[string]any{
				"old_rel_path":   f.RelPath,
				"old_local_path": f.LocalPath,
				"new_local_path": newLocal,
				"kind":           f.Kind,
				"device":         f.Device,
				"inode_str":      f.InodeStr,
				"size":           f.Size,
			})
		}
		return map[string]any{
			"instance_id":     "q1",
			"torrent_hash":    hash,
			"file_mappings":   mappings,
			"move_companions": true,
			"move_samples":    false,
		}
	}

	// 5. Dry-run multi-file relocate in Read-Only mode previews 5 file changes + 2 companion moves without modifying disk
	packRelPayload := map[string]any{
		"items":   []map[string]any{buildPackItem(packHash), buildPackItem(xsPackHash)},
		"dry_run": true,
	}
	var dryPackRes actions.QbitRelocateResult
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/actions/qbit-relocate", packRelPayload, &dryPackRes); c != http.StatusOK {
		t.Fatalf("expected 200 on dry_run multi-file relocate, got %d", c)
	}
	if dryPackRes.RelocatedCount != 2 || len(dryPackRes.Errors) != 0 {
		t.Fatalf("expected 2 dry-run pack results with 0 errors, got %+v", dryPackRes)
	}
	firstOutcome := dryPackRes.Results[0]
	if firstOutcome.NewSavePath != "/qbit_plex/Bäst i Test" || len(firstOutcome.FileChanges) != 5 || len(firstOutcome.MovedCompanions) != 2 {
		t.Fatalf("unexpected dry-run pack outcome: %+v", firstOutcome)
	}
	if _, statErr := os.Stat(nfoPath); statErr != nil {
		t.Fatalf("expected Season13.nfo untouched during dry_run, got %v", statErr)
	}

	packRelPayload["dry_run"] = false
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/actions/qbit-relocate", packRelPayload, nil); c != http.StatusForbidden {
		t.Fatalf("expected 403 for multi-file relocate in read-only mode, got %d", c)
	}

	// 6. Unlock Write Mode and execute cross-seeded pack relocation + companion moves
	apiDoJSON(t, ts, http.MethodPatch, "/api/config/read-only", map[string]bool{"read_only_mode": false}, nil)
	var execPackRes actions.QbitRelocateResult
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/actions/qbit-relocate", packRelPayload, &execPackRes); c != http.StatusOK {
		t.Fatalf("expected 200 on execute multi-file relocate, got %d", c)
	}
	if execPackRes.RelocatedCount != 2 || len(execPackRes.Errors) != 0 {
		t.Fatalf("expected both primary and cross-seed packs relocated, got %+v", execPackRes)
	}

	// Verify leftover .nfo and Subs/Episode02.srt moved to Plex Season 13, and empty dl folder removed
	if _, statErr := os.Stat(filepath.Join(movedSeasonDir, "Season13.nfo")); statErr != nil {
		t.Fatalf("expected Season13.nfo moved alongside pack: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(movedSeasonDir, "Subs", "Episode02.srt")); statErr != nil {
		t.Fatalf("expected Subs/Episode02.srt moved preserving Subs/ subfolder: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dlDir, "Season13Pack")); !os.IsNotExist(statErr) {
		t.Fatalf("expected empty old Season13Pack folder removed, got statErr=%v", statErr)
	}

	mq.mu.Lock()
	packMT := mq.torrents[packHash]
	xsMT := mq.torrents[xsPackHash]
	ops := append([]string(nil), mq.relocateOps...)
	mq.mu.Unlock()

	expectedNames := map[string]bool{
		"Season 13/Bäst i Test - S13E01.mkv": false,
		"Season 13/Bäst i Test - S13E02.mkv": false,
		"Season 13/Season13.nfo":             false,
		"Season 13/Bäst i Test - S13E01.srt": false,
		"Season 13/Subs/Episode02.srt":       false,
	}
	if packMT.SavePath != "/qbit_plex/Bäst i Test" || len(packMT.Files) != 5 {
		t.Fatalf("unexpected qBit primary pack state after relocate: %+v", packMT)
	}
	for _, f := range packMT.Files {
		if _, wanted := expectedNames[f.Name]; !wanted {
			t.Fatalf("unexpected file path in relocated qBit pack: %q", f.Name)
		}
		expectedNames[f.Name] = true
	}
	for name, seen := range expectedNames {
		if !seen {
			t.Fatalf("missing expected renamed file %q in relocated qBit pack", name)
		}
	}
	if xsMT.SavePath != "/qbit_plex/Bäst i Test" || len(xsMT.Files) != 5 {
		t.Fatalf("unexpected cross-seeded pack state after relocate: %+v", xsMT)
	}
	if !strings.Contains(strings.Join(ops, ","), "setLocation:"+packHash+":/qbit_plex/Bäst i Test") {
		t.Fatalf("expected setLocation op for packHash, got %v", ops)
	}

	// 7. Sample protection test: cross-folder torrent with Sample/sample.mkv refuses when move_samples=false
	movedMovie := filepath.Join(plexDir, "Movies", "My Movie (2024)", "My Movie (2024).mkv")
	if err := os.MkdirAll(filepath.Dir(movedMovie), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(movieDlPath, movedMovie); err != nil {
		t.Fatal(err)
	}

	buildSampleItem := func(moveSamples bool) map[string]any {
		mappings := make([]map[string]any, 0, len(inspSample.Files))
		for _, f := range inspSample.Files {
			newLocal := ""
			if f.RelPath == "MovieWithSample/Movie.mkv" {
				newLocal = qbit.NormalizeSlashPath(movedMovie)
			}
			mappings = append(mappings, map[string]any{
				"old_rel_path":   f.RelPath,
				"old_local_path": f.LocalPath,
				"new_local_path": newLocal,
				"kind":           f.Kind,
				"device":         f.Device,
				"inode_str":      f.InodeStr,
				"size":           f.Size,
			})
		}
		return map[string]any{
			"instance_id":     "q1",
			"torrent_hash":    sampleHash,
			"file_mappings":   mappings,
			"move_companions": true,
			"move_samples":    moveSamples,
		}
	}

	var refusedSample actions.QbitRelocateResult
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/actions/qbit-relocate", map[string]any{
		"items":   []map[string]any{buildSampleItem(false)},
		"dry_run": false,
	}, &refusedSample); c != http.StatusOK {
		t.Fatalf("expected 200 JSON result on sample refusal, got %d", c)
	}
	if refusedSample.RelocatedCount != 0 || len(refusedSample.Errors) != 1 || !strings.Contains(strings.ToLower(refusedSample.Errors[0].Error), "sample") {
		t.Fatalf("expected clean refusal mentioning sample when move_samples=false, got %+v", refusedSample)
	}
	if _, statErr := os.Stat(sampleDlPath); statErr != nil {
		t.Fatalf("expected Sample/sample.mkv untouched when move_samples=false, got %v", statErr)
	}

	// Now explicitly opt into moving samples -> relocates cleanly
	var allowedSample actions.QbitRelocateResult
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/actions/qbit-relocate", map[string]any{
		"items":   []map[string]any{buildSampleItem(true)},
		"dry_run": false,
	}, &allowedSample); c != http.StatusOK || allowedSample.RelocatedCount != 1 || len(allowedSample.Errors) != 0 {
		t.Fatalf("expected sample pack relocation to succeed with move_samples=true, got code=%d res=%+v", c, allowedSample)
	}
	if _, statErr := os.Stat(filepath.Join(plexDir, "Movies", "My Movie (2024)", "Sample", "sample.mkv")); statErr != nil {
		t.Fatalf("expected Sample/sample.mkv moved when move_samples=true: %v", statErr)
	}

	_ = preFiles // kept for readability of captured inspect map
}

func TestQbitSwapRenameCollisionAndCommonSlashDir(t *testing.T) {
	if got := qbit.CommonSlashDir([]string{"/a/b/Show/Season 01/e1.mkv", "/a/b/Show/Season 02/e2.mkv"}, false); got != "/a/b/Show" {
		t.Fatalf("expected /a/b/Show, got %q", got)
	}
	if got := qbit.CommonSlashDir([]string{"C:/Movies/A/1.mkv", "D:/Movies/A/2.mkv"}, true); got != "" {
		t.Fatalf("expected empty string across different Windows drives, got %q", got)
	}

	root := t.TempDir()
	plexDir := filepath.Join(root, "plex")
	f1 := filepath.Join(plexDir, "Show", "Season 01", "EpA.mkv")
	f2 := filepath.Join(plexDir, "Show", "Season 01", "EpB.mkv")
	writeTestFile(t, f1, makePayload("swap-a", 16*1024))
	writeTestFile(t, f2, makePayload("swap-b", 16*1024))

	swapHash := "dddd4444dddd4444dddd4444dddd4444dddd4444"
	mq := newMockQbitServer(t, []mockTorrent{
		{
			Hash:     swapHash,
			Name:     "Show",
			SavePath: "/qbit_plex",
			State:    "uploading",
			Ratio:    1.0,
			Files: []struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
			}{
				{Name: "Show/Season 01/EpA.mkv", Size: 16 * 1024},
				{Name: "Show/Season 01/EpB.mkv", Size: 16 * 1024},
			},
		},
	})

	cfgMgr, err := config.NewManager(filepath.Join(root, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = cfgMgr.Update(config.AppSettings{
		ReadOnlyMode: false,
		Folders:      []config.FolderConfig{{ID: "plex", Label: "Plex", Path: plexDir, Role: "primary"}},
		QbitInstances: []config.QbitInstanceConfig{
			{
				ID:       "q1",
				Name:     "q1",
				URL:      mq.server.URL,
				Username: "admin",
				Password: "secret",
				Enabled:  true,
				PathMappings: []config.PathMapping{
					{RemotePrefix: "/qbit_plex", LocalPrefix: qbit.NormalizeSlashPath(plexDir)},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	qc := qbit.NewClient(5 * time.Second)
	inst := cfgMgr.Get().QbitInstances[0]
	// Swap A <-> B paths in a single RelocateTorrent call; mock rejects direct collision with 409
	out, err := qc.RelocateTorrent(context.Background(), inst, swapHash, map[string]string{
		"Show/Season 01/EpA.mkv": "/qbit_plex/Show/Season 01/EpB.mkv",
		"Show/Season 01/EpB.mkv": "/qbit_plex/Show/Season 01/EpA.mkv",
	}, false)
	if err != nil {
		t.Fatalf("expected two-stage swap rename to succeed without 409 collision, got %v", err)
	}
	if out.RenamedCount != 2 {
		t.Fatalf("expected RenamedCount=2, got %+v", out)
	}
	mq.mu.Lock()
	mt := mq.torrents[swapHash]
	mq.mu.Unlock()
	if mt.SavePath != "/qbit_plex/Show" || mt.Files[0].Name != "Season 01/EpB.mkv" || mt.Files[1].Name != "Season 01/EpA.mkv" {
		t.Fatalf("expected swapped file names in mock qBit, got SavePath=%q Files=%+v", mt.SavePath, mt.Files)
	}
}

func TestArrSeriesAndMovieTitleYearDisambiguation(t *testing.T) {
	root := t.TempDir()
	plexDir := filepath.Join(root, "plex")
	dlDir := filepath.Join(root, "downloads")
	if err := os.MkdirAll(plexDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Disk files in Downloads (Folder B) to test Sonarr & Radarr title+year matching
	fAvatar2024 := filepath.Join(dlDir, "tv", "Avatar.The.Last.Airbender.2024.S02E01.Somewhere.Safe.2160p.NF.WEB-DL.mkv")
	fAvatar2005 := filepath.Join(dlDir, "tv", "Avatar.The.Last.Airbender.S02E01.The.Avatar.State.720p.mkv")
	fDaryl := filepath.Join(dlDir, "tv", "The.Walking.Dead.Daryl.Dixon.S01E01.1080p.mkv")
	fDoctorWho := filepath.Join(dlDir, "tv", "Doctor.Who.S05E01.1080p.mkv")
	fLastOfUs := filepath.Join(dlDir, "tv", "The.Last.of.Us.2023.S01E01.1080p.mkv")
	fFastX := filepath.Join(dlDir, "movies", "Fast.X.2023.2160p.mkv")

	for i, p := range []string{fAvatar2024, fAvatar2005, fDaryl, fDoctorWho, fLastOfUs, fFastX} {
		writeTestFile(t, p, makePayload("disambig-"+strconv.Itoa(i), 24*1024))
	}

	sonarrMock := newMockArrServer(t, "sonarr")
	sonarrMock.series = []map[string]any{
		{"id": 101, "title": "Avatar: The Last Airbender", "year": 2005, "path": "/media/tv/Avatar The Last Airbender", "monitored": true},
		{"id": 102, "title": "Avatar: The Last Airbender (2024)", "year": 2024, "path": "/media/tv/Avatar The Last Airbender (2024)", "monitored": true},
		{"id": 103, "title": "The Walking Dead", "year": 2010, "path": "/media/tv/The Walking Dead", "monitored": true},
		{"id": 104, "title": "The Walking Dead: Daryl Dixon", "year": 2023, "path": "/media/tv/The Walking Dead Daryl Dixon", "monitored": true},
		{"id": 105, "title": "Doctor Who (2005)", "year": 2005, "path": "/media/tv/Doctor Who (2005)", "monitored": true},
		{"id": 106, "title": "The Last of Us", "year": 2023, "path": "/media/tv/The Last of Us", "monitored": true},
	}
	sonarrMock.episodeFiles["101"] = []map[string]any{
		{
			"id": 1011, "seriesId": 101, "seasonNumber": 2,
			"path":         "/media/tv/Avatar The Last Airbender/Season 02/Avatar.The.Last.Airbender.S02E01.The.Avatar.State.mkv",
			"relativePath": "Season 02/Avatar.The.Last.Airbender.S02E01.The.Avatar.State.mkv",
			"size":         900001,
		},
	}
	sonarrMock.episodeFiles["102"] = []map[string]any{
		{
			"id": 1021, "seriesId": 102, "seasonNumber": 2,
			"path":         "/media/tv/Avatar The Last Airbender (2024)/Season 02/Avatar.The.Last.Airbender.2024.S02E01.Somewhere.Safe.DV.HDR.mkv",
			"relativePath": "Season 02/Avatar.The.Last.Airbender.2024.S02E01.Somewhere.Safe.DV.HDR.mkv",
			"size":         900002,
		},
	}
	sonarrMock.episodeFiles["103"] = []map[string]any{
		{
			"id": 1031, "seriesId": 103, "seasonNumber": 1,
			"path":         "/media/tv/The Walking Dead/Season 01/The.Walking.Dead.S01E01.mkv",
			"relativePath": "Season 01/The.Walking.Dead.S01E01.mkv",
			"size":         900003,
		},
	}
	sonarrMock.wanted = []map[string]any{
		{"id": 1041, "seriesId": 104, "seasonNumber": 1, "episodeNumber": 1, "title": "L'ame Perdue", "monitored": true, "hasFile": false},
		{"id": 1051, "seriesId": 105, "seasonNumber": 5, "episodeNumber": 1, "title": "The Eleventh Hour", "monitored": true, "hasFile": false},
		{"id": 1061, "seriesId": 106, "seasonNumber": 1, "episodeNumber": 1, "title": "When You're Lost in the Darkness", "monitored": true, "hasFile": false},
	}

	radarrMock := newMockArrServer(t, "radarr")
	radarrMock.movies = []map[string]any{
		{"id": 201, "title": "Fast", "year": 2023, "path": "/media/movies/Fast (2023)", "hasFile": false, "monitored": true},
		{"id": 202, "title": "Fast X", "year": 2023, "path": "/media/movies/Fast X (2023)", "hasFile": false, "monitored": true},
	}

	cfgMgr, err := config.NewManager(filepath.Join(root, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = cfgMgr.Update(config.AppSettings{
		ReadOnlyMode: true,
		Folders: []config.FolderConfig{
			{ID: "plex", Label: "Plex", Path: plexDir, Role: "primary"},
			{ID: "dl", Label: "Downloads", Path: dlDir, Role: "secondary"},
		},
		ArrInstances: []config.ArrInstanceConfig{
			{
				ID: "son1", Name: "Sonarr", Type: "sonarr", URL: sonarrMock.server.URL, APIKey: "arr-secret-key", Enabled: true,
				PathMappings: []config.PathMapping{{RemotePrefix: "/media", LocalPrefix: qbit.NormalizeSlashPath(plexDir)}},
			},
			{
				ID: "rad1", Name: "Radarr", Type: "radarr", URL: radarrMock.server.URL, APIKey: "arr-secret-key", Enabled: true,
				PathMappings: []config.PathMapping{{RemotePrefix: "/media", LocalPrefix: qbit.NormalizeSlashPath(plexDir)}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(server.New(cfgMgr, qbit.NewClient(5*time.Second), arr.NewClient(5*time.Second), nil))
	t.Cleanup(ts.Close)

	var scanRes scanner.ScanResult
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/scan", nil, &scanRes); c != http.StatusOK {
		t.Fatalf("expected 200 on /api/scan, got %d", c)
	}

	byName := make(map[string]scanner.ComparisonGroup, len(scanRes.Groups))
	for _, g := range scanRes.Groups {
		byName[g.Filename] = g
	}

	g2024 := byName[filepath.Base(fAvatar2024)]
	if len(g2024.ArrMatches) != 1 || g2024.ArrMatches[0].ItemID != 102 || g2024.ArrSummary != "REPLACED_IN_ARR" {
		t.Fatalf("expected Avatar 2024 release to match ONLY ID 102, got %+v", g2024.ArrMatches)
	}

	g2005 := byName[filepath.Base(fAvatar2005)]
	if len(g2005.ArrMatches) != 1 || g2005.ArrMatches[0].ItemID != 101 || g2005.ArrSummary != "REPLACED_IN_ARR" {
		t.Fatalf("expected Avatar (2005) release to match ONLY ID 101, got %+v", g2005.ArrMatches)
	}

	gDaryl := byName[filepath.Base(fDaryl)]
	if len(gDaryl.ArrMatches) != 1 || gDaryl.ArrMatches[0].ItemID != 104 || gDaryl.ArrSummary != "WANTED_NOT_IMPORTED" {
		t.Fatalf("expected Walking Dead Daryl Dixon release to match ONLY ID 104, got %+v", gDaryl.ArrMatches)
	}

	gDoc := byName[filepath.Base(fDoctorWho)]
	if len(gDoc.ArrMatches) != 1 || gDoc.ArrMatches[0].ItemID != 105 || gDoc.ArrSummary != "WANTED_NOT_IMPORTED" {
		t.Fatalf("expected Doctor Who (no year in filename) to match unique ID 105, got %+v", gDoc.ArrMatches)
	}

	gLast := byName[filepath.Base(fLastOfUs)]
	if len(gLast.ArrMatches) != 1 || gLast.ArrMatches[0].ItemID != 106 || gLast.ArrSummary != "WANTED_NOT_IMPORTED" {
		t.Fatalf("expected The Last of Us 2023 to match ID 106, got %+v", gLast.ArrMatches)
	}

	gFastX := byName[filepath.Base(fFastX)]
	if len(gFastX.ArrMatches) != 1 || gFastX.ArrMatches[0].ItemID != 202 || gFastX.ArrSummary != "WANTED_NOT_IMPORTED" {
		t.Fatalf("expected Fast X 2023 to match ONLY ID 202 (not Fast 2023), got %+v", gFastX.ArrMatches)
	}
}

func TestArrPostersGenresAndDisplaySetting(t *testing.T) {
	root := t.TempDir()
	plexDir := filepath.Join(root, "plex")
	dlDir := filepath.Join(root, "downloads")

	// Legacy config backward compatibility: missing show_media_metadata defaults to true.
	legacyCfgPath := filepath.Join(root, "legacy-settings.json")
	if err := os.WriteFile(legacyCfgPath, []byte(`{"read_only_mode":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	legacyMgr, err := config.NewManager(legacyCfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !legacyMgr.Get().ShowMediaMetadata {
		t.Fatalf("expected legacy config without show_media_metadata to default to true")
	}

	// Files covering imported, wanted, queue, library_unlinked, and replaced paths.
	fIncPlex := filepath.Join(plexDir, "movies", "Inception (2010)", "Inception (2010).mkv")
	fIncDl := filepath.Join(dlDir, "movies", "Inception.2010.1080p.mkv")
	writeTestFile(t, fIncPlex, makePayload("inc-meta", 32*1024))
	if err := os.MkdirAll(filepath.Dir(fIncDl), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(fIncPlex, fIncDl); err != nil {
		t.Fatal(err)
	}

	fDune := filepath.Join(dlDir, "movies", "Dune.Part.Two.2024.1080p.mkv")
	fHeat := filepath.Join(dlDir, "movies", "Heat.1995.BluRay.mkv")
	fAndorUnlinked := filepath.Join(plexDir, "tv", "Andor", "Season 01", "Andor.S01E01.mkv")
	fSevReplaced := filepath.Join(dlDir, "tv", "Severance.S01E01.720p.mkv")
	fSiloWanted := filepath.Join(dlDir, "tv", "Silo.S01E01.1080p.mkv")
	heatSize := 40 * 1024
	writeTestFile(t, fDune, makePayload("dune2-meta", 28*1024))
	writeTestFile(t, fHeat, makePayload("heat-meta", heatSize))
	writeTestFile(t, fAndorUnlinked, makePayload("andor-meta", 30*1024))
	writeTestFile(t, fSevReplaced, makePayload("sev-rep-meta", 26*1024))
	writeTestFile(t, fSiloWanted, makePayload("silo-want-meta", 24*1024))

	radarrPosterPNG := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 'R', 'A', 'D', '1'}
	heatPosterPNG := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 'H', 'E', 'A', 'T'}
	sonarrPosterPNG := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 'S', 'O', 'N', '1'}
	remotePosterPNG := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 'R', 'E', 'M', '2'}

	var remoteMu sync.Mutex
	remoteSawApiKey := false
	remotePosterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "" {
			remoteMu.Lock()
			remoteSawApiKey = true
			remoteMu.Unlock()
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(remotePosterPNG)
	}))
	t.Cleanup(remotePosterSrv.Close)

	mq := newMockQbitServer(t, []mockTorrent{
		{
			Hash:     "cccc3333cccc3333cccc3333cccc3333cccc3333",
			Name:     "Heat.1995.BluRay",
			SavePath: qbit.NormalizeSlashPath(dlDir),
			State:    "stalledUP",
			Ratio:    1.1,
			Tracker:  "https://tracker.org/announce",
			Trackers: []mockTracker{{URL: "https://tracker.org/announce", Status: 2}},
			Files: []struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
			}{{Name: "movies/Heat.1995.BluRay.mkv", Size: int64(heatSize)}},
		},
	})

	radarrMock := newMockArrServer(t, "radarr")
	radarrMock.posters[10] = radarrPosterPNG
	radarrMock.posters[30] = heatPosterPNG
	radarrMock.movies = []map[string]any{
		{
			"id":        10,
			"title":     "Inception",
			"year":      2010,
			"path":      "/media/movies/Inception (2010)",
			"hasFile":   true,
			"monitored": true,
			"genres":    []string{"Sci-Fi", "Thriller"},
			"images":    []map[string]any{{"coverType": "poster", "url": "/MediaCover/10/poster.jpg"}},
			"movieFile": map[string]any{
				"id":           101,
				"path":         "/media/movies/Inception (2010)/Inception (2010).mkv",
				"relativePath": "Inception (2010).mkv",
				"size":         32 * 1024,
			},
		},
		{
			"id":        20,
			"title":     "Dune: Part Two",
			"year":      2024,
			"path":      "/media/movies/Dune Part Two (2024)",
			"hasFile":   false,
			"monitored": true,
			"genres":    []string{"Action", "Adventure"},
			"images":    []map[string]any{{"coverType": "poster", "remoteUrl": remotePosterSrv.URL + "/poster.jpg"}},
		},
		{
			"id":        30,
			"title":     "Heat",
			"year":      1995,
			"path":      "/media/movies/Heat (1995)",
			"hasFile":   false,
			"monitored": true,
			"genres":    []string{"Crime", "Drama"},
			"images":    []map[string]any{{"coverType": "poster", "url": "/MediaCover/30/poster.jpg"}},
		},
	}
	radarrMock.queue = []map[string]any{
		{
			"id":                    901,
			"movieId":               30,
			"title":                 "Heat.1995.BluRay",
			"status":                "completed",
			"trackedDownloadStatus": "warning",
			"trackedDownloadState":  "importBlocked",
			"downloadId":            "CCCC3333CCCC3333CCCC3333CCCC3333CCCC3333",
			"errorMessage":          "Upgrade blocked",
		},
	}

	sonarrMock := newMockArrServer(t, "sonarr")
	sonarrMock.posters[70] = sonarrPosterPNG
	sonarrMock.posters[71] = sonarrPosterPNG
	sonarrMock.posters[72] = sonarrPosterPNG
	sonarrMock.series = []map[string]any{
		{
			"id":        70,
			"title":     "Severance",
			"year":      2022,
			"path":      "/media/tv/Severance",
			"monitored": true,
			"genres":    []string{"Drama", "Mystery"},
			"images":    []map[string]any{{"coverType": "poster", "url": "/MediaCover/70/poster.jpg"}},
		},
		{
			"id":        71,
			"title":     "Andor",
			"year":      2022,
			"path":      "/media/tv/Andor",
			"monitored": true,
			"genres":    []string{"Action", "Sci-Fi"},
			"images":    []map[string]any{{"coverType": "poster", "url": "/MediaCover/71/poster.jpg"}},
		},
		{
			"id":        72,
			"title":     "Silo",
			"year":      2023,
			"path":      "/media/tv/Silo",
			"monitored": true,
			"genres":    []string{"Drama", "Sci-Fi"},
			"images":    []map[string]any{{"coverType": "poster", "url": "/MediaCover/72/poster.jpg"}},
		},
	}
	sonarrMock.episodeFiles["70"] = []map[string]any{
		{
			"id":           701,
			"seriesId":     70,
			"seasonNumber": 1,
			"path":         "/media/tv/Severance/Season 01/Severance - S01E01.mkv",
			"relativePath": "Season 01/Severance - S01E01.mkv",
			"size":         999999,
		},
	}
	sonarrMock.wanted = []map[string]any{
		{
			"id":            7201,
			"seriesId":      72,
			"seasonNumber":  1,
			"episodeNumber": 1,
			"title":         "Freedom Day",
			"monitored":     true,
			"hasFile":       false,
		},
	}

	cfgMgr, err := config.NewManager(filepath.Join(root, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfgMgr.Get().ShowMediaMetadata {
		t.Fatalf("expected fresh config default ShowMediaMetadata=true")
	}

	ts := httptest.NewServer(server.New(cfgMgr, qbit.NewClient(5*time.Second), arr.NewClient(5*time.Second), nil))
	t.Cleanup(ts.Close)

	// Verify GET /api/config returns default show_media_metadata=true
	var gotCfg config.AppSettings
	if c := apiDoJSON(t, ts, http.MethodGet, "/api/config", nil, &gotCfg); c != http.StatusOK {
		t.Fatalf("GET /api/config returned %d", c)
	}
	if !gotCfg.ShowMediaMetadata {
		t.Fatalf("expected GET /api/config ShowMediaMetadata=true")
	}

	// Toggle off via PUT /api/config, verify persistence, then set instances with show_media_metadata=true
	gotCfg.ShowMediaMetadata = false
	var savedCfg config.AppSettings
	if c := apiDoJSON(t, ts, http.MethodPut, "/api/config", gotCfg, &savedCfg); c != http.StatusOK {
		t.Fatalf("PUT /api/config(false) returned %d", c)
	}
	if savedCfg.ShowMediaMetadata {
		t.Fatalf("expected PUT /api/config to persist ShowMediaMetadata=false")
	}
	reloadedMgr, err := config.NewManager(filepath.Join(root, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if reloadedMgr.Get().ShowMediaMetadata {
		t.Fatalf("expected reloaded settings.json to keep ShowMediaMetadata=false")
	}

	updateCfg := config.AppSettings{
		ReadOnlyMode:      true,
		ShowMediaMetadata: true,
		Folders: []config.FolderConfig{
			{ID: "plex", Label: "Plex", Path: plexDir, Role: "primary"},
			{ID: "dl", Label: "Downloads", Path: dlDir, Role: "secondary"},
		},
		QbitInstances: []config.QbitInstanceConfig{
			{
				ID: "qb1", Name: "qBit", URL: mq.server.URL, Username: "admin", Password: "secret", Enabled: true,
				PathMappings: []config.PathMapping{{RemotePrefix: qbit.NormalizeSlashPath(dlDir), LocalPrefix: qbit.NormalizeSlashPath(dlDir)}},
			},
		},
		ArrInstances: []config.ArrInstanceConfig{
			{
				ID: "rad1", Name: "Radarr", Type: "radarr", URL: radarrMock.server.URL, APIKey: "arr-secret-key", Enabled: true,
				PathMappings: []config.PathMapping{{RemotePrefix: "/media", LocalPrefix: qbit.NormalizeSlashPath(plexDir)}},
			},
			{
				ID: "son1", Name: "Sonarr", Type: "sonarr", URL: sonarrMock.server.URL, APIKey: "arr-secret-key", Enabled: true,
				PathMappings: []config.PathMapping{{RemotePrefix: "/media", LocalPrefix: qbit.NormalizeSlashPath(plexDir)}},
			},
		},
	}
	if c := apiDoJSON(t, ts, http.MethodPut, "/api/config", updateCfg, &savedCfg); c != http.StatusOK || !savedCfg.ShowMediaMetadata {
		t.Fatalf("PUT /api/config(true) failed: code=%d saved=%+v", c, savedCfg)
	}

	var scanRes scanner.ScanResult
	if c := apiDoJSON(t, ts, http.MethodPost, "/api/scan", nil, &scanRes); c != http.StatusOK {
		t.Fatalf("POST /api/scan returned %d", c)
	}
	byName := make(map[string]scanner.ComparisonGroup, len(scanRes.Groups))
	for _, g := range scanRes.Groups {
		byName[g.Filename] = g
	}

	checkGenresAndPoster := func(name, wantSummary string, wantGenres []string) {
		t.Helper()
		g, ok := byName[name]
		if !ok {
			t.Fatalf("missing group %q in scan result", name)
		}
		if g.ArrSummary != wantSummary {
			t.Fatalf("%s: expected ArrSummary=%q, got %q (%+v)", name, wantSummary, g.ArrSummary, g.ArrMatches)
		}
		if len(g.ArrMatches) == 0 {
			t.Fatalf("%s: expected ArrMatches non-empty", name)
		}
		m := g.ArrMatches[0]
		if !m.HasPoster {
			t.Fatalf("%s: expected HasPoster=true, got false (%+v)", name, m)
		}
		if len(m.Genres) != len(wantGenres) {
			t.Fatalf("%s: expected Genres=%v, got %v", name, wantGenres, m.Genres)
		}
		for i := range wantGenres {
			if m.Genres[i] != wantGenres[i] {
				t.Fatalf("%s: expected Genres=%v, got %v", name, wantGenres, m.Genres)
			}
		}
	}

	checkGenresAndPoster("Inception (2010).mkv", "IMPORTED", []string{"Sci-Fi", "Thriller"})
	checkGenresAndPoster("Dune.Part.Two.2024.1080p.mkv", "WANTED_NOT_IMPORTED", []string{"Action", "Adventure"})
	checkGenresAndPoster("Heat.1995.BluRay.mkv", "IN_QUEUE_BLOCKED", []string{"Crime", "Drama"})
	checkGenresAndPoster("Andor.S01E01.mkv", "UNLINKED_IN_LIBRARY", []string{"Action", "Sci-Fi"})
	checkGenresAndPoster("Severance.S01E01.720p.mkv", "REPLACED_IN_ARR", []string{"Drama", "Mystery"})
	checkGenresAndPoster("Silo.S01E01.1080p.mkv", "WANTED_NOT_IMPORTED", []string{"Drama", "Sci-Fi"})

	// Verify /api/arr/poster 200 (local Arr proxy), 200 (remote fallback without leaking X-Api-Key), 404, 400
	fetchPoster := func(query string) (int, http.Header, []byte) {
		t.Helper()
		resp, err := http.Get(ts.URL + "/api/arr/poster" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, resp.Header, body
	}

	code, hdr, body := fetchPoster("?instance_id=rad1&item_id=10")
	if code != http.StatusOK || !strings.HasPrefix(hdr.Get("Content-Type"), "image/png") || !strings.Contains(hdr.Get("Cache-Control"), "max-age") || !bytes.Equal(body, radarrPosterPNG) {
		t.Fatalf("unexpected local Radarr poster response: code=%d ct=%q cc=%q len=%d", code, hdr.Get("Content-Type"), hdr.Get("Cache-Control"), len(body))
	}

	code, hdr, body = fetchPoster("?instance_id=son1&item_id=70")
	if code != http.StatusOK || !strings.HasPrefix(hdr.Get("Content-Type"), "image/png") || !bytes.Equal(body, sonarrPosterPNG) {
		t.Fatalf("unexpected local Sonarr poster response: code=%d ct=%q len=%d", code, hdr.Get("Content-Type"), len(body))
	}

	code, hdr, body = fetchPoster("?instance_id=rad1&item_id=20")
	if code != http.StatusOK || !strings.HasPrefix(hdr.Get("Content-Type"), "image/png") || !bytes.Equal(body, remotePosterPNG) {
		t.Fatalf("unexpected remote fallback poster response: code=%d ct=%q len=%d", code, hdr.Get("Content-Type"), len(body))
	}
	remoteMu.Lock()
	leakedKey := remoteSawApiKey
	remoteMu.Unlock()
	if leakedKey {
		t.Fatalf("poster proxy leaked X-Api-Key header to external remoteUrl")
	}

	if code, _, _ = fetchPoster("?instance_id=unknown&item_id=10"); code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown instance_id, got %d", code)
	}
	if code, _, _ = fetchPoster("?instance_id=son1&item_id=9999"); code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing poster item_id, got %d", code)
	}
	for _, bad := range []string{"?instance_id=rad1", "?instance_id=rad1&item_id=0", "?instance_id=rad1&item_id=abc", "?item_id=10"} {
		if code, _, _ = fetchPoster(bad); code != http.StatusBadRequest {
			t.Fatalf("expected 400 for query %q, got %d", bad, code)
		}
	}
}

