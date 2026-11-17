package actions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if realP, err := filepath.EvalSymlinks(abs); err == nil {
		return realP, nil
	}
	// If target doesn't exist yet or is broken, resolve parent
	parent := filepath.Dir(abs)
	if realParent, err := filepath.EvalSymlinks(parent); err == nil {
		return filepath.Join(realParent, filepath.Base(abs)), nil
	}
	return abs, nil
}

func isWithinConfiguredFolders(targetPath string, folders []config.FolderConfig) *config.FolderConfig {
	resolvedTarget, err := resolveRealPath(targetPath)
	if err != nil {
		return nil
	}

	for i := range folders {
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
				rootResolved, rErr := resolveRealPath(vt.folder.Path)
				fileResolved, fErr := resolveRealPath(vt.path)
				if rErr == nil && fErr == nil {
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
				}
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
