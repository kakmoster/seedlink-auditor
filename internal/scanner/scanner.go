package scanner

import (
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"seedlink-auditor/internal/arr"
	"seedlink-auditor/internal/config"
	"seedlink-auditor/internal/qbit"
)

type ScannedFile struct {
	FolderID    string  `json:"folder_id"`
	FolderLabel string  `json:"folder_label"`
	FolderRole  string  `json:"folder_role"`
	AbsPath     string  `json:"abs_path"`
	RelPath     string  `json:"rel_path"`
	Filename    string  `json:"filename"`
	Ext         string  `json:"ext"`
	Size        int64   `json:"size"`
	Mtime       float64 `json:"mtime"`
	Inode       uint64  `json:"inode"`
	Device      uint64  `json:"device"`
	Nlink       int     `json:"nlink"`
}

type FolderStat struct {
	ID         string  `json:"id"`
	Label      string  `json:"label"`
	Path       string  `json:"path"`
	Role       string  `json:"role"`
	FileCount  int     `json:"file_count"`
	TotalBytes int64   `json:"total_bytes"`
	Device     *uint64 `json:"device"`
	Accessible bool    `json:"accessible"`
	Error      *string `json:"error"`
}

type HardlinkPair struct {
	SourcePath string `json:"source_path"`
	TargetPath string `json:"target_path"`
	Size       int64  `json:"size"`
}

type TorrentMatch struct {
	InstanceID    string   `json:"instance_id"`
	InstanceName  string   `json:"instance_name"`
	TorrentHash   string   `json:"torrent_hash"`
	TorrentName   string   `json:"torrent_name"`
	State         string   `json:"state"`
	IsSeeding     bool     `json:"is_seeding"`
	SeedingStatus string   `json:"seeding_status"`
	Ratio         float64  `json:"ratio"`
	Tracker       string   `json:"tracker"`
	TrackerHost   string   `json:"tracker_host"`
	TrackerHealth string   `json:"tracker_health"`
	TrackerMsg    string   `json:"tracker_msg"`
	SavePath      string   `json:"save_path"`
	RelativeName  string   `json:"relative_name"`
	MappedPath       string   `json:"mapped_path"`
	Size             int64    `json:"size"`
	TorrentFileCount int      `json:"torrent_file_count"`
	MatchType        string   `json:"match_type"`
	MatchedPaths     []string `json:"matched_paths"`
	MatchedRoles     []string `json:"matched_roles"`
}

type ArrReference struct {
	InstanceID       string `json:"instance_id"`
	InstanceName     string `json:"instance_name"`
	AppType          string `json:"app_type"`
	ItemID           int    `json:"item_id"`
	EpisodeIDs       []int  `json:"episode_ids,omitempty"`
	Title            string `json:"title"`
	Year             int    `json:"year,omitempty"`
	SeasonNumber     int    `json:"season_number,omitempty"`
	EpisodeNumber    int    `json:"episode_number,omitempty"`
	Monitored        bool   `json:"monitored"`
	HasFile          bool   `json:"has_file"`
	MatchKind        string `json:"match_kind"`
	StatusMessage    string `json:"status_message,omitempty"`
	FilePath         string `json:"file_path,omitempty"`
	RootPath         string `json:"root_path,omitempty"`
	ExistingFileName string `json:"existing_file_name,omitempty"`
	ExistingFilePath string `json:"existing_file_path,omitempty"`
	CanImport        bool   `json:"can_import"`
	CanRescan        bool   `json:"can_rescan"`
}

type ComparisonGroup struct {
	ID                    string         `json:"id"`
	Filename              string         `json:"filename"`
	RelPath               string         `json:"rel_path"`
	Ext                   string         `json:"ext"`
	Size                  int64          `json:"size"`
	Status                string         `json:"status"`
	SeedingSummary        string         `json:"seeding_summary"`
	HasTrackerIssue       bool           `json:"has_tracker_issue"`
	HasUnregistered       bool           `json:"has_unregistered"`
	ArrSummary            string         `json:"arr_summary"`
	HasArrIssue           bool           `json:"has_arr_issue"`
	SoleCopyOnDisk        bool           `json:"sole_copy_on_disk"`
	ExternalHardlinkCount int            `json:"external_hardlink_count"`
	CanHardlink           bool           `json:"can_hardlink"`
	HardlinkPairs         []HardlinkPair `json:"hardlink_pairs"`
	WastedBytes           int64          `json:"wasted_bytes"`
	ReclaimableBytes      int64          `json:"reclaimable_bytes"`
	FolderAFiles          []ScannedFile  `json:"folder_a_files"`
	FolderBFiles          []ScannedFile  `json:"folder_b_files"`
	AllPaths              []string       `json:"all_paths"`
	Torrents              []TorrentMatch `json:"torrents"`
	ArrMatches            []ArrReference `json:"arr_matches"`
}

type Summary struct {
	TotalGroups                   int   `json:"total_groups"`
	TotalFilesScanned             int   `json:"total_files_scanned"`
	LogicalBytes                  int64 `json:"logical_bytes"`
	PhysicalBytes                 int64 `json:"physical_bytes"`
	SpaceSavedByHardlinksBytes    int64 `json:"space_saved_by_hardlinks_bytes"`
	HardlinkedCount               int   `json:"hardlinked_count"`
	HardlinkedSavedBytes          int64 `json:"hardlinked_saved_bytes"`
	DuplicateCopyCount            int   `json:"duplicate_copy_count"`
	DuplicateCopyReclaimableBytes int64 `json:"duplicate_copy_reclaimable_bytes"`
	OrphanedInBCount              int   `json:"orphaned_in_b_count"`
	OrphanedInBSoleCount          int   `json:"orphaned_in_b_sole_count"`
	OrphanedInBReclaimableBytes   int64 `json:"orphaned_in_b_reclaimable_bytes"`
	OnlyInACount                  int   `json:"only_in_a_count"`
	SameNameDiffSizeCount         int   `json:"same_name_diff_size_count"`
	UntrackedCount                int   `json:"untracked_count"`
	UntrackedBytes                int64 `json:"untracked_bytes"`
	SeedingCount                  int   `json:"seeding_count"`
	UnregisteredCount             int   `json:"unregistered_count"`
	TrackerErrorCount             int   `json:"tracker_error_count"`
	ArrImportedCount              int   `json:"arr_imported_count"`
	ArrIssueCount                 int   `json:"arr_issue_count"`
	ArrWantedNotImportedCount     int   `json:"arr_wanted_not_imported_count"`
	ArrUnlinkedLibraryCount       int   `json:"arr_unlinked_library_count"`
	TotalReclaimableBytes         int64 `json:"total_reclaimable_bytes"`
}

