package server

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"seedlink-auditor/internal/actions"
	"seedlink-auditor/internal/arr"
	"seedlink-auditor/internal/config"
	"seedlink-auditor/internal/qbit"
	"seedlink-auditor/internal/scanner"
)

type Server struct {
	cfgMgr     *config.Manager
	qbitClient *qbit.Client
	arrClient  *arr.Client
	webFS      fs.FS
	mux        *http.ServeMux

	scanMu     sync.RWMutex
	lastScan   *scanner.ScanResult
	posterMeta map[string]arr.PosterInfo
}

func New(cfgMgr *config.Manager, qbitClient *qbit.Client, arrClient *arr.Client, webFS fs.FS) *Server {
	s := &Server{
		cfgMgr:     cfgMgr,
		qbitClient: qbitClient,
		arrClient:  arrClient,
		webFS:      webFS,
		mux:        http.NewServeMux(),
		posterMeta: make(map[string]arr.PosterInfo),
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
	s.mux.HandleFunc("POST /api/arr/test", s.handleTestArr)
	s.mux.HandleFunc("GET /api/fs/browse", s.handleBrowseFS)
	s.mux.HandleFunc("POST /api/scan", s.handleScan)
	s.mux.HandleFunc("POST /api/scan/refresh-groups", s.handleRefreshGroups)
	s.mux.HandleFunc("POST /api/actions/convert-hardlinks", s.handleConvertHardlinks)
	s.mux.HandleFunc("POST /api/actions/delete-torrents", s.handleDeleteTorrents)
	s.mux.HandleFunc("POST /api/actions/delete-files", s.handleDeleteFiles)
	s.mux.HandleFunc("POST /api/actions/arr-import", s.handleArrImport)
	s.mux.HandleFunc("POST /api/actions/arr-rescan", s.handleArrRescan)
	s.mux.HandleFunc("POST /api/actions/qbit-relocate/inspect", s.handleQbitRelocateInspect)
	s.mux.HandleFunc("POST /api/actions/qbit-relocate", s.handleQbitRelocate)
	s.mux.HandleFunc("POST /api/arr/commands/status", s.handleArrCommandsStatus)
	s.mux.HandleFunc("GET /api/arr/poster", s.handleArrPoster)
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

func (s *Server) handleTestArr(w http.ResponseWriter, r *http.Request) {
	var inst config.ArrInstanceConfig
	if err := decodeJSONBody(w, r, &inst); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON payload: %v", err))
		return
	}
	res := s.arrClient.TestConnection(r.Context(), inst)
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
	start := time.Now()
	settings := s.cfgMgr.Get()
	var scanned scanner.ScannedFolders
	var qbitIdx *qbit.Index
	var arrIdx *arr.Index

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		scanned = scanner.ScanFolders(settings)
	}()
	go func() {
		defer wg.Done()
		if s.qbitClient != nil {
			qbitIdx = s.qbitClient.BuildIndex(r.Context(), settings.QbitInstances)
		}
	}()
	go func() {
		defer wg.Done()
		if s.arrClient != nil {
			arrIdx = s.arrClient.BuildIndex(r.Context(), settings.ArrInstances)
		}
	}()
	wg.Wait()

	result := scanner.CompareScanned(settings, scanned, qbitIdx, arrIdx, start)

	s.scanMu.Lock()
	copied := result
	s.lastScan = &copied
	nextPosters := make(map[string]arr.PosterInfo)
	if arrIdx != nil {
		for k, v := range arrIdx.PosterMeta {
			nextPosters[k] = v
		}
	}
	s.posterMeta = nextPosters
	s.scanMu.Unlock()

	return result
}

func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	result := s.runAndStoreScan(r)
	writeJSON(w, http.StatusOK, result)
}

func waitForDeletedPaths(r *http.Request, paths []string) {
	nativePaths := make([]string, 0, len(paths))
	for _, p := range paths {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		nativePaths = append(nativePaths, filepath.FromSlash(trimmed))
	}
	if len(nativePaths) == 0 {
		return
	}

	deadline := time.Now().Add(1500 * time.Millisecond)
	for {
		anyExists := false
		for _, np := range nativePaths {
			if _, err := os.Lstat(np); err == nil {
				anyExists = true
				break
			}
		}
		if !anyExists || !time.Now().Before(deadline) {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(150 * time.Millisecond):
		}
	}
}

