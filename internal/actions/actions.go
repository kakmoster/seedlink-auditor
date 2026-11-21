package actions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"seedlink-auditor/internal/arr"
	"seedlink-auditor/internal/config"
	"seedlink-auditor/internal/qbit"
	"seedlink-auditor/internal/scanner"
)

const sampleChunkSize = 64 * 1024

var ErrReadOnlyMode = errors.New("Read-Only Mode is active. Disable Read-Only Mode in the header (or run with dry_run=true) to apply changes.")

type HardlinkPairInput struct {
	SourcePath string `json:"source_path"`
	TargetPath string `json:"target_path"`
}

type ConvertedPairResult struct {
	SourcePath     string `json:"source_path"`
	TargetPath     string `json:"target_path"`
	Size           int64  `json:"size"`
	BytesReclaimed int64  `json:"bytes_reclaimed"`
}

type HardlinkError struct {
	SourcePath string `json:"source_path"`
	TargetPath string `json:"target_path"`
	Error      string `json:"error"`
}

type ConvertHardlinksResult struct {
	DryRun         bool                  `json:"dry_run"`
	ConvertedCount int                   `json:"converted_count"`
	BytesReclaimed int64                 `json:"bytes_reclaimed"`
	Converted      []ConvertedPairResult `json:"converted"`
	Errors         []HardlinkError       `json:"errors"`
}

type TorrentDeleteItem struct {
	InstanceID    string   `json:"instance_id"`
	TorrentHash   string   `json:"torrent_hash"`
	ExpectedPaths []string `json:"expected_paths"`
}

type TorrentDeleteDetail struct {
	InstanceID    string   `json:"instance_id"`
	InstanceName  string   `json:"instance_name"`
	DeletedHashes []string `json:"deleted_hashes"`
	FilesRemoved  []string `json:"files_removed"`
}

type TorrentDeleteError struct {
	InstanceID  string `json:"instance_id"`
	TorrentHash string `json:"torrent_hash"`
	Error       string `json:"error"`
}

type InspectedPathInfo struct {
	Size   int64  `json:"size"`
	Nlink  int    `json:"nlink"`
	Inode  uint64 `json:"inode"`
	Device uint64 `json:"device"`
}

type DeleteTorrentsResult struct {
	DryRun         bool                         `json:"dry_run"`
	DeleteFiles    bool                         `json:"delete_files"`
	DeletedCount   int                          `json:"deleted_count"`
	BytesReclaimed int64                        `json:"bytes_reclaimed"`
	Details        []TorrentDeleteDetail        `json:"details"`
	InspectedPaths map[string]InspectedPathInfo `json:"inspected_paths"`
	Errors         []TorrentDeleteError         `json:"errors"`
}

type DeleteFileError struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

type DeleteFilesResult struct {
	DryRun         bool              `json:"dry_run"`
	DeletedCount   int               `json:"deleted_count"`
	BytesReclaimed int64             `json:"bytes_reclaimed"`
	DeletedPaths   []string          `json:"deleted_paths"`
	RemovedDirs    []string          `json:"removed_dirs"`
	Errors         []DeleteFileError `json:"errors"`
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b)
}

func ensureWritable(settings config.AppSettings, dryRun bool) error {
	if settings.ReadOnlyMode && !dryRun {
		return ErrReadOnlyMode
	}
	return nil
}

func resolveRealPath(p string) (string, error) {
	abs, err := filepath.Abs(filepath.FromSlash(p))
	if err != nil {
		return "", err
	}
	if realP, err := filepath.EvalSymlinks(abs); err == nil {
		return realP, nil
	}
	// Walk up to the nearest existing ancestor so newly created subdirectories resolve through symlinked roots.
	var missingParts []string
	cur := abs
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		missingParts = append(missingParts, filepath.Base(cur))
		cur = parent
		if realCur, err := filepath.EvalSymlinks(cur); err == nil {
			res := realCur
			for i := len(missingParts) - 1; i >= 0; i-- {
				res = filepath.Join(res, missingParts[i])
			}
			return res, nil
		}
	}
	return abs, nil
}

func isWithinConfiguredFolders(targetPath string, folders []config.FolderConfig) *config.FolderConfig {
	resolvedTarget, err := resolveRealPath(targetPath)
	if err != nil {
		return nil
	}

	for i := range folders {
		if strings.TrimSpace(folders[i].Path) == "" {
			continue
		}
		rootResolved, rErr := resolveRealPath(folders[i].Path)
		if rErr != nil {
			continue
		}
		rel, relErr := filepath.Rel(rootResolved, resolvedTarget)
		if relErr != nil {
			continue
		}
		if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return &folders[i]
	}
	return nil
}

func removeEmptyParentDirs(rootFolderPath, removedFilePath string) []string {
	var removedDirs []string
	rootResolved, rErr := resolveRealPath(rootFolderPath)
	fileResolved, fErr := resolveRealPath(removedFilePath)
	if rErr != nil || fErr != nil {
		return removedDirs
	}
	parent := filepath.Dir(fileResolved)
	for parent != rootResolved {
		rel, relErr := filepath.Rel(rootResolved, parent)
		if relErr != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			break
		}
		if rmErr := os.Remove(parent); rmErr != nil {
			break
		}
		removedDirs = append(removedDirs, parent)
		parent = filepath.Dir(parent)
	}
	return removedDirs
}

func hashChunkAt(f *os.File, offset int64, size int) (string, error) {
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return "", err
	}
	buf := make([]byte, size)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", err
	}
	sum := sha256.Sum256(buf[:n])
	return hex.EncodeToString(sum[:]), nil
}

func sampleFileDigest(path string, size int64) ([3]string, error) {
	var out [3]string
	f, err := os.Open(path)
	if err != nil {
		return out, err
	}
	defer f.Close()

	head, err := hashChunkAt(f, 0, sampleChunkSize)
	if err != nil {
		return out, err
	}
	if size <= sampleChunkSize {
		return [3]string{head, head, head}, nil
	}

	midOffset := (size / 2) - (sampleChunkSize / 2)
	if midOffset < 0 {
		midOffset = 0
	}
	middle, err := hashChunkAt(f, midOffset, sampleChunkSize)
	if err != nil {
		return out, err
	}

	tailOffset := size - sampleChunkSize
	if tailOffset < 0 {
		tailOffset = 0
	}
	tail, err := hashChunkAt(f, tailOffset, sampleChunkSize)
	if err != nil {
		return out, err
	}

	return [3]string{head, middle, tail}, nil
}

func estimateFreedBytes(paths []string) (int64, map[string]InspectedPathInfo) {
	type inodeItem struct {
		size  int64
		nlink int
	}
	type inodeKey struct {
		device uint64
		inode  uint64
		path   string // non-empty only when inode == 0
	}

	seen := make(map[string]struct{}, len(paths))
	inodeSelected := make(map[inodeKey][]inodeItem)
	perPath := make(map[string]InspectedPathInfo)

	for _, raw := range paths {
		if raw == "" {
			continue
		}
		if _, ok := seen[raw]; ok {
			continue
		}
		seen[raw] = struct{}{}

		info, err := os.Lstat(raw)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		st, err := scanner.StatPath(raw, info)
		if err != nil {
			continue
		}

		k := inodeKey{device: st.Device, inode: st.Inode}
		if st.Inode == 0 {
			k.path = raw
		}
		inodeSelected[k] = append(inodeSelected[k], inodeItem{size: info.Size(), nlink: st.Nlink})
		perPath[raw] = InspectedPathInfo{
			Size:   info.Size(),
			Nlink:  st.Nlink,
			Inode:  st.Inode,
			Device: st.Device,
		}
	}

	var totalFreed int64
	for k, items := range inodeSelected {
		sz := items[0].size
		nlink := items[0].nlink
		if k.inode == 0 || len(items) >= nlink {
			totalFreed += sz
		}
	}
	return totalFreed, perPath
}