type ScanResult struct {
	ScannedAt          float64               `json:"scanned_at"`
	DurationMS         float64               `json:"duration_ms"`
	CrossDeviceWarning bool                  `json:"cross_device_warning"`
	Warnings           []string              `json:"warnings"`
	Folders            []FolderStat          `json:"folders"`
	QbitInstances      []qbit.InstanceReport `json:"qbit_instances"`
	ArrInstances       []arr.InstanceReport  `json:"arr_instances"`
	Summary            Summary               `json:"summary"`
	Groups             []ComparisonGroup     `json:"groups"`
	ReadOnlyMode       bool                  `json:"read_only_mode"`
}

type devInodeKey struct {
	Device uint64
	Inode  uint64
}

type fullInodeKey struct {
	Device  uint64
	Inode   uint64
	AbsPath string // populated only when Inode == 0
}

func makeFullInodeKey(f ScannedFile) fullInodeKey {
	if f.Inode != 0 {
		return fullInodeKey{Device: f.Device, Inode: f.Inode}
	}
	return fullInodeKey{Device: f.Device, Inode: 0, AbsPath: f.AbsPath}
}

func scanDirectory(folder config.FolderConfig, ignoreExts map[string]struct{}, minSizeBytes int64) ([]ScannedFile, *uint64, *string) {
	rootInfo, err := os.Stat(folder.Path)
	if err != nil || !rootInfo.IsDir() {
		msg := fmt.Sprintf("Folder '%s' path does not exist or is not a directory: %s", folder.Label, folder.Path)
		return nil, nil, &msg
	}

	rootStat, err := StatPath(folder.Path, rootInfo)
	if err != nil {
		msg := fmt.Sprintf("Cannot stat folder '%s' (%s): %v", folder.Label, folder.Path, err)
		return nil, nil, &msg
	}
	rootDev := rootStat.Device

	files := make([]ScannedFile, 0)
	_ = filepath.WalkDir(folder.Path, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			if p != folder.Path && d.Type()&os.ModeSymlink != 0 {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}

		name := d.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if len(ignoreExts) > 0 {
			if _, skip := ignoreExts[ext]; skip {
				return nil
			}
		}

		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		if minSizeBytes > 0 && info.Size() < minSizeBytes {
			return nil
		}

		fstat, statErr := StatPath(p, info)
		if statErr != nil {
			return nil
		}

		rel, relErr := filepath.Rel(folder.Path, p)
		if relErr != nil {
			rel = name
		}

		files = append(files, ScannedFile{
			FolderID:    folder.ID,
			FolderLabel: folder.Label,
			FolderRole:  folder.Role,
			AbsPath:     qbit.NormalizeSlashPath(p),
			RelPath:     qbit.NormalizeSlashPath(rel),
			Filename:    name,
			Ext:         ext,
			Size:        info.Size(),
			Mtime:       float64(info.ModTime().UnixNano()) / 1e9,
			Inode:       fstat.Inode,
			Device:      fstat.Device,
			Nlink:       fstat.Nlink,
		})
		return nil
	})

	return files, &rootDev, nil
}

type unionFind struct {
	parent []int
}

func newUnionFind(n int) *unionFind {
	p := make([]int, n)
	for i := range p {
		p[i] = i
	}
	return &unionFind{parent: p}
}

func (u *unionFind) find(x int) int {
	for u.parent[x] != x {
		u.parent[x] = u.parent[u.parent[x]]
		x = u.parent[x]
	}
	return x
}

func (u *unionFind) union(a, b int) {
	ra, rb := u.find(a), u.find(b)
	if ra != rb {
		u.parent[rb] = ra
	}
}

func clusterFiles(allFiles []ScannedFile) [][]ScannedFile {
	n := len(allFiles)
	if n == 0 {
		return nil
	}

	uf := newUnionFind(n)

	// 1. Always merge files sharing physical (Device, Inode) when Inode != 0
	inodeGroups := make(map[devInodeKey][]int)
	for idx, f := range allFiles {
		if f.Inode != 0 {
			k := devInodeKey{Device: f.Device, Inode: f.Inode}
			inodeGroups[k] = append(inodeGroups[k], idx)
		}
	}
	for _, idxs := range inodeGroups {
		first := idxs[0]
		for _, other := range idxs[1:] {
			uf.union(first, other)
		}
	}

	// 2. Group by lowercase filename, disambiguating intra-folder collisions
	byFilename := make(map[string][]int)
	for idx, f := range allFiles {
		key := strings.ToLower(f.Filename)
		byFilename[key] = append(byFilename[key], idx)
	}

	type folderInodeKey struct {
		Device uint64
		Inode  uint64
		FallbackIdx int
	}

	for _, idxs := range byFilename {
		if len(idxs) <= 1 {
			continue
		}

		distinctInFolder := make(map[string]map[folderInodeKey]struct{})
		for _, idx := range idxs {
			f := allFiles[idx]
			if _, ok := distinctInFolder[f.FolderID]; !ok {
				distinctInFolder[f.FolderID] = make(map[folderInodeKey]struct{})
			}
			fb := 0
			if f.Inode == 0 {
				fb = idx + 1
			}
			distinctInFolder[f.FolderID][folderInodeKey{Device: f.Device, Inode: f.Inode, FallbackIdx: fb}] = struct{}{}
		}

		hasCollision := false
		for _, set := range distinctInFolder {
			if len(set) > 1 {
				hasCollision = true
				break
			}
		}

		if !hasCollision {
			first := idxs[0]
			for _, other := range idxs[1:] {
				uf.union(first, other)
			}
			continue
		}

		// Disambiguate generic filenames appearing multiple times within the same folder:
		// First pair matching relative paths across folders
		byRel := make(map[string][]int)
		for _, idx := range idxs {
			rk := strings.ToLower(allFiles[idx].RelPath)
			byRel[rk] = append(byRel[rk], idx)
		}
		for _, relIdxs := range byRel {
			if len(relIdxs) > 1 {
				first := relIdxs[0]
				for _, other := range relIdxs[1:] {
					uf.union(first, other)
				}
			}
		}

		// Next pair by (filename, size) where each folder has at most one distinct inode of that size
		bySize := make(map[int64][]int)
		for _, idx := range idxs {
			bySize[allFiles[idx].Size] = append(bySize[allFiles[idx].Size], idx)
		}
		for _, sizeIdxs := range bySize {
			sizeFolderInodes := make(map[string]map[folderInodeKey]struct{})
			for _, idx := range sizeIdxs {
				f := allFiles[idx]
				if _, ok := sizeFolderInodes[f.FolderID]; !ok {
					sizeFolderInodes[f.FolderID] = make(map[folderInodeKey]struct{})
				}
				fb := 0
				if f.Inode == 0 {
					fb = idx + 1
				}
				sizeFolderInodes[f.FolderID][folderInodeKey{Device: f.Device, Inode: f.Inode, FallbackIdx: fb}] = struct{}{}
			}
			uniquePerFolder := true
			for _, set := range sizeFolderInodes {
				if len(set) > 1 {
					uniquePerFolder = false
					break
				}
			}
			if uniquePerFolder && len(sizeIdxs) > 1 {
				first := sizeIdxs[0]
				for _, other := range sizeIdxs[1:] {
					uf.union(first, other)
				}
			}
		}
	}

	clustersMap := make(map[int][]ScannedFile)
	order := make([]int, 0)
	for idx, f := range allFiles {
		root := uf.find(idx)
		if _, exists := clustersMap[root]; !exists {
			order = append(order, root)
		}
		clustersMap[root] = append(clustersMap[root], f)
	}

	out := make([][]ScannedFile, 0, len(order))
	for _, root := range order {
		out = append(out, clustersMap[root])
	}
	return out
}