func (s *Server) handleRefreshGroups(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var payload struct {
		GroupIDs            []string            `json:"group_ids"`
		WaitForDeletedPaths []string            `json:"wait_for_deleted_paths"`
		IncludeArr          *bool               `json:"include_arr"`
		ExtraQbitHashes     map[string][]string `json:"extra_qbit_hashes"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON payload: %v", err))
		return
	}
	includeArr := true
	if payload.IncludeArr != nil {
		includeArr = *payload.IncludeArr
	}

	waitForDeletedPaths(r, payload.WaitForDeletedPaths)

	s.scanMu.RLock()
	if s.lastScan == nil {
		s.scanMu.RUnlock()
		res := s.runAndStoreScan(r)
		writeJSON(w, http.StatusOK, map[string]any{
			"full_scan":         res,
			"summary":           res.Summary,
			"updated_groups":    []scanner.ComparisonGroup{},
			"removed_group_ids": []string{},
			"duration_ms":       res.DurationMS,
		})
		return
	}

	targetSet := make(map[string]struct{}, len(payload.GroupIDs))
	for _, id := range payload.GroupIDs {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			targetSet[trimmed] = struct{}{}
		}
	}

	prevTargets := make([]scanner.ComparisonGroup, 0, len(targetSet))
	for _, g := range s.lastScan.Groups {
		if _, ok := targetSet[g.ID]; ok {
			prevTargets = append(prevTargets, g)
		}
	}
	currentSummary := s.lastScan.Summary
	s.scanMu.RUnlock()

	if len(prevTargets) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"summary":           currentSummary,
			"updated_groups":    []scanner.ComparisonGroup{},
			"removed_group_ids": []string{},
			"duration_ms":       0.0,
		})
		return
	}

	settings := s.cfgMgr.Get()
	hashesByInstance := make(map[string][]string)
	itemIDsByInstance := make(map[string][]int)

	for _, g := range prevTargets {
		for _, t := range g.Torrents {
			if t.InstanceID != "" && t.TorrentHash != "" {
				hashesByInstance[t.InstanceID] = append(hashesByInstance[t.InstanceID], t.TorrentHash)
			}
		}
		for _, am := range g.ArrMatches {
			if am.InstanceID != "" {
				if _, exists := itemIDsByInstance[am.InstanceID]; !exists {
					itemIDsByInstance[am.InstanceID] = make([]int, 0, 2)
				}
				if am.ItemID > 0 {
					itemIDsByInstance[am.InstanceID] = append(itemIDsByInstance[am.InstanceID], am.ItemID)
				}
			}
		}
	}
	for instID, extraHashes := range payload.ExtraQbitHashes {
		if strings.TrimSpace(instID) == "" {
			continue
		}
		for _, h := range extraHashes {
			if strings.TrimSpace(h) != "" {
				hashesByInstance[instID] = append(hashesByInstance[instID], h)
			}
		}
	}

	var qbitIdx *qbit.Index
	var arrIdx *arr.Index
	var discoveredArrPaths []string

	var wg sync.WaitGroup
	if s.qbitClient != nil && len(hashesByInstance) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			qbitIdx = s.qbitClient.BuildIndexForHashes(r.Context(), settings.QbitInstances, hashesByInstance)
		}()
	}
	if s.arrClient != nil && includeArr && len(itemIDsByInstance) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			arrIdx, discoveredArrPaths = s.arrClient.BuildIndexForItems(r.Context(), settings.ArrInstances, itemIDsByInstance, true)
		}()
	}
	wg.Wait()

	arrHadError := false
	if arrIdx != nil {
		for _, rep := range arrIdx.Instances {
			if !rep.OK && rep.Error != nil && *rep.Error != "Disabled" {
				arrHadError = true
				break
			}
		}
	}

	updatedGroups := make([]scanner.ComparisonGroup, 0, len(prevTargets))
	removedGroupIDs := make([]string, 0)
	updatedByID := make(map[string]scanner.ComparisonGroup, len(prevTargets))
	removedSet := make(map[string]struct{}, len(prevTargets))

	for _, prev := range prevTargets {
		updated, alive := scanner.RefreshSingleGroup(settings, prev, qbitIdx, arrIdx, discoveredArrPaths)
		if !alive {
			removedGroupIDs = append(removedGroupIDs, prev.ID)
			removedSet[prev.ID] = struct{}{}
			continue
		}
		if (!includeArr || arrHadError) && len(prev.ArrMatches) > 0 {
			updated.ArrSummary = prev.ArrSummary
			updated.HasArrIssue = prev.HasArrIssue
			updated.ArrMatches = prev.ArrMatches
		}
		updatedGroups = append(updatedGroups, updated)
		updatedByID[prev.ID] = updated
	}

	s.scanMu.Lock()
	if arrIdx != nil && len(arrIdx.PosterMeta) > 0 {
		if s.posterMeta == nil {
			s.posterMeta = make(map[string]arr.PosterInfo, len(arrIdx.PosterMeta))
		}
		for k, v := range arrIdx.PosterMeta {
			s.posterMeta[k] = v
		}
	}
	var nextSummary scanner.Summary
	if s.lastScan != nil {
		nextGroups := make([]scanner.ComparisonGroup, 0, len(s.lastScan.Groups))
		for _, g := range s.lastScan.Groups {
			if _, removed := removedSet[g.ID]; removed {
				continue
			}
			if upd, ok := updatedByID[g.ID]; ok {
				nextGroups = append(nextGroups, upd)
			} else {
				nextGroups = append(nextGroups, g)
			}
		}
		s.lastScan.Groups = nextGroups
		s.lastScan.Summary = scanner.RecalculateSummary(nextGroups)
		nextSummary = s.lastScan.Summary
	} else {
		nextSummary = scanner.RecalculateSummary(updatedGroups)
	}
	s.scanMu.Unlock()

	durationMS := math.Round(float64(time.Since(start).Microseconds())/100.0) / 10.0
	writeJSON(w, http.StatusOK, map[string]any{
		"summary":           nextSummary,
		"updated_groups":    updatedGroups,
		"removed_group_ids": removedGroupIDs,
		"duration_ms":       durationMS,
	})
}

func isArrCommandDoneStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "failed", "aborted", "cancelled", "orphaned", "error":
		return true
	default:
		return false
	}
}

func (s *Server) handleArrCommandsStatus(w http.ResponseWriter, r *http.Request) {
	type commandQuery struct {
		InstanceID string `json:"instance_id"`
		CommandID  int    `json:"command_id"`
	}
	type commandStatusOut struct {
		InstanceID string `json:"instance_id"`
		CommandID  int    `json:"command_id"`
		Name       string `json:"name,omitempty"`
		Status     string `json:"status"`
		Done       bool   `json:"done"`
		Error      string `json:"error,omitempty"`
	}

	var payload struct {
		Commands []commandQuery `json:"commands"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON payload: %v", err))
		return
	}

	settings := s.cfgMgr.Get()
	instByID := make(map[string]config.ArrInstanceConfig, len(settings.ArrInstances))
	for _, inst := range settings.ArrInstances {
		instByID[inst.ID] = inst
	}

	type uniqueKey struct {
		InstanceID string
		CommandID  int
	}
	seen := make(map[uniqueKey]struct{}, len(payload.Commands))
	queries := make([]commandQuery, 0, len(payload.Commands))
	for _, q := range payload.Commands {
		if q.InstanceID == "" || q.CommandID <= 0 {
			continue
		}
		k := uniqueKey{InstanceID: q.InstanceID, CommandID: q.CommandID}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		queries = append(queries, q)
	}

	results := make([]commandStatusOut, len(queries))
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func(pos int, cmd commandQuery) {
			defer wg.Done()
			inst, ok := instByID[cmd.InstanceID]
			if !ok || s.arrClient == nil {
				results[pos] = commandStatusOut{
					InstanceID: cmd.InstanceID,
					CommandID:  cmd.CommandID,
					Status:     "error",
					Done:       true,
					Error:      "Unknown Radarr/Sonarr instance",
				}
				return
			}
			resp, err := s.arrClient.GetCommandStatus(r.Context(), inst, cmd.CommandID)
			if err != nil {
				results[pos] = commandStatusOut{
					InstanceID: cmd.InstanceID,
					CommandID:  cmd.CommandID,
					Status:     "error",
					Done:       true,
					Error:      err.Error(),
				}
				return
			}
			status := strings.TrimSpace(resp.Status)
			if status == "" {
				status = "queued"
			}
			results[pos] = commandStatusOut{
				InstanceID: cmd.InstanceID,
				CommandID:  cmd.CommandID,
				Name:       resp.Name,
				Status:     status,
				Done:       isArrCommandDoneStatus(status),
				Error:      resp.Message,
			}
		}(i, q)
	}
	wg.Wait()

	allDone := true
	for _, r := range results {
		if !r.Done {
			allDone = false
			break
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"all_done": allDone,
		"commands": results,
	})
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