func ConvertCopiesToHardlinks(settings config.AppSettings, pairs []HardlinkPairInput, dryRun bool) (ConvertHardlinksResult, error) {
	if err := ensureWritable(settings, dryRun); err != nil {
		return ConvertHardlinksResult{}, err
	}

	res := ConvertHardlinksResult{
		DryRun:    dryRun,
		Converted: make([]ConvertedPairResult, 0),
		Errors:    make([]HardlinkError, 0),
	}

	for _, pair := range pairs {
		src := strings.TrimSpace(pair.SourcePath)
		tgt := strings.TrimSpace(pair.TargetPath)
		if src == "" || tgt == "" {
			res.Errors = append(res.Errors, HardlinkError{
				SourcePath: src, TargetPath: tgt, Error: "Missing source or target path.",
			})
			continue
		}

		if isWithinConfiguredFolders(src, settings.Folders) == nil || isWithinConfiguredFolders(tgt, settings.Folders) == nil {
			res.Errors = append(res.Errors, HardlinkError{
				SourcePath: src, TargetPath: tgt, Error: "Source or target path is outside configured folders.",
			})
			continue
		}

		srcInfo, sErr := os.Lstat(src)
		tgtInfo, tErr := os.Lstat(tgt)
		if sErr != nil || tErr != nil || !srcInfo.Mode().IsRegular() || !tgtInfo.Mode().IsRegular() {
			res.Errors = append(res.Errors, HardlinkError{
				SourcePath: src, TargetPath: tgt, Error: "Source or target file does not exist or is not a regular file.",
			})
			continue
		}

		if os.SameFile(srcInfo, tgtInfo) {
			res.Errors = append(res.Errors, HardlinkError{
				SourcePath: src, TargetPath: tgt, Error: "Files are already hardlinked.",
			})
			continue
		}

		srcStat, sStErr := scanner.StatPath(src, srcInfo)
		tgtStat, tStErr := scanner.StatPath(tgt, tgtInfo)
		if sStErr != nil || tStErr != nil {
			res.Errors = append(res.Errors, HardlinkError{
				SourcePath: src, TargetPath: tgt, Error: "Failed to stat source or target file.",
			})
			continue
		}

		if srcStat.Device != tgtStat.Device {
			res.Errors = append(res.Errors, HardlinkError{
				SourcePath: src, TargetPath: tgt, Error: "Files are on different filesystem devices (device ID differs); cannot hardlink.",
			})
			continue
		}

		if srcInfo.Size() != tgtInfo.Size() {
			res.Errors = append(res.Errors, HardlinkError{
				SourcePath: src, TargetPath: tgt,
				Error: fmt.Sprintf("File sizes do not match (%d vs %d).", srcInfo.Size(), tgtInfo.Size()),
			})
			continue
		}

		srcDigest, sdErr := sampleFileDigest(src, srcInfo.Size())
		tgtDigest, tdErr := sampleFileDigest(tgt, tgtInfo.Size())
		if sdErr != nil || tdErr != nil {
			res.Errors = append(res.Errors, HardlinkError{
				SourcePath: src, TargetPath: tgt, Error: "Content read failed during verification.",
			})
			continue
		}
		if srcDigest != tgtDigest {
			res.Errors = append(res.Errors, HardlinkError{
				SourcePath: src, TargetPath: tgt, Error: "Content mismatch (sample SHA-256 differs); refusing to replace target.",
			})
			continue
		}

		var reclaimed int64
		if tgtStat.Nlink <= 1 {
			reclaimed = tgtInfo.Size()
		}

		if !dryRun {
			tmpPath := filepath.Join(filepath.Dir(tgt), fmt.Sprintf("%s.seedlink_tmp_%s", filepath.Base(tgt), randomHex(4)))
			if err := os.Link(src, tmpPath); err != nil {
				res.Errors = append(res.Errors, HardlinkError{
					SourcePath: src, TargetPath: tgt, Error: fmt.Sprintf("Hardlink creation failed: %v", err),
				})
				continue
			}
			if err := os.Rename(tmpPath, tgt); err != nil {
				_ = os.Remove(tmpPath)
				res.Errors = append(res.Errors, HardlinkError{
					SourcePath: src, TargetPath: tgt, Error: fmt.Sprintf("Atomic replace failed: %v", err),
				})
				continue
			}
		}

		res.BytesReclaimed += reclaimed
		res.Converted = append(res.Converted, ConvertedPairResult{
			SourcePath:     src,
			TargetPath:     tgt,
			Size:           tgtInfo.Size(),
			BytesReclaimed: reclaimed,
		})
	}

	res.ConvertedCount = len(res.Converted)
	return res, nil
}

func DeleteTorrentsAction(
	ctx context.Context,
	settings config.AppSettings,
	qbitClient *qbit.Client,
	items []TorrentDeleteItem,
	deleteFiles bool,
	dryRun bool,
) (DeleteTorrentsResult, error) {
	if err := ensureWritable(settings, dryRun); err != nil {
		return DeleteTorrentsResult{}, err
	}

	instancesByID := make(map[string]config.QbitInstanceConfig, len(settings.QbitInstances))
	for _, inst := range settings.QbitInstances {
		instancesByID[inst.ID] = inst
	}

	groupedHashes := make(map[string][]string)
	instanceOrder := make([]string, 0)
	hashSeenPerInst := make(map[string]map[string]struct{})
	allExpectedPaths := make([]string, 0)
	errs := make([]TorrentDeleteError, 0)

	for _, it := range items {
		instID := strings.TrimSpace(it.InstanceID)
		tHash := strings.TrimSpace(it.TorrentHash)
		if _, ok := instancesByID[instID]; !ok {
			errs = append(errs, TorrentDeleteError{
				InstanceID: instID, TorrentHash: tHash, Error: "Unknown qBittorrent instance ID.",
			})
			continue
		}
		if tHash == "" {
			errs = append(errs, TorrentDeleteError{
				InstanceID: instID, TorrentHash: tHash, Error: "Missing torrent hash.",
			})
			continue
		}
		if _, exists := hashSeenPerInst[instID]; !exists {
			hashSeenPerInst[instID] = make(map[string]struct{})
			instanceOrder = append(instanceOrder, instID)
		}
		if _, dup := hashSeenPerInst[instID][tHash]; !dup {
			hashSeenPerInst[instID][tHash] = struct{}{}
			groupedHashes[instID] = append(groupedHashes[instID], tHash)
		}
		for _, p := range it.ExpectedPaths {
			if strings.TrimSpace(p) != "" {
				allExpectedPaths = append(allExpectedPaths, p)
			}
		}
	}

	var bytesFreed int64
	inspected := make(map[string]InspectedPathInfo)
	if deleteFiles {
		bytesFreed, inspected = estimateFreedBytes(allExpectedPaths)
	}

	details := make([]TorrentDeleteDetail, 0, len(instanceOrder))
	totalDeleted := 0

	if !dryRun {
		for _, instID := range instanceOrder {
			inst := instancesByID[instID]
			hashes := groupedHashes[instID]
			deletedHashes, err := qbitClient.DeleteTorrents(ctx, inst, hashes, deleteFiles)
			if err != nil {
				errs = append(errs, TorrentDeleteError{
					InstanceID: instID, TorrentHash: strings.Join(hashes, ","), Error: err.Error(),
				})
				continue
			}
			totalDeleted += len(deletedHashes)
			details = append(details, TorrentDeleteDetail{
				InstanceID:    instID,
				InstanceName:  inst.Name,
				DeletedHashes: deletedHashes,
				FilesRemoved:  []string{},
			})
		}
	} else {
		for _, instID := range instanceOrder {
			inst := instancesByID[instID]
			hashes := groupedHashes[instID]
			totalDeleted += len(hashes)
			filesPreview := []string{}
			if deleteFiles {
				seenP := make(map[string]struct{})
				for _, p := range allExpectedPaths {
					if _, ok := seenP[p]; !ok {
						seenP[p] = struct{}{}
						filesPreview = append(filesPreview, p)
					}
				}
			}
			details = append(details, TorrentDeleteDetail{
				InstanceID:    instID,
				InstanceName:  inst.Name,
				DeletedHashes: hashes,
				FilesRemoved:  filesPreview,
			})
		}
	}

	return DeleteTorrentsResult{
		DryRun:         dryRun,
		DeleteFiles:    deleteFiles,
		DeletedCount:   totalDeleted,
		BytesReclaimed: bytesFreed,
		Details:        details,
		InspectedPaths: inspected,
		Errors:         errs,
	}, nil
}