// ScannedFolders holds the combined disk walk output across all configured folders.
type ScannedFolders struct {
	Files              []ScannedFile
	FolderStats        []FolderStat
	Warnings           []string
	CrossDeviceWarning bool
	PrimaryIDs         map[string]bool
}

func primaryFolderIDs(settings config.AppSettings) map[string]bool {
	primaryIDs := make(map[string]bool)
	for _, f := range settings.Folders {
		if f.Role == "primary" {
			primaryIDs[f.ID] = true
		}
	}
	if len(primaryIDs) == 0 && len(settings.Folders) > 0 {
		primaryIDs[settings.Folders[0].ID] = true
	}
	return primaryIDs
}

func buildIgnoreExtSet(settings config.AppSettings) map[string]struct{} {
	ignoreExts := make(map[string]struct{}, len(settings.IgnoreExtensions))
	for _, e := range settings.IgnoreExtensions {
		ignoreExts[strings.ToLower(e)] = struct{}{}
	}
	return ignoreExts
}

func hasEnabledArrInstance(settings config.AppSettings) bool {
	for _, a := range settings.ArrInstances {
		if a.Enabled {
			return true
		}
	}
	return false
}

// ScanFolders walks all configured folders concurrently and collects file metadata.
func ScanFolders(settings config.AppSettings) ScannedFolders {
	ignoreExts := buildIgnoreExtSet(settings)
	primaryIDs := primaryFolderIDs(settings)

	type folderWalkResult struct {
		files  []ScannedFile
		devPtr *uint64
		errPtr *string
		stat   FolderStat
	}

	results := make([]folderWalkResult, len(settings.Folders))
	var wg sync.WaitGroup

	for i, folder := range settings.Folders {
		role := "secondary"
		if primaryIDs[folder.ID] {
			role = "primary"
		}
		effFolder := folder
		effFolder.Role = role

		wg.Add(1)
		go func(pos int, f config.FolderConfig, r string) {
			defer wg.Done()
			scanned, devPtr, errPtr := scanDirectory(f, ignoreExts, settings.MinSizeBytes)
			var totalBytes int64
			for _, sf := range scanned {
				totalBytes += sf.Size
			}
			results[pos] = folderWalkResult{
				files:  scanned,
				devPtr: devPtr,
				errPtr: errPtr,
				stat: FolderStat{
					ID:         f.ID,
					Label:      f.Label,
					Path:       f.Path,
					Role:       r,
					FileCount:  len(scanned),
					TotalBytes: totalBytes,
					Device:     devPtr,
					Accessible: errPtr == nil,
					Error:      errPtr,
				},
			}
		}(i, effFolder, role)
	}
	wg.Wait()

	warnings := make([]string, 0)
	allFiles := make([]ScannedFile, 0)
	folderStats := make([]FolderStat, 0, len(settings.Folders))
	primaryDevs := make(map[uint64]struct{})
	secondaryDevs := make(map[uint64]struct{})

	for _, res := range results {
		if res.errPtr != nil {
			warnings = append(warnings, *res.errPtr)
		}
		if res.devPtr != nil {
			if res.stat.Role == "primary" {
				primaryDevs[*res.devPtr] = struct{}{}
			} else {
				secondaryDevs[*res.devPtr] = struct{}{}
			}
		}
		allFiles = append(allFiles, res.files...)
		folderStats = append(folderStats, res.stat)
	}

	for _, f := range allFiles {
		if primaryIDs[f.FolderID] {
			primaryDevs[f.Device] = struct{}{}
		} else {
			secondaryDevs[f.Device] = struct{}{}
		}
	}

	hasSharedDev := false
	for dev := range primaryDevs {
		if _, ok := secondaryDevs[dev]; ok {
			hasSharedDev = true
			break
		}
	}
	crossDeviceWarning := len(primaryDevs) > 0 && len(secondaryDevs) > 0 && !hasSharedDev
	if crossDeviceWarning {
		warnings = append(warnings,
			"Primary and secondary folders are on different filesystem devices (device ID differs). "+
				"Hardlinks cannot span separate mounts or filesystems — in Docker, mount a single shared parent folder "+
				"(e.g. /path/to/data -> /data) so hardlink detection and creation work.")
	}

	return ScannedFolders{
		Files:              allFiles,
		FolderStats:        folderStats,
		Warnings:           warnings,
		CrossDeviceWarning: crossDeviceWarning,
		PrimaryIDs:         primaryIDs,
	}
}

