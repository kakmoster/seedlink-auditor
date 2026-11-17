package server_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"seedlink-auditor/internal/actions"
	"seedlink-auditor/internal/config"
	"seedlink-auditor/internal/qbit"
	"seedlink-auditor/internal/scanner"
	"seedlink-auditor/internal/server"
)

type mockTorrent struct {
	Hash     string
	Name     string
	SavePath string
	State    string
	Ratio    float64
	Tracker  string
	Files    []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
}

type mockQbitServer struct {
	mu       sync.Mutex
	torrents map[string]mockTorrent
	server   *httptest.Server
	localMap map[string]string // remote prefix -> local root for optional deleteFiles simulation
}

func newMockQbitServer(t *testing.T, initial []mockTorrent) *mockQbitServer {
	t.Helper()
	mq := &mockQbitServer{
		torrents: make(map[string]mockTorrent),
		localMap: make(map[string]string),
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
		mq.mu.Lock()
		defer mq.mu.Unlock()
		list := make([]map[string]any, 0, len(mq.torrents))
		for _, mt := range mq.torrents {
			list = append(list, map[string]any{
				"hash":      mt.Hash,
				"name":      mt.Name,
				"save_path": mt.SavePath,
				"state":     mt.State,
				"ratio":     mt.Ratio,
				"tracker":   mt.Tracker,
			})
		}
		_ = json.NewEncoder(w).Encode(list)
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

	srv := server.New(cfgMgr, qbit.NewClient(5*time.Second), nil)
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
	if andor == nil || andor.Status != "ORPHANED_IN_B" || !andor.SoleCopyOnDisk || andor.SeedingSummary != "SEEDING" {
		t.Fatalf("unexpected Andor group state: %+v", andor)
	}
	if dup == nil || dup.Status != "DUPLICATE_COPY" || !dup.CanHardlink || len(dup.HardlinkPairs) != 1 {
		t.Fatalf("unexpected Interstellar duplicate group state: %+v", dup)
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
