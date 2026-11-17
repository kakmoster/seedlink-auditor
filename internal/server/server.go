package server

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"seedlink-auditor/internal/actions"
	"seedlink-auditor/internal/config"
	"seedlink-auditor/internal/qbit"
	"seedlink-auditor/internal/scanner"
)

type Server struct {
	cfgMgr     *config.Manager
	qbitClient *qbit.Client
	webFS      fs.FS
	mux        *http.ServeMux

	scanMu   sync.RWMutex
	lastScan *scanner.ScanResult
}

func New(cfgMgr *config.Manager, qbitClient *qbit.Client, webFS fs.FS) *Server {
	s := &Server{
		cfgMgr:     cfgMgr,
		qbitClient: qbitClient,
		webFS:      webFS,
		mux:        http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /api/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/config", s.handleGetConfig)
	s.mux.HandleFunc("PUT /api/config", s.handlePutConfig)
	s.mux.HandleFunc("PATCH /api/config/read-only", s.handleToggleReadOnly)
	s.mux.HandleFunc("POST /api/qbit/test", s.handleTestQbit)
	s.mux.HandleFunc("GET /api/fs/browse", s.handleBrowseFS)
	s.mux.HandleFunc("POST /api/scan", s.handleScan)
	s.mux.HandleFunc("POST /api/actions/convert-hardlinks", s.handleConvertHardlinks)
	s.mux.HandleFunc("POST /api/actions/delete-torrents", s.handleDeleteTorrents)
	s.mux.HandleFunc("POST /api/actions/delete-files", s.handleDeleteFiles)
	s.mux.HandleFunc("GET /api/export", s.handleExport)

	if s.webFS != nil {
		staticHandler := http.StripPrefix("/static/", http.FileServer(http.FS(s.webFS)))
		s.mux.Handle("GET /static/", staticHandler)
		s.mux.HandleFunc("GET /{$}", s.handleIndex)
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"detail": msg})
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(s.webFS, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.cfgMgr.Get())
}