func evaluateCluster(groupFiles []ScannedFile, primaryIDs map[string]bool, hasEnabledArr bool, qbitIndex *qbit.Index, arrIndex *arr.Index, groupID string) ComparisonGroup {
	sorted := make([]ScannedFile, len(groupFiles))
	copy(sorted, groupFiles)
	sort.Slice(sorted, func(i, j int) bool {
		pi := 1
		if primaryIDs[sorted[i].FolderID] {
			pi = 0
		}
		pj := 1
		if primaryIDs[sorted[j].FolderID] {
			pj = 0
		}
		if pi != pj {
			return pi < pj
		}
		return sorted[i].AbsPath < sorted[j].AbsPath
	})

	inA := make([]ScannedFile, 0)
	inB := make([]ScannedFile, 0)
	aInodes := make(map[fullInodeKey]struct{})
	bInodes := make(map[fullInodeKey]struct{})
	allInodes := make(map[fullInodeKey]struct{})
	distinctSizes := make(map[int64]struct{})

	for _, f := range sorted {
		k := makeFullInodeKey(f)
		allInodes[k] = struct{}{}
		distinctSizes[f.Size] = struct{}{}
		if primaryIDs[f.FolderID] {
			inA = append(inA, f)
			aInodes[k] = struct{}{}
		} else {
			inB = append(inB, f)
			bInodes[k] = struct{}{}
		}
	}

	var status string
	canHardlink := false
	hardlinkPairs := make([]HardlinkPair, 0)
	soleCopyOnDisk := false
	externalHardlinkCount := 0
	var wastedBytes int64
	var reclaimableBytes int64

	if len(inA) > 0 && len(inB) > 0 {
		if len(allInodes) == 1 && sorted[0].Inode != 0 {
			status = "HARDLINKED"
		} else if len(distinctSizes) == 1 {
			status = "DUPLICATE_COPY"
			fileSize := sorted[0].Size
			if len(allInodes) > 1 {
				wastedBytes = fileSize * int64(len(allInodes)-1)
			}
			reclaimableBytes = wastedBytes

			src := inA[0]
			srcKey := makeFullInodeKey(src)
			candidates := make([]ScannedFile, 0, len(inB)+len(inA)-1)
			candidates = append(candidates, inB...)
			candidates = append(candidates, inA[1:]...)
			for _, cand := range candidates {
				if makeFullInodeKey(cand) != srcKey && cand.Device == src.Device && fileSize > 0 {
					canHardlink = true
					hardlinkPairs = append(hardlinkPairs, HardlinkPair{
						SourcePath: src.AbsPath,
						TargetPath: cand.AbsPath,
						Size:       fileSize,
					})
				}
			}
		} else {
			aSizes := make(map[int64]struct{})
			bSizes := make(map[int64]struct{})
			for _, f := range inA {
				aSizes[f.Size] = struct{}{}
			}
			for _, f := range inB {
				bSizes[f.Size] = struct{}{}
			}
			commonSizes := make([]int64, 0)
			for sz := range aSizes {
				if _, ok := bSizes[sz]; ok {
					commonSizes = append(commonSizes, sz)
				}
			}
			sameABInodes := len(aInodes) == len(bInodes)
			if sameABInodes {
				for k := range aInodes {
					if _, ok := bInodes[k]; !ok {
						sameABInodes = false
						break
					}
				}
			}
			hasSharedInode := false
			for k := range aInodes {
				if _, ok := bInodes[k]; ok {
					hasSharedInode = true
					break
				}
			}

			if len(commonSizes) > 0 && sameABInodes {
				status = "HARDLINKED"
			} else if len(commonSizes) > 0 && !hasSharedInode {
				status = "DUPLICATE_COPY"
				for _, sz := range commonSizes {
					inodesForSz := make(map[fullInodeKey]struct{})
					var src ScannedFile
					hasSrc := false
					for _, f := range sorted {
						if f.Size == sz {
							inodesForSz[makeFullInodeKey(f)] = struct{}{}
						}
					}
					for _, f := range inA {
						if f.Size == sz {
							src = f
							hasSrc = true
							break
						}
					}
					if len(inodesForSz) > 1 {
						wasted := sz * int64(len(inodesForSz)-1)
						wastedBytes += wasted
						reclaimableBytes += wasted
					}
					if hasSrc && sz > 0 {
						for _, cand := range inB {
							if cand.Size == sz && cand.Device == src.Device {
								canHardlink = true
								hardlinkPairs = append(hardlinkPairs, HardlinkPair{
									SourcePath: src.AbsPath,
									TargetPath: cand.AbsPath,
									Size:       sz,
								})
							}
						}
					}
				}
			} else {
				status = "SAME_NAME_DIFF_SIZE"
			}
		}
	} else if len(inB) > 0 && len(inA) == 0 {
		status = "ORPHANED_IN_B"

		bByInode := make(map[fullInodeKey][]ScannedFile)
		bInodeOrder := make([]fullInodeKey, 0)
		for _, f := range inB {
			k := makeFullInodeKey(f)
			if _, exists := bByInode[k]; !exists {
				bInodeOrder = append(bInodeOrder, k)
			}
			bByInode[k] = append(bByInode[k], f)
		}

		allSole := true
		for _, k := range bInodeOrder {
			inodeFiles := bByInode[k]
			rep := inodeFiles[0]
			extLinks := rep.Nlink - len(inodeFiles)
			if extLinks < 0 {
				extLinks = 0
			}
			externalHardlinkCount += extLinks
			if extLinks == 0 {
				reclaimableBytes += rep.Size
				wastedBytes += rep.Size
			} else {
				allSole = false
			}
		}
		soleCopyOnDisk = allSole && len(bByInode) > 0
	} else {
		status = "ONLY_IN_A"
	}

	// Correlate against qBittorrent index
	type torrentKey struct {
		InstanceID  string
		TorrentHash string
	}
	matchedMap := make(map[torrentKey]*TorrentMatch)
	matchedOrder := make([]torrentKey, 0)

	for _, f := range sorted {
		matches := qbitIndex.FindMatches(f.AbsPath, f.Filename, f.Size)
		for _, m := range matches {
			tk := torrentKey{InstanceID: m.Entry.InstanceID, TorrentHash: m.Entry.TorrentHash}
			existing, ok := matchedMap[tk]
			if !ok {
				tm := TorrentMatch{
					InstanceID:    m.Entry.InstanceID,
					InstanceName:  m.Entry.InstanceName,
					TorrentHash:   m.Entry.TorrentHash,
					TorrentName:   m.Entry.TorrentName,
					State:         m.Entry.State,
					IsSeeding:     m.Entry.IsSeeding,
					SeedingStatus: m.Entry.SeedingStatus,
					Ratio:         m.Entry.Ratio,
					Tracker:       m.Entry.Tracker,
					TrackerHost:   m.Entry.TrackerHost,
					TrackerHealth: m.Entry.TrackerHealth,
					TrackerMsg:    m.Entry.TrackerMsg,
					SavePath:      m.Entry.SavePath,
					RelativeName:  m.Entry.RelativeName,
					MappedPath:       m.Entry.MappedPath,
					Size:             m.Entry.Size,
					TorrentFileCount: m.Entry.TorrentFileCount,
					MatchType:        m.MatchType,
					MatchedPaths:     []string{f.AbsPath},
					MatchedRoles:     []string{f.FolderRole},
				}
				matchedMap[tk] = &tm
				matchedOrder = append(matchedOrder, tk)
			} else {
				if !containsStr(existing.MatchedPaths, f.AbsPath) {
					existing.MatchedPaths = append(existing.MatchedPaths, f.AbsPath)
				}
				if !containsStr(existing.MatchedRoles, f.FolderRole) {
					existing.MatchedRoles = append(existing.MatchedRoles, f.FolderRole)
				}
				if m.MatchType == "path" {
					existing.MatchType = "path"
				}
			}
		}
	}

	torrents := make([]TorrentMatch, 0, len(matchedOrder))
	anySeeding := false
	anyUnregistered := false
	anyError := false
	anyDownloading := false
	for _, tk := range matchedOrder {
		tm := *matchedMap[tk]
		torrents = append(torrents, tm)
		if tm.TrackerHealth == "UNREGISTERED" {
			anyUnregistered = true
		} else if tm.TrackerHealth == "TRACKER_ERROR" || tm.TrackerHealth == "LOCAL_ERROR" {
			anyError = true
		}
		if tm.IsSeeding {
			anySeeding = true
		} else if tm.SeedingStatus == "downloading" {
			anyDownloading = true
		}
	}

	hasTrackerIssue := anyUnregistered || anyError

	var seedingSummary string
	switch {
	case anyUnregistered && !anySeeding:
		seedingSummary = "UNREGISTERED"
	case anyError && !anySeeding:
		seedingSummary = "ERROR"
	case anySeeding:
		seedingSummary = "SEEDING"
	case anyDownloading:
		seedingSummary = "DOWNLOADING"
	case len(torrents) > 0:
		seedingSummary = "PAUSED"
	default:
		seedingSummary = "UNTRACKED"
	}

	repFile := sorted[0]
	if len(inA) > 0 {
		repFile = inA[0]
	} else if len(inB) > 0 {
		repFile = inB[0]
	}

	var displaySize int64
	allPaths := make([]string, 0, len(sorted))
	for _, f := range sorted {
		if f.Size > displaySize {
			displaySize = f.Size
		}
		allPaths = append(allPaths, f.AbsPath)
	}

	// Correlate against Radarr / Sonarr index
	arrMatches := make([]ArrReference, 0)
	arrSummary := "UNCONFIGURED"
	hasArrIssue := false

	if hasEnabledArr {
		type arrItemKey struct {
			InstanceID string
			ItemID     int
			Kind       string
		}
		seenArr := make(map[arrItemKey]struct{})
		importedItems := make(map[string]struct{})
		unlinkedItems := make(map[string]struct{})
		anyImported := false
		anyQueueBlocked := false
		anyWanted := false
		anyLibraryUnlinked := false
		anyReplaced := false

		importCandidatePath := repFile.AbsPath
		if len(inB) > 0 {
			importCandidatePath = inB[0].AbsPath
		}

		// 1. Imported file matches
		for _, f := range sorted {
			for _, fm := range arrIndex.FindFileMatches(f.AbsPath, f.Filename, f.Size) {
				k := arrItemKey{InstanceID: fm.Entry.InstanceID, ItemID: fm.Entry.ItemID, Kind: "imported"}
				if _, dup := seenArr[k]; dup {
					continue
				}
				seenArr[k] = struct{}{}
				importedItems[fmt.Sprintf("%s:%d", fm.Entry.InstanceID, fm.Entry.ItemID)] = struct{}{}
				anyImported = true
				arrMatches = append(arrMatches, ArrReference{
					InstanceID:    fm.Entry.InstanceID,
					InstanceName:  fm.Entry.InstanceName,
					AppType:       fm.Entry.AppType,
					ItemID:        fm.Entry.ItemID,
					Title:         fm.Entry.Title,
					Year:          fm.Entry.Year,
					SeasonNumber:  fm.Entry.SeasonNumber,
					Monitored:     fm.Entry.Monitored,
					HasFile:       true,
					MatchKind:     "imported",
					StatusMessage: "Imported and referenced",
					FilePath:      fm.Entry.MappedPath,
				})
			}
		}

		// 2. Queue matches (by torrent hash)
		hashes := make([]string, 0, len(torrents))
		for _, t := range torrents {
			hashes = append(hashes, t.TorrentHash)
		}
		for _, qm := range arrIndex.FindQueueMatches(hashes) {
			if qm.ItemID > 0 {
				if _, alreadyImported := importedItems[fmt.Sprintf("%s:%d", qm.InstanceID, qm.ItemID)]; alreadyImported {
					continue
				}
			}
			k := arrItemKey{InstanceID: qm.InstanceID, ItemID: qm.ItemID, Kind: "queue"}
			if _, dup := seenArr[k]; dup {
				continue
			}
			seenArr[k] = struct{}{}
			msg := qm.StatusMessage
			if msg == "" {
				if qm.TrackedDownloadState != "" {
					msg = qm.TrackedDownloadState
				} else {
					msg = qm.Status
				}
			}
			if qm.IsBlocked {
				anyQueueBlocked = true
			}
			var queueRootPath string
			if qm.ItemID > 0 {
				for _, lib := range arrIndex.LibraryRoots {
					if lib.InstanceID == qm.InstanceID && lib.ItemID == qm.ItemID && lib.MappedPath != "" {
						queueRootPath = lib.MappedPath
						break
					}
				}
			}
			arrMatches = append(arrMatches, ArrReference{
				InstanceID:    qm.InstanceID,
				InstanceName:  qm.InstanceName,
				AppType:       qm.AppType,
				ItemID:        qm.ItemID,
				Title:         qm.Title,
				Monitored:     true,
				HasFile:       false,
				MatchKind:     "queue",
				StatusMessage: msg,
				FilePath:      importCandidatePath,
				RootPath:      queueRootPath,
				CanImport:     len(inA) == 0 || qm.IsBlocked,
			})
		}

		// 3. Unlinked file inside a Radarr/Sonarr library directory (in Folder A, not in Arr file table)
		for _, f := range inA {
			for _, lib := range arrIndex.FindLibraryPathOwner(f.AbsPath) {
				if _, alreadyImported := importedItems[fmt.Sprintf("%s:%d", lib.InstanceID, lib.ItemID)]; alreadyImported {
					continue
				}
				k := arrItemKey{InstanceID: lib.InstanceID, ItemID: lib.ItemID, Kind: "library_unlinked"}
				if _, dup := seenArr[k]; dup {
					continue
				}
				seenArr[k] = struct{}{}
				unlinkedItems[fmt.Sprintf("%s:%d", lib.InstanceID, lib.ItemID)] = struct{}{}
				anyLibraryUnlinked = true
				arrMatches = append(arrMatches, ArrReference{
					InstanceID:    lib.InstanceID,
					InstanceName:  lib.InstanceName,
					AppType:       lib.AppType,
					ItemID:        lib.ItemID,
					Title:         lib.Title,
					Year:          lib.Year,
					Monitored:     lib.Monitored,
					HasFile:       lib.HasFile,
					MatchKind:     "library_unlinked",
					StatusMessage: "File sits in library folder but is unlinked in Arr",
					FilePath:      f.AbsPath,
					RootPath:      lib.MappedPath,
					CanRescan:     true,
				})
			}
		}

		// 4. Title / wanted / replaced matches when not yet matched on disk
		if !anyImported {
			nameCandidates := make([]string, 0, len(sorted))
			for _, f := range sorted {
				nameCandidates = append(nameCandidates, f.Filename)
			}
			torrentNames := make([]string, 0, len(torrents))
			for _, t := range torrents {
				torrentNames = append(torrentNames, t.TorrentName)
			}
			if len(torrentNames) == 0 {
				torrentNames = []string{""}
			}
			for _, fn := range nameCandidates {
				for _, tn := range torrentNames {
					for _, tm := range arrIndex.FindTitleMatches(fn, tn) {
						itemKey := fmt.Sprintf("%s:%d", tm.InstanceID, tm.ItemID)
						if _, alreadyImported := importedItems[itemKey]; alreadyImported {
							continue
						}
						if _, alreadyUnlinked := unlinkedItems[itemKey]; alreadyUnlinked {
							continue
						}
						kind := "wanted_title"
						canImport := true
						msg := "Wanted/missing in Arr; file not imported to library"
						if tm.HasFile {
							kind = "replaced"
							canImport = false
							if tm.ExistingFileName != "" {
								msg = "Arr already has: " + tm.ExistingFileName
							} else {
								msg = "Arr already has another file for this title"
							}
						}
						k := arrItemKey{InstanceID: tm.InstanceID, ItemID: tm.ItemID, Kind: kind}
						if _, dup := seenArr[k]; dup {
							continue
						}
						seenArr[k] = struct{}{}
						if tm.HasFile {
							anyReplaced = true
						} else {
							anyWanted = true
						}
						arrMatches = append(arrMatches, ArrReference{
							InstanceID:       tm.InstanceID,
							InstanceName:     tm.InstanceName,
							AppType:          tm.AppType,
							ItemID:           tm.ItemID,
							EpisodeIDs:       tm.EpisodeIDs,
							Title:            tm.Title,
							Year:             tm.Year,
							SeasonNumber:     tm.SeasonNumber,
							EpisodeNumber:    tm.EpisodeNumber,
							Monitored:        tm.Monitored,
							HasFile:          tm.HasFile,
							MatchKind:        kind,
							StatusMessage:    msg,
							FilePath:         importCandidatePath,
							RootPath:         tm.RootPath,
							ExistingFileName: tm.ExistingFileName,
							ExistingFilePath: tm.ExistingFilePath,
							CanImport:        canImport,
						})
					}
				}
			}
		}

		switch {
		case anyImported:
			arrSummary = "IMPORTED"
		case anyQueueBlocked:
			arrSummary = "IN_QUEUE_BLOCKED"
			hasArrIssue = true
		case anyWanted:
			arrSummary = "WANTED_NOT_IMPORTED"
			hasArrIssue = true
		case anyLibraryUnlinked:
			arrSummary = "UNLINKED_IN_LIBRARY"
			hasArrIssue = true
		case anyReplaced:
			arrSummary = "REPLACED_IN_ARR"
		default:
			arrSummary = "NOT_IN_ARR"
		}
	}

	return ComparisonGroup{
		ID:                    groupID,
		Filename:              repFile.Filename,
		RelPath:               repFile.RelPath,
		Ext:                   repFile.Ext,
		Size:                  displaySize,
		Status:                status,
		SeedingSummary:        seedingSummary,
		HasTrackerIssue:       hasTrackerIssue,
		HasUnregistered:       anyUnregistered,
		ArrSummary:            arrSummary,
		HasArrIssue:           hasArrIssue,
		SoleCopyOnDisk:        soleCopyOnDisk,
		ExternalHardlinkCount: externalHardlinkCount,
		CanHardlink:           canHardlink,
		HardlinkPairs:         hardlinkPairs,
		WastedBytes:           wastedBytes,
		ReclaimableBytes:      reclaimableBytes,
		FolderAFiles:          inA,
		FolderBFiles:          inB,
		AllPaths:              allPaths,
		Torrents:              torrents,
		ArrMatches:            arrMatches,
	}
}