func (s *Server) handleArrImport(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Items             []actions.ArrImportItem `json:"items"`
		ForceManualImport bool                    `json:"force_manual_import"`
		ImportMode        string                  `json:"import_mode"`
		DryRun            bool                    `json:"dry_run"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON payload: %v", err))
		return
	}

	res, err := actions.TriggerArrImportAction(r.Context(), s.cfgMgr.Get(), s.arrClient, payload.Items, payload.ForceManualImport, payload.ImportMode, payload.DryRun)
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

func (s *Server) handleArrRescan(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Items  []actions.ArrRescanItem `json:"items"`
		DryRun bool                    `json:"dry_run"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON payload: %v", err))
		return
	}

	res, err := actions.TriggerArrRescanAction(r.Context(), s.cfgMgr.Get(), s.arrClient, payload.Items, payload.DryRun)
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

func (s *Server) handleQbitRelocateInspect(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Torrents           []actions.QbitInspectTorrentRef `json:"torrents"`
		SelectedLocalPaths []string                        `json:"selected_local_paths"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON payload: %v", err))
		return
	}

	res, err := actions.InspectQbitRelocateAction(r.Context(), s.cfgMgr.Get(), s.qbitClient, payload.Torrents, payload.SelectedLocalPaths)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleQbitRelocate(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Items  []actions.QbitRelocateItem `json:"items"`
		DryRun bool                       `json:"dry_run"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON payload: %v", err))
		return
	}

	res, err := actions.RelocateQbitTorrentsAction(r.Context(), s.cfgMgr.Get(), s.qbitClient, payload.Items, payload.DryRun)
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