func DeleteDiskFiles(settings config.AppSettings, filePaths []string, cleanupEmptyDirs bool, dryRun bool) (DeleteFilesResult, error) {
	if err := ensureWritable(settings, dryRun); err != nil {
		return DeleteFilesResult{}, err
	}

	type validTarget struct {
		path   string
		folder config.FolderConfig
	}

	seen := make(map[string]struct{}, len(filePaths))
	validTargets := make([]validTarget, 0, len(filePaths))
	validPathStrings := make([]string, 0, len(filePaths))
	errs := make([]DeleteFileError, 0)

	for _, raw := range filePaths {
		if raw == "" {
			continue
		}
		if _, dup := seen[raw]; dup {
			continue
		}
		seen[raw] = struct{}{}

		matchedFolder := isWithinConfiguredFolders(raw, settings.Folders)
		if matchedFolder == nil {
			errs = append(errs, DeleteFileError{
				Path: raw, Error: "Path is outside configured folders or points to a root folder.",
			})
			continue
		}

		info, err := os.Lstat(raw)
		if err != nil || !info.Mode().IsRegular() {
			errs = append(errs, DeleteFileError{
				Path: raw, Error: "File does not exist or is not a regular file.",
			})
			continue
		}

		validTargets = append(validTargets, validTarget{path: raw, folder: *matchedFolder})
		validPathStrings = append(validPathStrings, raw)
	}

	bytesFreed, _ := estimateFreedBytes(validPathStrings)

	deletedPaths := make([]string, 0, len(validTargets))
	removedDirs := make([]string, 0)

	if !dryRun {
		for _, vt := range validTargets {
			if err := os.Remove(vt.path); err != nil {
				errs = append(errs, DeleteFileError{Path: vt.path, Error: err.Error()})
				continue
			}
			deletedPaths = append(deletedPaths, vt.path)

			if cleanupEmptyDirs {
				removedDirs = append(removedDirs, removeEmptyParentDirs(vt.folder.Path, vt.path)...)
			}
		}
	} else {
		deletedPaths = append(deletedPaths, validPathStrings...)
	}

	return DeleteFilesResult{
		DryRun:         dryRun,
		DeletedCount:   len(deletedPaths),
		BytesReclaimed: bytesFreed,
		DeletedPaths:   deletedPaths,
		RemovedDirs:    removedDirs,
		Errors:         errs,
	}, nil
}

type ArrImportItem struct {
	InstanceID       string `json:"instance_id"`
	LocalPath        string `json:"local_path"`
	DownloadClientID string `json:"download_client_id"`
	ItemID           int    `json:"item_id"`
	EpisodeIDs       []int  `json:"episode_ids"`
	TargetRootPath   string `json:"target_root_path,omitempty"`
}

type ArrImportEntryResult struct {
	InstanceID       string   `json:"instance_id"`
	InstanceName     string   `json:"instance_name"`
	AppType          string   `json:"app_type"`
	LocalPath        string   `json:"local_path"`
	RemotePath       string   `json:"remote_path"`
	TargetRootPath   string   `json:"target_root_path,omitempty"`
	ImportMode       string   `json:"import_mode,omitempty"`
	ParsedTitle      string   `json:"parsed_title,omitempty"`
	Rejections       []string `json:"rejections"`
	CommandName      string   `json:"command_name"`
	CommandID        int      `json:"command_id,omitempty"`
	CommandStatus    string   `json:"command_status,omitempty"`
	ManualMatchFound bool     `json:"manual_match_found"`
}

type ArrActionError struct {
	InstanceID string `json:"instance_id"`
	Target     string `json:"target"`
	Error      string `json:"error"`
}

type ArrImportResult struct {
	DryRun            bool                   `json:"dry_run"`
	ForceManualImport bool                   `json:"force_manual_import"`
	ImportMode        string                 `json:"import_mode,omitempty"`
	TriggeredCount    int                    `json:"triggered_count"`
	Results           []ArrImportEntryResult `json:"results"`
	Errors            []ArrActionError       `json:"errors"`
}

type ArrRescanItem struct {
	InstanceID string `json:"instance_id"`
	ItemID     int    `json:"item_id"`
	Title      string `json:"title"`
}

type ArrRescanEntryResult struct {
	InstanceID    string `json:"instance_id"`
	InstanceName  string `json:"instance_name"`
	AppType       string `json:"app_type"`
	ItemID        int    `json:"item_id"`
	Title         string `json:"title"`
	CommandName   string `json:"command_name"`
	CommandID     int    `json:"command_id,omitempty"`
	CommandStatus string `json:"command_status,omitempty"`
}

type ArrRescanResult struct {
	DryRun         bool                   `json:"dry_run"`
	TriggeredCount int                    `json:"triggered_count"`
	Results        []ArrRescanEntryResult `json:"results"`
	Errors         []ArrActionError       `json:"errors"`
}

func normalizeArrImportMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "copy":
		return "copy"
	case "move":
		return "move"
	default:
		return "auto"
	}
}