func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var payload config.AppSettings
	if err := decodeJSONBody(w, r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON payload: %v", err))
		return
	}
	updated, err := s.cfgMgr.Update(payload)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleToggleReadOnly(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		ReadOnlyMode bool `json:"read_only_mode"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON payload: %v", err))
		return
	}
	updated, err := s.cfgMgr.SetReadOnly(payload.ReadOnlyMode)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleTestQbit(w http.ResponseWriter, r *http.Request) {
	var inst config.QbitInstanceConfig
	if err := decodeJSONBody(w, r, &inst); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON payload: %v", err))
		return
	}
	res := s.qbitClient.TestConnection(r.Context(), inst)
	writeJSON(w, http.StatusOK, res)
}

type dirEntryDTO struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func (s *Server) handleBrowseFS(w http.ResponseWriter, r *http.Request) {
	requested := strings.TrimSpace(r.URL.Query().Get("path"))
	if requested == "" {
		if runtime.GOOS != "windows" {
			if info, err := os.Stat("/data"); err == nil && info.IsDir() {
				requested = "/data"
			} else {
				requested = "/"
			}
		} else {
			drives := make([]dirEntryDTO, 0)
			for c := 'A'; c <= 'Z'; c++ {
				root := fmt.Sprintf("%c:/", c)
				if _, err := os.Stat(root); err == nil {
					drives = append(drives, dirEntryDTO{Name: root, Path: root})
				}
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"current_path": "",
				"parent_path":  nil,
				"directories":  drives,
			})
			return
		}
	}

	info, err := os.Stat(requested)
	if err != nil || !info.IsDir() {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Directory not found: %s", requested))
		return
	}

	resolved, err := filepath.Abs(requested)
	if err != nil {
		resolved = requested
	}
	if realP, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = realP
	}

	parentDir := filepath.Dir(resolved)
	var parentOut any
	if parentDir != resolved {
		parentOut = qbit.NormalizeSlashPath(parentDir)
	} else if runtime.GOOS == "windows" {
		parentOut = ""
	} else {
		parentOut = nil
	}

	entries, err := os.ReadDir(resolved)
	if err != nil {
		writeError(w, http.StatusForbidden, fmt.Sprintf("Cannot read directory: %v", err))
		return
	}

	dirs := make([]dirEntryDTO, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dirs = append(dirs, dirEntryDTO{
			Name: entry.Name(),
			Path: qbit.NormalizeSlashPath(filepath.Join(resolved, entry.Name())),
		})
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name)
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"current_path": qbit.NormalizeSlashPath(resolved),
		"parent_path":  parentOut,
		"directories":  dirs,
	})
}

func (s *Server) runAndStoreScan(r *http.Request) scanner.ScanResult {
	settings := s.cfgMgr.Get()
	idx := s.qbitClient.BuildIndex(r.Context(), settings.QbitInstances)
	result := scanner.ScanAndCompare(settings, idx)

	s.scanMu.Lock()
	copied := result
	s.lastScan = &copied
	s.scanMu.Unlock()

	return result
}

func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	result := s.runAndStoreScan(r)
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleConvertHardlinks(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Pairs  []actions.HardlinkPairInput `json:"pairs"`
		DryRun bool                        `json:"dry_run"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON payload: %v", err))
		return
	}

	res, err := actions.ConvertCopiesToHardlinks(s.cfgMgr.Get(), payload.Pairs, payload.DryRun)
	if err != nil {
		if errors.Is(err, actions.ErrReadOnlyMode) {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleDeleteTorrents(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Items       []actions.TorrentDeleteItem `json:"items"`
		DeleteFiles *bool                       `json:"delete_files"`
		DryRun      bool                        `json:"dry_run"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON payload: %v", err))
		return
	}
	deleteFiles := true
	if payload.DeleteFiles != nil {
		deleteFiles = *payload.DeleteFiles
	}

	res, err := actions.DeleteTorrentsAction(r.Context(), s.cfgMgr.Get(), s.qbitClient, payload.Items, deleteFiles, payload.DryRun)
	if err != nil {
		if errors.Is(err, actions.ErrReadOnlyMode) {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleDeleteFiles(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		FilePaths        []string `json:"file_paths"`
		CleanupEmptyDirs *bool    `json:"cleanup_empty_dirs"`
		DryRun           bool     `json:"dry_run"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON payload: %v", err))
		return
	}
	cleanup := true
	if payload.CleanupEmptyDirs != nil {
		cleanup = *payload.CleanupEmptyDirs
	}

	res, err := actions.DeleteDiskFiles(s.cfgMgr.Get(), payload.FilePaths, cleanup, payload.DryRun)
	if err != nil {
		if errors.Is(err, actions.ErrReadOnlyMode) {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "csv"
	}

	s.scanMu.RLock()
	cached := s.lastScan
	s.scanMu.RUnlock()

	var scanRes scanner.ScanResult
	if cached != nil {
		scanRes = *cached
	} else {
		scanRes = s.runAndStoreScan(r)
	}

	if format == "json" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="seedlink-audit-report.json"`)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(scanRes)
		return
	}

	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	_ = cw.Write([]string{
		"Status",
		"Filename",
		"RelativePath",
		"SizeBytes",
		"ReclaimableBytes",
		"WastedBytes",
		"SeedingSummary",
		"SoleCopyOnDisk",
		"CanHardlink",
		"FolderAPaths",
		"FolderBPaths",
		"FolderAInodes",
		"FolderBInodes",
		"Torrents",
		"Trackers",
	})

	for _, grp := range scanRes.Groups {
		aPaths := make([]string, 0, len(grp.FolderAFiles))
		aInodes := make([]string, 0, len(grp.FolderAFiles))
		for _, f := range grp.FolderAFiles {
			aPaths = append(aPaths, f.AbsPath)
			aInodes = append(aInodes, fmt.Sprintf("%d (nlink:%d)", f.Inode, f.Nlink))
		}
		bPaths := make([]string, 0, len(grp.FolderBFiles))
		bInodes := make([]string, 0, len(grp.FolderBFiles))
		for _, f := range grp.FolderBFiles {
			bPaths = append(bPaths, f.AbsPath)
			bInodes = append(bInodes, fmt.Sprintf("%d (nlink:%d)", f.Inode, f.Nlink))
		}

		torrents := make([]string, 0, len(grp.Torrents))
		trackerSet := make(map[string]struct{})
		for _, t := range grp.Torrents {
			torrents = append(torrents, fmt.Sprintf("%s: %s [%s]", t.InstanceName, t.TorrentName, t.State))
			if t.TrackerHost != "" {
				trackerSet[t.TrackerHost] = struct{}{}
			}
		}
		trackers := make([]string, 0, len(trackerSet))
		for tr := range trackerSet {
			trackers = append(trackers, tr)
		}
		sort.Strings(trackers)

		_ = cw.Write([]string{
			grp.Status,
			grp.Filename,
			grp.RelPath,
			strconv.FormatInt(grp.Size, 10),
			strconv.FormatInt(grp.ReclaimableBytes, 10),
			strconv.FormatInt(grp.WastedBytes, 10),
			grp.SeedingSummary,
			strconv.FormatBool(grp.SoleCopyOnDisk),
			strconv.FormatBool(grp.CanHardlink),
			strings.Join(aPaths, " | "),
			strings.Join(bPaths, " | "),
			strings.Join(aInodes, " | "),
			strings.Join(bInodes, " | "),
			strings.Join(torrents, " | "),
			strings.Join(trackers, " | "),
		})
	}
	cw.Flush()

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="seedlink-audit-report.csv"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