func (s *Server) handleArrPoster(w http.ResponseWriter, r *http.Request) {
	instID := strings.TrimSpace(r.URL.Query().Get("instance_id"))
	itemIDStr := strings.TrimSpace(r.URL.Query().Get("item_id"))
	if instID == "" || itemIDStr == "" {
		writeError(w, http.StatusBadRequest, "Missing instance_id or item_id")
		return
	}
	itemID, err := strconv.Atoi(itemIDStr)
	if err != nil || itemID <= 0 {
		writeError(w, http.StatusBadRequest, "Invalid item_id")
		return
	}
	if s.arrClient == nil {
		writeError(w, http.StatusNotFound, "Radarr/Sonarr client unavailable")
		return
	}

	settings := s.cfgMgr.Get()
	var targetInst config.ArrInstanceConfig
	found := false
	for _, inst := range settings.ArrInstances {
		if inst.ID == instID && inst.Enabled {
			targetInst = inst
			found = true
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, "Unknown or disabled Radarr/Sonarr instance")
		return
	}

	s.scanMu.RLock()
	var fallback arr.PosterInfo
	if s.posterMeta != nil {
		fallback = s.posterMeta[fmt.Sprintf("%s:%d", targetInst.ID, itemID)]
	}
	s.scanMu.RUnlock()

	data, contentType, err := s.arrClient.FetchPoster(r.Context(), targetInst, itemID, fallback)
	if err != nil {
		writeError(w, http.StatusNotFound, "Poster not found")
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
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
		"ArrSummary",
		"ArrMatches",
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
			desc := fmt.Sprintf("%s: %s [%s]", t.InstanceName, t.TorrentName, t.State)
			if t.TrackerHealth != "" {
				if t.TrackerMsg != "" {
					desc = fmt.Sprintf("%s: %s [%s | %s: %s]", t.InstanceName, t.TorrentName, t.State, t.TrackerHealth, t.TrackerMsg)
				} else {
					desc = fmt.Sprintf("%s: %s [%s | %s]", t.InstanceName, t.TorrentName, t.State, t.TrackerHealth)
				}
			}
			torrents = append(torrents, desc)
			if t.TrackerHost != "" {
				trackerSet[t.TrackerHost] = struct{}{}
			}
		}
		trackers := make([]string, 0, len(trackerSet))
		for tr := range trackerSet {
			trackers = append(trackers, tr)
		}
		sort.Strings(trackers)

		arrDescs := make([]string, 0, len(grp.ArrMatches))
		for _, m := range grp.ArrMatches {
			arrDescs = append(arrDescs, fmt.Sprintf("%s (%s): %s [%s]", m.InstanceName, m.AppType, m.Title, m.MatchKind))
		}

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
			grp.ArrSummary,
			strings.Join(arrDescs, " | "),
		})
	}
	cw.Flush()

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="seedlink-audit-report.csv"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