func TriggerArrImportAction(
	ctx context.Context,
	settings config.AppSettings,
	arrClient *arr.Client,
	items []ArrImportItem,
	forceManualImport bool,
	importMode string,
	dryRun bool,
) (ArrImportResult, error) {
	if err := ensureWritable(settings, dryRun); err != nil {
		return ArrImportResult{}, err
	}

	effMode := normalizeArrImportMode(importMode)

	instancesByID := make(map[string]config.ArrInstanceConfig, len(settings.ArrInstances))
	for _, inst := range settings.ArrInstances {
		instancesByID[inst.ID] = inst
	}

	res := ArrImportResult{
		DryRun:            dryRun,
		ForceManualImport: forceManualImport,
		ImportMode:        effMode,
		Results:           make([]ArrImportEntryResult, 0, len(items)),
		Errors:            make([]ArrActionError, 0),
	}

	type previewCacheKey struct {
		instID       string
		remoteFolder string
	}
	type previewCacheVal struct {
		items []arr.ManualImportItem
		err   error
	}
	previewCache := make(map[previewCacheKey]previewCacheVal)

	for _, it := range items {
		inst, ok := instancesByID[strings.TrimSpace(it.InstanceID)]
		if !ok {
			res.Errors = append(res.Errors, ArrActionError{
				InstanceID: it.InstanceID, Target: it.LocalPath, Error: "Unknown Radarr/Sonarr instance ID.",
			})
			continue
		}
		localPath := strings.TrimSpace(it.LocalPath)
		if localPath == "" || isWithinConfiguredFolders(localPath, settings.Folders) == nil {
			res.Errors = append(res.Errors, ArrActionError{
				InstanceID: inst.ID, Target: localPath, Error: "Local path is empty or outside configured folders.",
			})
			continue
		}

		remotePath := arr.ToRemotePath(localPath, inst.PathMappings)
		remoteFolder := arr.ToRemotePath(filepath.Dir(localPath), inst.PathMappings)
		baseName := filepath.Base(localPath)

		var matchedPreview *arr.ManualImportItem
		rejections := make([]string, 0)
		parsedTitle := ""

		ck := previewCacheKey{instID: inst.ID, remoteFolder: remoteFolder}
		cachedPreview, hit := previewCache[ck]
		if !hit {
			pItems, pErr := arrClient.PreviewManualImport(ctx, inst, remoteFolder)
			cachedPreview = previewCacheVal{items: pItems, err: pErr}
			previewCache[ck] = cachedPreview
		}
		previewItems, pErr := cachedPreview.items, cachedPreview.err
		if pErr == nil {
			normRemote := qbit.NormalizeSlashPath(remotePath)
			for i := range previewItems {
				cand := &previewItems[i]
				candNorm := qbit.NormalizeSlashPath(cand.Path)
				if strings.EqualFold(candNorm, normRemote) || strings.EqualFold(filepath.Base(candNorm), baseName) || strings.EqualFold(cand.Name, baseName) {
					matchedPreview = cand
					break
				}
			}
			if matchedPreview != nil {
				if matchedPreview.Movie != nil {
					parsedTitle = fmt.Sprintf("%s (%d)", matchedPreview.Movie.Title, matchedPreview.Movie.Year)
				} else if matchedPreview.Series != nil {
					parsedTitle = matchedPreview.Series.Title
					if len(matchedPreview.Episodes) > 0 {
						ep := matchedPreview.Episodes[0]
						parsedTitle = fmt.Sprintf("%s S%02dE%02d", matchedPreview.Series.Title, ep.SeasonNumber, ep.EpisodeNumber)
					}
				}
				for _, rej := range matchedPreview.Rejections {
					if strings.TrimSpace(rej.Reason) != "" {
						rejections = append(rejections, strings.TrimSpace(rej.Reason))
					}
				}
			}
		} else {
			rejections = append(rejections, fmt.Sprintf("ManualImport preview note: %v", pErr))
		}

		cmdName := "DownloadedMoviesScan"
		if inst.Type == "sonarr" {
			cmdName = "DownloadedEpisodesScan"
		}
		if forceManualImport && matchedPreview != nil {
			cmdName = "ManualImport"
		}

		entryRes := ArrImportEntryResult{
			InstanceID:       inst.ID,
			InstanceName:     inst.Name,
			AppType:          inst.Type,
			LocalPath:        localPath,
			RemotePath:       remotePath,
			TargetRootPath:   strings.TrimSpace(it.TargetRootPath),
			ImportMode:       effMode,
			ParsedTitle:      parsedTitle,
			Rejections:       rejections,
			CommandName:      cmdName,
			ManualMatchFound: matchedPreview != nil,
		}

		if !dryRun {
			var cmdResp arr.CommandResponse
			var cmdErr error
			if forceManualImport && matchedPreview != nil {
				dlID := matchedPreview.DownloadID
				if dlID == "" && strings.TrimSpace(it.DownloadClientID) != "" {
					dlID = strings.ToUpper(strings.TrimSpace(it.DownloadClientID))
				}
				payload := arr.ManualImportFilePayload{
					Path:         matchedPreview.Path,
					FolderName:   filepath.Base(filepath.Dir(matchedPreview.Path)),
					Quality:      matchedPreview.Quality,
					Languages:    matchedPreview.Languages,
					ReleaseGroup: matchedPreview.ReleaseGroup,
					IndexerFlags: matchedPreview.IndexerFlags,
					DownloadID:   dlID,
				}
				if inst.Type == "radarr" {
					if matchedPreview.Movie != nil && matchedPreview.Movie.ID > 0 {
						payload.MovieID = matchedPreview.Movie.ID
					} else if it.ItemID > 0 {
						payload.MovieID = it.ItemID
					}
				} else {
					if matchedPreview.Series != nil && matchedPreview.Series.ID > 0 {
						payload.SeriesID = matchedPreview.Series.ID
					} else if it.ItemID > 0 {
						payload.SeriesID = it.ItemID
					}
					if len(matchedPreview.Episodes) > 0 {
						for _, ep := range matchedPreview.Episodes {
							payload.EpisodeIDs = append(payload.EpisodeIDs, ep.ID)
						}
					} else if len(it.EpisodeIDs) > 0 {
						payload.EpisodeIDs = it.EpisodeIDs
					}
				}
				cmdResp, cmdErr = arrClient.TriggerManualImport(ctx, inst, []arr.ManualImportFilePayload{payload}, effMode)
			} else {
				cmdResp, cmdErr = arrClient.TriggerDownloadScan(ctx, inst, remotePath, strings.ToUpper(strings.TrimSpace(it.DownloadClientID)), effMode)
			}
			if cmdErr != nil {
				res.Errors = append(res.Errors, ArrActionError{
					InstanceID: inst.ID, Target: remotePath, Error: cmdErr.Error(),
				})
				continue
			}
			entryRes.CommandID = cmdResp.ID
			entryRes.CommandStatus = cmdResp.Status
		}

		res.Results = append(res.Results, entryRes)
	}

	res.TriggeredCount = len(res.Results)
	return res, nil
}

func TriggerArrRescanAction(
	ctx context.Context,
	settings config.AppSettings,
	arrClient *arr.Client,
	items []ArrRescanItem,
	dryRun bool,
) (ArrRescanResult, error) {
	if err := ensureWritable(settings, dryRun); err != nil {
		return ArrRescanResult{}, err
	}

	instancesByID := make(map[string]config.ArrInstanceConfig, len(settings.ArrInstances))
	for _, inst := range settings.ArrInstances {
		instancesByID[inst.ID] = inst
	}

	res := ArrRescanResult{
		DryRun:  dryRun,
		Results: make([]ArrRescanEntryResult, 0, len(items)),
		Errors:  make([]ArrActionError, 0),
	}

	seen := make(map[string]struct{}, len(items))
	for _, it := range items {
		inst, ok := instancesByID[strings.TrimSpace(it.InstanceID)]
		if !ok {
			res.Errors = append(res.Errors, ArrActionError{
				InstanceID: it.InstanceID, Target: it.Title, Error: "Unknown Radarr/Sonarr instance ID.",
			})
			continue
		}
		if it.ItemID <= 0 {
			res.Errors = append(res.Errors, ArrActionError{
				InstanceID: inst.ID, Target: it.Title, Error: "Missing Radarr/Sonarr item ID.",
			})
			continue
		}
		dedupKey := fmt.Sprintf("%s:%d", inst.ID, it.ItemID)
		if _, dup := seen[dedupKey]; dup {
			continue
		}
		seen[dedupKey] = struct{}{}

		cmdName := "RefreshMovie"
		if inst.Type == "sonarr" {
			cmdName = "RescanSeries"
		}
		entry := ArrRescanEntryResult{
			InstanceID:   inst.ID,
			InstanceName: inst.Name,
			AppType:      inst.Type,
			ItemID:       it.ItemID,
			Title:        it.Title,
			CommandName:  cmdName,
		}
		if !dryRun {
			cmdResp, cmdErr := arrClient.TriggerRescan(ctx, inst, it.ItemID)
			if cmdErr != nil {
				res.Errors = append(res.Errors, ArrActionError{
					InstanceID: inst.ID, Target: it.Title, Error: cmdErr.Error(),
				})
				continue
			}
			entry.CommandID = cmdResp.ID
			entry.CommandStatus = cmdResp.Status
		}
		res.Results = append(res.Results, entry)
	}

	res.TriggeredCount = len(res.Results)
	return res, nil
}

var (
	videoExtSet = map[string]struct{}{
		".mkv": {}, ".mp4": {}, ".avi": {}, ".m4v": {}, ".ts": {}, ".m2ts": {},
		".mov": {}, ".wmv": {}, ".flv": {}, ".webm": {}, ".mpg": {}, ".mpeg": {},
		".vob": {}, ".iso": {}, ".img": {},
	}
	subtitleExtSet = map[string]struct{}{
		".srt": {}, ".ass": {}, ".ssa": {}, ".sub": {}, ".idx": {}, ".vtt": {},
		".sup": {}, ".smi": {}, ".pgs": {},
	}
	metadataExtSet = map[string]struct{}{
		".nfo": {}, ".txt": {}, ".sfv": {}, ".md5": {}, ".sha1": {}, ".sha256": {},
		".jpg": {}, ".jpeg": {}, ".png": {}, ".webp": {}, ".gif": {}, ".bmp": {},
		".tbn": {}, ".xml": {}, ".srr": {}, ".srs": {}, ".nzb": {}, ".cue": {},
		".log": {}, ".m3u": {}, ".m3u8": {}, ".url": {}, ".diz": {},
	}
	sampleDirNames = map[string]struct{}{
		"sample": {}, "samples": {}, "proof": {}, "extras": {}, "featurettes": {}, "bonus": {},
	}
)