// RecalculateSummary computes global scan statistics purely from the active comparison groups.
func RecalculateSummary(groups []ComparisonGroup) Summary {
	seenPhysical := make(map[devInodeKey]int64)
	var zeroInodeBytes int64
	var logicalBytes int64
	totalFiles := 0

	var summary Summary
	for _, g := range groups {
		groupFiles := make([]ScannedFile, 0, len(g.FolderAFiles)+len(g.FolderBFiles))
		groupFiles = append(groupFiles, g.FolderAFiles...)
		groupFiles = append(groupFiles, g.FolderBFiles...)
		totalFiles += len(groupFiles)

		groupAllInodes := make(map[fullInodeKey]struct{}, len(groupFiles))
		for _, f := range groupFiles {
			groupAllInodes[makeFullInodeKey(f)] = struct{}{}
			logicalBytes += f.Size
			if f.Inode != 0 {
				k := devInodeKey{Device: f.Device, Inode: f.Inode}
				if _, exists := seenPhysical[k]; !exists {
					seenPhysical[k] = f.Size
				}
			} else {
				zeroInodeBytes += f.Size
			}
		}

		switch g.Status {
		case "HARDLINKED":
			summary.HardlinkedCount++
			if len(groupFiles) > 1 && len(groupAllInodes) == 1 && groupFiles[0].Inode != 0 {
				summary.HardlinkedSavedBytes += groupFiles[0].Size * int64(len(groupFiles)-1)
			}
		case "DUPLICATE_COPY":
			summary.DuplicateCopyCount++
			summary.DuplicateCopyReclaimableBytes += g.ReclaimableBytes
		case "ORPHANED_IN_B":
			summary.OrphanedInBCount++
			if g.SoleCopyOnDisk {
				summary.OrphanedInBSoleCount++
			}
			summary.OrphanedInBReclaimableBytes += g.ReclaimableBytes
		case "ONLY_IN_A":
			summary.OnlyInACount++
		case "SAME_NAME_DIFF_SIZE":
			summary.SameNameDiffSizeCount++
		}

		if g.HasUnregistered {
			summary.UnregisteredCount++
		}
		if g.HasTrackerIssue {
			summary.TrackerErrorCount++
		}

		switch g.SeedingSummary {
		case "SEEDING":
			summary.SeedingCount++
		case "UNTRACKED":
			summary.UntrackedCount++
			groupPhys := make(map[fullInodeKey]int64, len(groupFiles))
			for _, f := range groupFiles {
				groupPhys[makeFullInodeKey(f)] = f.Size
			}
			for _, sz := range groupPhys {
				summary.UntrackedBytes += sz
			}
		}

		switch g.ArrSummary {
		case "IMPORTED":
			summary.ArrImportedCount++
		case "IN_QUEUE_BLOCKED":
			summary.ArrIssueCount++
		case "WANTED_NOT_IMPORTED":
			summary.ArrIssueCount++
			summary.ArrWantedNotImportedCount++
		case "UNLINKED_IN_LIBRARY":
			summary.ArrIssueCount++
			summary.ArrUnlinkedLibraryCount++
		}
	}

	physicalBytes := zeroInodeBytes
	for _, sz := range seenPhysical {
		physicalBytes += sz
	}
	spaceSaved := logicalBytes - physicalBytes
	if spaceSaved < 0 {
		spaceSaved = 0
	}

	summary.TotalGroups = len(groups)
	summary.TotalFilesScanned = totalFiles
	summary.LogicalBytes = logicalBytes
	summary.PhysicalBytes = physicalBytes
	summary.SpaceSavedByHardlinksBytes = spaceSaved
	summary.TotalReclaimableBytes = summary.OrphanedInBReclaimableBytes + summary.DuplicateCopyReclaimableBytes
	return summary
}