// ClassifyTorrentFileKind categorizes a torrent-relative path as sample, video, subtitle, metadata, or other.
func ClassifyTorrentFileKind(relPath string) string {
	norm := strings.TrimLeft(qbit.NormalizeSlashPath(relPath), "/")
	if norm == "" {
		return "other"
	}
	segments := strings.Split(norm, "/")
	if len(segments) >= 3 {
		// Check interior directories (skip top-level release folder name so shows titled "Sample" aren't flagged).
		for _, seg := range segments[1 : len(segments)-1] {
			if _, ok := sampleDirNames[strings.ToLower(seg)]; ok {
				return "sample"
			}
		}
	}

	base := segments[len(segments)-1]
	ext := strings.ToLower(path.Ext(base))
	stem := strings.ToLower(strings.TrimSuffix(base, ext))
	if stem == "sample" || stem == "proof" ||
		strings.HasPrefix(stem, "sample-") || strings.HasPrefix(stem, "sample.") || strings.HasPrefix(stem, "sample_") ||
		strings.HasSuffix(stem, "-sample") || strings.HasSuffix(stem, ".sample") || strings.HasSuffix(stem, "_sample") ||
		strings.HasSuffix(stem, " sample") {
		return "sample"
	}

	if _, ok := videoExtSet[ext]; ok {
		return "video"
	}
	if _, ok := subtitleExtSet[ext]; ok {
		return "subtitle"
	}
	if _, ok := metadataExtSet[ext]; ok {
		return "metadata"
	}
	return "other"
}

type QbitInspectTorrentRef struct {
	InstanceID  string `json:"instance_id"`
	TorrentHash string `json:"torrent_hash"`
}

type QbitInspectedFile struct {
	RelPath      string `json:"rel_path"`
	LocalPath    string `json:"local_path"`
	Filename     string `json:"filename"`
	Ext          string `json:"ext"`
	Size         int64  `json:"size"`
	Kind         string `json:"kind"`
	RawKind      string `json:"raw_kind"`
	ExistsOnDisk bool   `json:"exists_on_disk"`
	Device       uint64 `json:"device,omitempty"`
	InodeStr     string `json:"inode_str,omitempty"`
}

type QbitInspectedTorrent struct {
	InstanceID           string              `json:"instance_id"`
	InstanceName         string              `json:"instance_name"`
	TorrentHash          string              `json:"torrent_hash"`
	TorrentName          string              `json:"torrent_name"`
	SavePath             string              `json:"save_path"`
	FileCount            int                 `json:"file_count"`
	SelectedCount        int                 `json:"selected_count"`
	UnselectedVideoCount int                 `json:"unselected_video_count"`
	CompanionCount       int                 `json:"companion_count"`
	SampleCount          int                 `json:"sample_count"`
	OtherCount           int                 `json:"other_count"`
	MissingCount         int                 `json:"missing_count"`
	Status               string              `json:"status"`
	CanRelocate          bool                `json:"can_relocate"`
	Warnings             []string            `json:"warnings"`
	Files                []QbitInspectedFile `json:"files"`
}

type QbitInspectResult struct {
	Torrents []QbitInspectedTorrent `json:"torrents"`
	Errors   []QbitRelocateError    `json:"errors"`
}

type devInodeKey struct {
	Device uint64
	Inode  uint64
}

// InspectQbitRelocateAction performs a read-only inspection of all files inside target torrents,
// detecting unselected sibling videos, companion .nfo/.srt files, and sample/extra files before a Move.
func InspectQbitRelocateAction(
	ctx context.Context,
	settings config.AppSettings,
	qbitClient *qbit.Client,
	torrents []QbitInspectTorrentRef,
	selectedLocalPaths []string,
) (QbitInspectResult, error) {
	res := QbitInspectResult{
		Torrents: make([]QbitInspectedTorrent, 0, len(torrents)),
		Errors:   make([]QbitRelocateError, 0),
	}
	if qbitClient == nil || len(torrents) == 0 {
		return res, nil
	}

	instancesByID := make(map[string]config.QbitInstanceConfig, len(settings.QbitInstances))
	for _, inst := range settings.QbitInstances {
		instancesByID[inst.ID] = inst
	}

	selectedPathSet := make(map[string]struct{}, len(selectedLocalPaths))
	selectedInodes := make(map[devInodeKey]struct{}, len(selectedLocalPaths))
	for _, p := range selectedLocalPaths {
		norm := qbit.NormalizeSlashPath(p)
		if norm == "" {
			continue
		}
		selectedPathSet[strings.ToLower(norm)] = struct{}{}
		if info, err := os.Lstat(filepath.FromSlash(norm)); err == nil && info.Mode().IsRegular() {
			if st, sErr := scanner.StatPath(filepath.FromSlash(norm), info); sErr == nil && st.Inode != 0 {
				selectedInodes[devInodeKey{Device: st.Device, Inode: st.Inode}] = struct{}{}
			}
		}
	}

	seen := make(map[string]struct{}, len(torrents))
	orderedRefs := make([]QbitInspectTorrentRef, 0, len(torrents))
	hashesByInst := make(map[string][]string)

	for _, ref := range torrents {
		instID := strings.TrimSpace(ref.InstanceID)
		tHash := strings.TrimSpace(ref.TorrentHash)
		if _, ok := instancesByID[instID]; !ok {
			res.Errors = append(res.Errors, QbitRelocateError{
				InstanceID: instID, TorrentHash: tHash, Error: "Unknown qBittorrent instance ID.",
			})
			continue
		}
		if tHash == "" {
			res.Errors = append(res.Errors, QbitRelocateError{
				InstanceID: instID, TorrentHash: tHash, Error: "Missing torrent hash.",
			})
			continue
		}
		k := instID + ":" + strings.ToLower(tHash)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		orderedRefs = append(orderedRefs, QbitInspectTorrentRef{InstanceID: instID, TorrentHash: tHash})
		hashesByInst[instID] = append(hashesByInst[instID], tHash)
	}

	detailsByInst := make(map[string]map[string]qbit.TorrentDetails, len(hashesByInst))
	fetchErrByInst := make(map[string]error, len(hashesByInst))
	var wg sync.WaitGroup
	var mu sync.Mutex

	for instID, hashes := range hashesByInst {
		wg.Add(1)
		go func(id string, targetHashes []string) {
			defer wg.Done()
			dm, err := qbitClient.FetchTorrentsDetails(ctx, instancesByID[id], targetHashes)
			mu.Lock()
			if err != nil {
				fetchErrByInst[id] = err
			} else {
				detailsByInst[id] = dm
			}
			mu.Unlock()
		}(instID, hashes)
	}
	wg.Wait()

	for _, ref := range orderedRefs {
		if fErr, hasErr := fetchErrByInst[ref.InstanceID]; hasErr {
			res.Errors = append(res.Errors, QbitRelocateError{
				InstanceID: ref.InstanceID, TorrentHash: ref.TorrentHash, Error: fErr.Error(),
			})
			continue
		}
		details, ok := detailsByInst[ref.InstanceID][strings.ToLower(ref.TorrentHash)]
		if !ok {
			res.Errors = append(res.Errors, QbitRelocateError{
				InstanceID: ref.InstanceID, TorrentHash: ref.TorrentHash, Error: "Torrent not found in qBittorrent.",
			})
			continue
		}

		inst := instancesByID[ref.InstanceID]
		inspected := QbitInspectedTorrent{
			InstanceID:   ref.InstanceID,
			InstanceName: inst.Name,
			TorrentHash:  details.Hash,
			TorrentName:  details.Name,
			SavePath:     details.SavePath,
			FileCount:    len(details.Files),
			Warnings:     make([]string, 0),
			Files:        make([]QbitInspectedFile, 0, len(details.Files)),
		}

		var missingNames, companionNames, sampleNames []string
		selectedVideoCount := 0

		for _, tf := range details.Files {
			relPath := strings.TrimLeft(qbit.NormalizeSlashPath(tf.Name), "/")
			combined := relPath
			if details.SavePath != "" {
				combined = details.SavePath + "/" + relPath
			}
			localPath := qbit.NormalizeSlashPath(qbit.ApplyPathMappings(combined, inst.PathMappings))
			rawKind := ClassifyTorrentFileKind(relPath)
			fname := path.Base(relPath)
			ext := strings.ToLower(path.Ext(fname))

			existsOnDisk := false
			var dev, inode uint64
			var inodeStr string
			if info, statErr := os.Lstat(filepath.FromSlash(localPath)); statErr == nil && info.Mode().IsRegular() {
				existsOnDisk = true
				if st, sErr := scanner.StatPath(filepath.FromSlash(localPath), info); sErr == nil {
					dev = st.Device
					inode = st.Inode
					inodeStr = strconv.FormatUint(st.Inode, 10)
				}
			} else {
				inspected.MissingCount++
				if len(missingNames) < 3 {
					missingNames = append(missingNames, relPath)
				}
			}

			_, byPath := selectedPathSet[strings.ToLower(localPath)]
			_, byInode := selectedInodes[devInodeKey{Device: dev, Inode: inode}]
			isSelected := byPath || (existsOnDisk && inode != 0 && byInode)

			kind := rawKind
			if isSelected {
				kind = "selected"
				inspected.SelectedCount++
				if rawKind == "video" {
					selectedVideoCount++
				}
			} else {
				switch rawKind {
				case "video":
					inspected.UnselectedVideoCount++
				case "metadata", "subtitle":
					inspected.CompanionCount++
					if len(companionNames) < 4 {
						companionNames = append(companionNames, fname)
					}
				case "sample":
					inspected.SampleCount++
					if len(sampleNames) < 3 {
						sampleNames = append(sampleNames, relPath)
					}
				default:
					inspected.OtherCount++
				}
			}

			inspected.Files = append(inspected.Files, QbitInspectedFile{
				RelPath:      relPath,
				LocalPath:    localPath,
				Filename:     fname,
				Ext:          ext,
				Size:         tf.Size,
				Kind:         kind,
				RawKind:      rawKind,
				ExistsOnDisk: existsOnDisk,
				Device:       dev,
				InodeStr:     inodeStr,
			})
		}

		switch {
		case inspected.MissingCount > 0:
			inspected.Status = "missing_files"
			inspected.CanRelocate = false
			inspected.Warnings = append(inspected.Warnings, fmt.Sprintf(
				"%d file(s) in this torrent are missing on disk (%s) — cannot relocate cleanly.",
				inspected.MissingCount, strings.Join(missingNames, ", "),
			))
		case inspected.SelectedCount == 0:
			inspected.Status = "no_selected_match"
			inspected.CanRelocate = false
			inspected.Warnings = append(inspected.Warnings,
				"None of the selected Move files matched files inside this torrent.")
		case inspected.UnselectedVideoCount > 0:
			inspected.Status = "partial_pack"
			inspected.CanRelocate = false
			totalVideos := selectedVideoCount + inspected.UnselectedVideoCount
			inspected.Warnings = append(inspected.Warnings, fmt.Sprintf(
				"Only %d of %d video file(s) in this torrent are selected for Move. Include all sibling episodes in the pack to relocate cleanly without breaking seeding.",
				selectedVideoCount, totalVideos,
			))
		case inspected.SampleCount > 0:
			inspected.Status = "has_samples"
			inspected.CanRelocate = true
			inspected.Warnings = append(inspected.Warnings, fmt.Sprintf(
				"%d sample/extra file(s) detected (%s). Moving samples into your media library is off by default; keep them excluded unless you want them moved.",
				inspected.SampleCount, strings.Join(sampleNames, ", "),
			))
			if inspected.CompanionCount > 0 {
				inspected.Warnings = append(inspected.Warnings, fmt.Sprintf(
					"%d companion file(s) (.nfo/.srt) found (%s) and will be moved alongside so seeding stays at 100%%.",
					inspected.CompanionCount, strings.Join(companionNames, ", "),
				))
			}
		case inspected.CompanionCount > 0:
			inspected.Status = "ready_with_companions"
			inspected.CanRelocate = true
			inspected.Warnings = append(inspected.Warnings, fmt.Sprintf(
				"%d companion file(s) (.nfo, .srt, etc.) detected (%s) and can be moved alongside so qBittorrent keeps 100%% seeding.",
				inspected.CompanionCount, strings.Join(companionNames, ", "),
			))
		default:
			inspected.Status = "ready"
			inspected.CanRelocate = true
		}

		res.Torrents = append(res.Torrents, inspected)
	}

	return res, nil
}

type QbitRelocateFileMapping struct {
	OldRelPath   string `json:"old_rel_path"`
	OldLocalPath string `json:"old_local_path,omitempty"`
	NewLocalPath string `json:"new_local_path,omitempty"`
	Kind         string `json:"kind,omitempty"`
	Device       uint64 `json:"device,omitempty"`
	InodeStr     string `json:"inode_str,omitempty"`
	Size         int64  `json:"size,omitempty"`
}

type QbitRelocateItem struct {
	InstanceID       string                    `json:"instance_id"`
	TorrentHash      string                    `json:"torrent_hash"`
	NewLocalPath     string                    `json:"new_local_path,omitempty"`
	FileMappings     []QbitRelocateFileMapping `json:"file_mappings,omitempty"`
	MoveCompanions   bool                      `json:"move_companions,omitempty"`
	MoveSamples      bool                      `json:"move_samples,omitempty"`
	CleanupEmptyDirs *bool                     `json:"cleanup_empty_dirs,omitempty"`
}

type QbitRelocateError struct {
	InstanceID  string `json:"instance_id"`
	TorrentHash string `json:"torrent_hash"`
	Error       string `json:"error"`
}

type QbitRelocateResult struct {
	DryRun         bool                          `json:"dry_run"`
	RelocatedCount int                           `json:"relocated_count"`
	Results        []qbit.RelocateTorrentOutcome `json:"results"`
	Errors         []QbitRelocateError           `json:"errors"`
}

type targetDirCandidate struct {
	absPath  string
	filename string
	ext      string
	size     int64
	device   uint64
	inode    uint64
}