// CompareScanned correlates pre-walked folder files against qBittorrent and Radarr/Sonarr indices.
func CompareScanned(settings config.AppSettings, scanned ScannedFolders, qbitIndex *qbit.Index, arrIndex *arr.Index, startedAt time.Time) ScanResult {
	if qbitIndex == nil {
		qbitIndex = qbit.NewIndex()
	}
	if arrIndex == nil {
		arrIndex = arr.NewIndex()
	}

	warnings := make([]string, len(scanned.Warnings))
	copy(warnings, scanned.Warnings)

	for _, rep := range qbitIndex.Instances {
		if !rep.OK && rep.Error != nil && *rep.Error != "Disabled" {
			warnings = append(warnings, fmt.Sprintf("qBittorrent '%s' error: %s", rep.InstanceName, *rep.Error))
		}
	}
	for _, rep := range arrIndex.Instances {
		if !rep.OK && rep.Error != nil && *rep.Error != "Disabled" {
			warnings = append(warnings, fmt.Sprintf("%s '%s' error: %s", strings.ToUpper(rep.AppType[:1])+rep.AppType[1:], rep.InstanceName, *rep.Error))
		}
	}

	hasEnabledArr := hasEnabledArrInstance(settings)
	primaryIDs := scanned.PrimaryIDs
	if len(primaryIDs) == 0 {
		primaryIDs = primaryFolderIDs(settings)
	}

	clusters := clusterFiles(scanned.Files)
	groups := make([]ComparisonGroup, 0, len(clusters))
	for idx, groupFiles := range clusters {
		groups = append(groups, evaluateCluster(groupFiles, primaryIDs, hasEnabledArr, qbitIndex, arrIndex, fmt.Sprintf("grp-%d", idx+1)))
	}

	priority := map[string]int{
		"ORPHANED_IN_B":       0,
		"DUPLICATE_COPY":      1,
		"SAME_NAME_DIFF_SIZE": 2,
		"HARDLINKED":          3,
		"ONLY_IN_A":           4,
	}
	sort.Slice(groups, func(i, j int) bool {
		pi, ok := priority[groups[i].Status]
		if !ok {
			pi = 9
		}
		pj, ok := priority[groups[j].Status]
		if !ok {
			pj = 9
		}
		if pi != pj {
			return pi < pj
		}
		if groups[i].ReclaimableBytes != groups[j].ReclaimableBytes {
			return groups[i].ReclaimableBytes > groups[j].ReclaimableBytes
		}
		return strings.ToLower(groups[i].Filename) < strings.ToLower(groups[j].Filename)
	})

	summary := RecalculateSummary(groups)
	durationMS := math.Round(float64(time.Since(startedAt).Microseconds())/100.0) / 10.0

	instancesOut := make([]qbit.InstanceReport, len(qbitIndex.Instances))
	copy(instancesOut, qbitIndex.Instances)

	arrInstancesOut := make([]arr.InstanceReport, len(arrIndex.Instances))
	copy(arrInstancesOut, arrIndex.Instances)

	return ScanResult{
		ScannedAt:          float64(time.Now().UnixNano()) / 1e9,
		DurationMS:         durationMS,
		CrossDeviceWarning: scanned.CrossDeviceWarning,
		Warnings:           warnings,
		Folders:            scanned.FolderStats,
		QbitInstances:      instancesOut,
		ArrInstances:       arrInstancesOut,
		Summary:            summary,
		Groups:             groups,
		ReadOnlyMode:       settings.ReadOnlyMode,
	}
}