func scanTargetPackCandidates(targetDir string) []targetDirCandidate {
	nativeRoot := filepath.FromSlash(qbit.NormalizeSlashPath(targetDir))
	info, err := os.Stat(nativeRoot)
	if err != nil || !info.IsDir() {
		return nil
	}
	var out []targetDirCandidate
	_ = filepath.WalkDir(nativeRoot, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		rel, relErr := filepath.Rel(nativeRoot, p)
		if relErr != nil {
			return nil
		}
		if d.IsDir() {
			if p != nativeRoot {
				if d.Type()&os.ModeSymlink != 0 {
					return filepath.SkipDir
				}
				depth := strings.Count(filepath.ToSlash(rel), "/") + 1
				if depth > 3 {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		stInfo, sErr := d.Info()
		if sErr != nil {
			return nil
		}
		fstat, stErr := scanner.StatPath(p, stInfo)
		if stErr != nil {
			return nil
		}
		out = append(out, targetDirCandidate{
			absPath:  qbit.NormalizeSlashPath(p),
			filename: d.Name(),
			ext:      strings.ToLower(filepath.Ext(d.Name())),
			size:     stInfo.Size(),
			device:   fstat.Device,
			inode:    fstat.Inode,
		})
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		return out[i].absPath < out[j].absPath
	})
	return out
}

func findMovedCandidate(
	candidates []targetDirCandidate,
	claimed map[string]struct{},
	dev uint64,
	inode uint64,
	origName string,
	ext string,
	size int64,
) (string, bool) {
	if inode != 0 {
		for _, c := range candidates {
			if _, used := claimed[strings.ToLower(c.absPath)]; used {
				continue
			}
			if c.inode == inode && (dev == 0 || c.device == dev) {
				return c.absPath, true
			}
		}
	}
	for _, c := range candidates {
		if _, used := claimed[strings.ToLower(c.absPath)]; used {
			continue
		}
		if strings.EqualFold(c.filename, origName) && (size <= 0 || c.size == size) {
			return c.absPath, true
		}
	}
	if size > 0 && ext != "" {
		var matches []targetDirCandidate
		for _, c := range candidates {
			if _, used := claimed[strings.ToLower(c.absPath)]; used {
				continue
			}
			if strings.EqualFold(c.ext, ext) && c.size == size {
				matches = append(matches, c)
			}
		}
		if len(matches) == 1 {
			return matches[0].absPath, true
		}
	}
	return "", false
}

func moveFileSafe(srcPath, dstPath string) error {
	srcNative := filepath.FromSlash(srcPath)
	dstNative := filepath.FromSlash(dstPath)
	if err := os.MkdirAll(filepath.Dir(dstNative), 0o755); err != nil {
		return err
	}
	if err := os.Rename(srcNative, dstNative); err == nil {
		return nil
	} else if err != nil {
		// Fallback for cross-device moves (copy to temp file on target device, sync, atomic rename, remove source).
		in, openErr := os.Open(srcNative)
		if openErr != nil {
			return err
		}
		defer in.Close()

		tmpNative := dstNative + ".seedlink_mv_" + randomHex(4)
		out, createErr := os.OpenFile(tmpNative, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
		if createErr != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		syncErr := out.Sync()
		closeErr := out.Close()
		if copyErr != nil || syncErr != nil || closeErr != nil {
			_ = os.Remove(tmpNative)
			if copyErr != nil {
				return copyErr
			}
			if syncErr != nil {
				return syncErr
			}
			return closeErr
		}
		in.Close()

		if renErr := os.Rename(tmpNative, dstNative); renErr != nil {
			_ = os.Remove(tmpNative)
			return renErr
		}
		if rmErr := os.Remove(srcNative); rmErr != nil {
			return fmt.Errorf("moved copy to %s but failed to remove original %s: %w", dstNative, srcNative, rmErr)
		}
	}
	return nil
}

type pendingCompanionMove struct {
	oldLocal  string
	destLocal string
	rawKind   string
	oldFolder config.FolderConfig
}

func relocatePackWithMappings(
	ctx context.Context,
	settings config.AppSettings,
	qbitClient *qbit.Client,
	inst config.QbitInstanceConfig,
	it QbitRelocateItem,
	dryRun bool,
) (qbit.RelocateTorrentOutcome, error) {
	tHash := strings.TrimSpace(it.TorrentHash)
	details, err := qbitClient.FetchTorrentDetails(ctx, inst, tHash)
	if err != nil {
		return qbit.RelocateTorrentOutcome{}, err
	}
	if len(details.Files) == 0 {
		return qbit.RelocateTorrentOutcome{}, errors.New("torrent contains no files")
	}

	byOldRel := make(map[string]QbitRelocateFileMapping, len(it.FileMappings))
	byOldRelLower := make(map[string]QbitRelocateFileMapping, len(it.FileMappings))
	for _, m := range it.FileMappings {
		nk := strings.TrimLeft(qbit.NormalizeSlashPath(m.OldRelPath), "/")
		if nk == "" {
			continue
		}
		m.OldRelPath = nk
		byOldRel[nk] = m
		byOldRelLower[strings.ToLower(nk)] = m
	}

	movedLocalPaths := make([]string, 0, len(it.FileMappings))
	oldLocalPaths := make([]string, 0, len(details.Files))
	claimedTargets := make(map[string]struct{}, len(it.FileMappings))
	var movedFolder *config.FolderConfig

	resolvedLocal := make(map[string]string, len(details.Files))
	for _, tf := range details.Files {
		oldRel := strings.TrimLeft(qbit.NormalizeSlashPath(tf.Name), "/")
		combined := oldRel
		if details.SavePath != "" {
			combined = details.SavePath + "/" + oldRel
		}
		oldLocal := qbit.NormalizeSlashPath(qbit.ApplyPathMappings(combined, inst.PathMappings))
		oldLocalPaths = append(oldLocalPaths, oldLocal)

		m, hasMap := byOldRel[oldRel]
		if !hasMap {
			m, hasMap = byOldRelLower[strings.ToLower(oldRel)]
		}
		if hasMap && strings.TrimSpace(m.NewLocalPath) != "" {
			normNew := qbit.NormalizeSlashPath(m.NewLocalPath)
			matchedFolder := isWithinConfiguredFolders(normNew, settings.Folders)
			if matchedFolder == nil {
				return qbit.RelocateTorrentOutcome{}, fmt.Errorf("target path %s is outside configured folders", normNew)
			}
			info, statErr := os.Lstat(filepath.FromSlash(normNew))
			if statErr != nil || !info.Mode().IsRegular() {
				return qbit.RelocateTorrentOutcome{}, fmt.Errorf("moved target file does not exist on disk: %s", normNew)
			}
			if movedFolder == nil {
				movedFolder = matchedFolder
			}
			resolvedLocal[oldRel] = normNew
			movedLocalPaths = append(movedLocalPaths, normNew)
			claimedTargets[strings.ToLower(normNew)] = struct{}{}
		}
	}

	if len(movedLocalPaths) == 0 {
		return qbit.RelocateTorrentOutcome{}, errors.New("no moved target files provided for torrent relocation")
	}

	caseInsensitive := runtime.GOOS == "windows"
	targetPackDir := qbit.CommonSlashDir(movedLocalPaths, caseInsensitive)
	oldPackDir := qbit.CommonSlashDir(oldLocalPaths, caseInsensitive)
	targetCandidates := scanTargetPackCandidates(targetPackDir)

	var pendingMoves []pendingCompanionMove
	var warnings []string

	for _, tf := range details.Files {
		oldRel := strings.TrimLeft(qbit.NormalizeSlashPath(tf.Name), "/")
		if _, alreadyResolved := resolvedLocal[oldRel]; alreadyResolved {
			continue
		}
		combined := oldRel
		if details.SavePath != "" {
			combined = details.SavePath + "/" + oldRel
		}
		oldLocal := qbit.NormalizeSlashPath(qbit.ApplyPathMappings(combined, inst.PathMappings))
		if isWithinConfiguredFolders(oldLocal, settings.Folders) == nil && oldPackDir == "" {
			return qbit.RelocateTorrentOutcome{}, fmt.Errorf("file %s resolves outside configured folders", oldLocal)
		}

		m, hasMap := byOldRel[oldRel]
		if !hasMap {
			m, hasMap = byOldRelLower[strings.ToLower(oldRel)]
		}
		var prevInode uint64
		if hasMap && m.InodeStr != "" {
			if parsed, pErr := strconv.ParseUint(m.InodeStr, 10, 64); pErr == nil {
				prevInode = parsed
			}
		}

		rawKind := ClassifyTorrentFileKind(oldRel)
		oldNative := filepath.FromSlash(oldLocal)
		oldInfo, oldErr := os.Lstat(oldNative)

		// Case 1: File no longer exists at oldLocal (already moved by Arr or a sibling cross-seed torrent).
		if oldErr != nil || !oldInfo.Mode().IsRegular() {
			foundPath, ok := findMovedCandidate(
				targetCandidates,
				claimedTargets,
				m.Device,
				prevInode,
				path.Base(oldRel),
				strings.ToLower(path.Ext(oldRel)),
				tf.Size,
			)
			if !ok {
				return qbit.RelocateTorrentOutcome{}, fmt.Errorf(
					"cannot relocate torrent: file %s is missing on disk and was not found in %s",
					oldRel, targetPackDir,
				)
			}
			resolvedLocal[oldRel] = foundPath
			claimedTargets[strings.ToLower(foundPath)] = struct{}{}
			continue
		}

		// Case 2: File still exists at oldLocal.
		shouldMove := ((rawKind == "metadata" || rawKind == "subtitle") && it.MoveCompanions) ||
			(rawKind == "sample" && it.MoveSamples)

		oldFolder := isWithinConfiguredFolders(oldLocal, settings.Folders)
		if shouldMove {
			if oldFolder == nil {
				return qbit.RelocateTorrentOutcome{}, fmt.Errorf("companion file %s is outside configured folders", oldLocal)
			}
			subRel, hasSub := stripSlashSubRel(oldPackDir, oldLocal, caseInsensitive)
			if !hasSub || subRel == "" || strings.HasPrefix(subRel, "../") || strings.Contains(subRel, "/../") {
				subRel = path.Base(oldRel)
			}
			destLocal := qbit.NormalizeSlashPath(targetPackDir + "/" + strings.TrimLeft(subRel, "/"))
			if isWithinConfiguredFolders(destLocal, settings.Folders) == nil {
				return qbit.RelocateTorrentOutcome{}, fmt.Errorf("destination companion path %s is outside configured folders", destLocal)
			}

			destNative := filepath.FromSlash(destLocal)
			if destInfo, dErr := os.Lstat(destNative); dErr == nil {
				if os.SameFile(oldInfo, destInfo) {
					resolvedLocal[oldRel] = destLocal
					claimedTargets[strings.ToLower(destLocal)] = struct{}{}
					continue
				}
				if destInfo.Mode().IsRegular() && destInfo.Size() == oldInfo.Size() {
					srcDig, sErr := sampleFileDigest(oldNative, oldInfo.Size())
					dstDig, dDigErr := sampleFileDigest(destNative, destInfo.Size())
					if sErr == nil && dDigErr == nil && srcDig == dstDig {
						resolvedLocal[oldRel] = destLocal
						claimedTargets[strings.ToLower(destLocal)] = struct{}{}
						continue
					}
				}
				return qbit.RelocateTorrentOutcome{}, fmt.Errorf(
					"companion destination already exists with different content: %s",
					destLocal,
				)
			}

			resolvedLocal[oldRel] = destLocal
			claimedTargets[strings.ToLower(destLocal)] = struct{}{}
			pendingMoves = append(pendingMoves, pendingCompanionMove{
				oldLocal:  oldLocal,
				destLocal: destLocal,
				rawKind:   rawKind,
				oldFolder: *oldFolder,
			})
		} else {
			if oldFolder == nil || movedFolder == nil || oldFolder.ID != movedFolder.ID {
				return qbit.RelocateTorrentOutcome{}, fmt.Errorf(
					"leftover %s file %s was not moved and sits in a different folder from moved videos; enable moving companion/sample files or use Copy/Hardlink instead",
					rawKind, oldRel,
				)
			}
			resolvedLocal[oldRel] = oldLocal
			warnings = append(warnings, fmt.Sprintf("Left %s in original path: %s", rawKind, oldRel))
		}
	}

	remoteTargets := make(map[string]string, len(details.Files))
	for _, tf := range details.Files {
		oldRel := strings.TrimLeft(qbit.NormalizeSlashPath(tf.Name), "/")
		localP := resolvedLocal[oldRel]
		remoteTargets[oldRel] = qbit.ToRemotePath(localP, inst.PathMappings)
	}

	// Always dry-run validate the torrent relocation layout BEFORE touching companion files on disk.
	outcome, relErr := qbitClient.RelocateTorrent(ctx, inst, tHash, remoteTargets, true)
	if relErr != nil {
		return qbit.RelocateTorrentOutcome{}, relErr
	}

	plannedCompanions := make([]qbit.MovedCompanionRecord, 0, len(pendingMoves))
	for _, mv := range pendingMoves {
		plannedCompanions = append(plannedCompanions, qbit.MovedCompanionRecord{
			OldLocalPath: mv.oldLocal,
			NewLocalPath: mv.destLocal,
			Kind:         mv.rawKind,
		})
	}

	if dryRun {
		outcome.MovedCompanions = plannedCompanions
		outcome.Warnings = warnings
		return outcome, nil
	}

	cleanupDirs := true
	if it.CleanupEmptyDirs != nil {
		cleanupDirs = *it.CleanupEmptyDirs
	}

	for _, mv := range pendingMoves {
		if err := moveFileSafe(mv.oldLocal, mv.destLocal); err != nil {
			return qbit.RelocateTorrentOutcome{}, fmt.Errorf("failed moving companion file %s: %w", mv.oldLocal, err)
		}
		if cleanupDirs {
			removeEmptyParentDirs(mv.oldFolder.Path, mv.oldLocal)
		}
	}

	outcome, relErr = qbitClient.RelocateTorrent(ctx, inst, tHash, remoteTargets, false)
	if relErr != nil {
		return qbit.RelocateTorrentOutcome{}, relErr
	}
	outcome.MovedCompanions = plannedCompanions
	outcome.Warnings = warnings
	return outcome, nil
}

func stripSlashSubRel(baseDir, fullPath string, caseInsensitive bool) (string, bool) {
	b := qbit.NormalizeSlashPath(baseDir)
	f := qbit.NormalizeSlashPath(fullPath)
	if b == "" || f == "" || len(f) <= len(b)+1 {
		return "", false
	}
	prefix := b + "/"
	if strings.HasPrefix(f, prefix) || (caseInsensitive && strings.EqualFold(f[:len(prefix)], prefix)) {
		return strings.TrimLeft(f[len(prefix):], "/"), true
	}
	return "", false
}

// RelocateQbitTorrentsAction updates qBittorrent save paths and filenames for single-file torrents
// and multi-file packs after Radarr/Sonarr has moved and renamed the underlying files on disk.
func RelocateQbitTorrentsAction(
	ctx context.Context,
	settings config.AppSettings,
	qbitClient *qbit.Client,
	items []QbitRelocateItem,
	dryRun bool,
) (QbitRelocateResult, error) {
	if err := ensureWritable(settings, dryRun); err != nil {
		return QbitRelocateResult{}, err
	}

	instancesByID := make(map[string]config.QbitInstanceConfig, len(settings.QbitInstances))
	for _, inst := range settings.QbitInstances {
		instancesByID[inst.ID] = inst
	}

	res := QbitRelocateResult{
		DryRun:  dryRun,
		Results: make([]qbit.RelocateTorrentOutcome, 0, len(items)),
		Errors:  make([]QbitRelocateError, 0),
	}

	seen := make(map[string]struct{}, len(items))
	for _, it := range items {
		instID := strings.TrimSpace(it.InstanceID)
		tHash := strings.TrimSpace(it.TorrentHash)

		inst, ok := instancesByID[instID]
		if !ok {
			res.Errors = append(res.Errors, QbitRelocateError{
				InstanceID: instID, TorrentHash: tHash, Error: "Unknown qBittorrent instance ID.",
			})
			continue
		}
		if tHash == "" {
			res.Errors = append(res.Errors, QbitRelocateError{
				InstanceID: instID, TorrentHash: tHash, Error: "Missing torrent hash.",
			})
			continue
		}
		dedupKey := instID + ":" + strings.ToLower(tHash)
		if _, dup := seen[dedupKey]; dup {
			continue
		}
		seen[dedupKey] = struct{}{}

		// Multi-file (or explicit mapping) relocation with companion .nfo/.srt inspection & handling.
		if len(it.FileMappings) > 0 {
			outcome, relErr := relocatePackWithMappings(ctx, settings, qbitClient, inst, it, dryRun)
			if relErr != nil {
				res.Errors = append(res.Errors, QbitRelocateError{
					InstanceID: instID, TorrentHash: tHash, Error: relErr.Error(),
				})
				continue
			}
			res.Results = append(res.Results, outcome)
			continue
		}

		// Legacy single-file relocation path (preserves existing single-file behavior).
		localPath := strings.TrimSpace(it.NewLocalPath)
		if localPath == "" || isWithinConfiguredFolders(localPath, settings.Folders) == nil {
			res.Errors = append(res.Errors, QbitRelocateError{
				InstanceID: instID, TorrentHash: tHash, Error: "Target path is empty or outside configured folders.",
			})
			continue
		}
		nativeTarget := filepath.FromSlash(localPath)
		info, statErr := os.Lstat(nativeTarget)
		if statErr != nil || !info.Mode().IsRegular() {
			res.Errors = append(res.Errors, QbitRelocateError{
				InstanceID: instID, TorrentHash: tHash, Error: "Moved target file does not exist on disk.",
			})
			continue
		}

		newRemotePath := qbit.ToRemotePath(localPath, inst.PathMappings)
		outcome, relErr := qbitClient.RelocateSingleFileTorrent(ctx, inst, tHash, newRemotePath, dryRun)
		if relErr != nil {
			res.Errors = append(res.Errors, QbitRelocateError{
				InstanceID: instID, TorrentHash: tHash, Error: relErr.Error(),
			})
			continue
		}
		res.Results = append(res.Results, outcome)
	}

	res.RelocatedCount = len(res.Results)
	return res, nil
}