func ScanAndCompare(settings config.AppSettings, qbitIndex *qbit.Index, arrIndex *arr.Index) ScanResult {
	start := time.Now()
	scanned := ScanFolders(settings)
	return CompareScanned(settings, scanned, qbitIndex, arrIndex, start)
}

func pathCompareKey(p string) string {
	norm := qbit.NormalizeSlashPath(p)
	if runtime.GOOS == "windows" {
		return strings.ToLower(norm)
	}
	return norm
}

func findOwningFolder(folders []config.FolderConfig, primaryIDs map[string]bool, absPath string) (config.FolderConfig, bool) {
	normAbs := qbit.NormalizeSlashPath(absPath)
	normLower := strings.ToLower(normAbs)
	bestLen := -1
	var best config.FolderConfig
	found := false

	for _, f := range folders {
		root := qbit.NormalizeSlashPath(f.Path)
		if root == "" {
			continue
		}
		matched := normAbs == root || strings.HasPrefix(normAbs, root+"/")
		if !matched && runtime.GOOS == "windows" {
			rootLower := strings.ToLower(root)
			matched = normLower == rootLower || strings.HasPrefix(normLower, rootLower+"/")
		}
		if matched && len(root) > bestLen {
			bestLen = len(root)
			role := "secondary"
			if primaryIDs[f.ID] {
				role = "primary"
			}
			best = f
			best.Role = role
			found = true
		}
	}
	return best, found
}

func statScannedFileFromDisk(folder config.FolderConfig, absPath string, ignoreExts map[string]struct{}, minSizeBytes int64, applyFilters bool) (ScannedFile, bool) {
	nativePath := filepath.FromSlash(absPath)
	info, err := os.Lstat(nativePath)
	if err != nil {
		return ScannedFile{}, false
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return ScannedFile{}, false
	}

	name := filepath.Base(nativePath)
	ext := strings.ToLower(filepath.Ext(name))
	if applyFilters {
		if len(ignoreExts) > 0 {
			if _, skip := ignoreExts[ext]; skip {
				return ScannedFile{}, false
			}
		}
		if minSizeBytes > 0 && info.Size() < minSizeBytes {
			return ScannedFile{}, false
		}
	}

	fstat, err := StatPath(nativePath, info)
	if err != nil {
		return ScannedFile{}, false
	}

	rel := name
	if folder.Path != "" {
		if r, relErr := filepath.Rel(folder.Path, nativePath); relErr == nil {
			rel = r
		}
	}

	return ScannedFile{
		FolderID:    folder.ID,
		FolderLabel: folder.Label,
		FolderRole:  folder.Role,
		AbsPath:     qbit.NormalizeSlashPath(nativePath),
		RelPath:     qbit.NormalizeSlashPath(rel),
		Filename:    name,
		Ext:         ext,
		Size:        info.Size(),
		Mtime:       float64(info.ModTime().UnixNano()) / 1e9,
		Inode:       fstat.Inode,
		Device:      fstat.Device,
		Nlink:       fstat.Nlink,
	}, true
}

// RefreshSingleGroup re-stats a single cluster's paths on disk (plus any newly discovered Arr paths)
// and re-evaluates its status against targeted qBittorrent and Arr indices.
// Returns (ComparisonGroup{}, false) if no files remain on disk for this group.
func RefreshSingleGroup(settings config.AppSettings, prev ComparisonGroup, qbitIndex *qbit.Index, arrIndex *arr.Index, discoveredArrPaths []string) (ComparisonGroup, bool) {
	if qbitIndex == nil {
		qbitIndex = qbit.NewIndex()
	}
	if arrIndex == nil {
		arrIndex = arr.NewIndex()
	}

	primaryIDs := primaryFolderIDs(settings)
	hasEnabledArr := hasEnabledArrInstance(settings)
	ignoreExts := buildIgnoreExtSet(settings)

	folderByID := make(map[string]config.FolderConfig, len(settings.Folders))
	for _, f := range settings.Folders {
		role := "secondary"
		if primaryIDs[f.ID] {
			role = "primary"
		}
		fc := f
		fc.Role = role
		folderByID[f.ID] = fc
	}

	prevFiles := make([]ScannedFile, 0, len(prev.FolderAFiles)+len(prev.FolderBFiles))
	prevFiles = append(prevFiles, prev.FolderAFiles...)
	prevFiles = append(prevFiles, prev.FolderBFiles...)

	groupFiles := make([]ScannedFile, 0, len(prevFiles)+len(discoveredArrPaths))
	seenPaths := make(map[string]struct{}, len(prevFiles)+len(discoveredArrPaths))
	existingInodes := make(map[devInodeKey]struct{}, len(prevFiles))

	// Seed inodes from previous scan before Lstat so files moved/renamed by Arr still match by inode.
	for _, pf := range prevFiles {
		if pf.Inode != 0 {
			existingInodes[devInodeKey{Device: pf.Device, Inode: pf.Inode}] = struct{}{}
		}
	}

	for _, pf := range prevFiles {
		pk := pathCompareKey(pf.AbsPath)
		if _, dup := seenPaths[pk]; dup {
			continue
		}
		seenPaths[pk] = struct{}{}

		folderCfg, ok := folderByID[pf.FolderID]
		if !ok {
			if owner, found := findOwningFolder(settings.Folders, primaryIDs, pf.AbsPath); found {
				folderCfg = owner
			} else {
				role := pf.FolderRole
				if primaryIDs[pf.FolderID] {
					role = "primary"
				}
				folderCfg = config.FolderConfig{
					ID:    pf.FolderID,
					Label: pf.FolderLabel,
					Role:  role,
				}
			}
		}

		sf, alive := statScannedFileFromDisk(folderCfg, pf.AbsPath, ignoreExts, settings.MinSizeBytes, false)
		if !alive {
			continue
		}
		groupFiles = append(groupFiles, sf)
		if sf.Inode != 0 {
			existingInodes[devInodeKey{Device: sf.Device, Inode: sf.Inode}] = struct{}{}
		}
	}

	for _, dp := range discoveredArrPaths {
		pk := pathCompareKey(dp)
		if pk == "" {
			continue
		}
		if _, dup := seenPaths[pk]; dup {
			continue
		}
		seenPaths[pk] = struct{}{}

		owner, found := findOwningFolder(settings.Folders, primaryIDs, dp)
		if !found {
			continue
		}
		sf, alive := statScannedFileFromDisk(owner, dp, ignoreExts, settings.MinSizeBytes, true)
		if !alive {
			continue
		}

		_, sameInode := existingInodes[devInodeKey{Device: sf.Device, Inode: sf.Inode}]
		belongs := (sf.Inode != 0 && sameInode) ||
			strings.EqualFold(sf.Filename, prev.Filename) ||
			(prev.Size > 0 && sf.Size == prev.Size && strings.EqualFold(sf.Ext, prev.Ext))
		if !belongs {
			continue
		}
		groupFiles = append(groupFiles, sf)
		if sf.Inode != 0 {
			existingInodes[devInodeKey{Device: sf.Device, Inode: sf.Inode}] = struct{}{}
		}
	}

	if len(groupFiles) == 0 {
		return ComparisonGroup{}, false
	}

	return evaluateCluster(groupFiles, primaryIDs, hasEnabledArr, qbitIndex, arrIndex, prev.ID), true
}

func containsStr(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}
