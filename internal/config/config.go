package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const DefaultConfigPath = "./data/settings.json"

type PathMapping struct {
	RemotePrefix string `json:"remote_prefix"`
	LocalPrefix  string `json:"local_prefix"`
}

type FolderConfig struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Path  string `json:"path"`
	Role  string `json:"role"` // "primary" or "secondary"
}

type QbitInstanceConfig struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	URL          string        `json:"url"`
	Username     string        `json:"username"`
	Password     string        `json:"password"`
	Enabled      bool          `json:"enabled"`
	PathMappings []PathMapping `json:"path_mappings"`
}

type AppSettings struct {
	ReadOnlyMode     bool                 `json:"read_only_mode"`
	Folders          []FolderConfig       `json:"folders"`
	QbitInstances    []QbitInstanceConfig `json:"qbit_instances"`
	IgnoreExtensions []string             `json:"ignore_extensions"`
	MinSizeBytes     int64                `json:"min_size_bytes"`
}

type Manager struct {
	mu         sync.RWMutex
	configPath string
	settings   AppSettings
}

func randomID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "id000001"
	}
	return hex.EncodeToString(b[:])
}

func defaultSettings() AppSettings {
	return AppSettings{
		ReadOnlyMode:     true,
		Folders:          []FolderConfig{},
		QbitInstances:    []QbitInstanceConfig{},
		IgnoreExtensions: []string{},
		MinSizeBytes:     0,
	}
}

func normalizeSettings(in AppSettings) (AppSettings, error) {
	out := AppSettings{
		ReadOnlyMode:     in.ReadOnlyMode,
		Folders:          make([]FolderConfig, 0, len(in.Folders)),
		QbitInstances:    make([]QbitInstanceConfig, 0, len(in.QbitInstances)),
		IgnoreExtensions: make([]string, 0, len(in.IgnoreExtensions)),
		MinSizeBytes:     in.MinSizeBytes,
	}
	if out.MinSizeBytes < 0 {
		out.MinSizeBytes = 0
	}

	hasPrimary := false
	for _, f := range in.Folders {
		label := strings.TrimSpace(f.Label)
		path := strings.TrimSpace(f.Path)
		if label == "" || path == "" {
			return AppSettings{}, errors.New("each folder requires a non-empty label and path")
		}
		id := strings.TrimSpace(f.ID)
		if id == "" {
			id = randomID()
		}
		role := strings.ToLower(strings.TrimSpace(f.Role))
		if role != "primary" {
			role = "secondary"
		} else {
			hasPrimary = true
		}
		out.Folders = append(out.Folders, FolderConfig{
			ID:    id,
			Label: label,
			Path:  path,
			Role:  role,
		})
	}
	if len(out.Folders) > 0 && !hasPrimary {
		out.Folders[0].Role = "primary"
	}

	for _, q := range in.QbitInstances {
		name := strings.TrimSpace(q.Name)
		u := strings.TrimRight(strings.TrimSpace(q.URL), "/")
		if name == "" || u == "" {
			return AppSettings{}, errors.New("each qBittorrent instance requires a non-empty name and url")
		}
		id := strings.TrimSpace(q.ID)
		if id == "" {
			id = randomID()
		}
		mappings := make([]PathMapping, 0, len(q.PathMappings))
		for _, m := range q.PathMappings {
			rp := strings.TrimSpace(m.RemotePrefix)
			lp := strings.TrimSpace(m.LocalPrefix)
			if rp == "" || lp == "" {
				continue
			}
			mappings = append(mappings, PathMapping{RemotePrefix: rp, LocalPrefix: lp})
		}
		out.QbitInstances = append(out.QbitInstances, QbitInstanceConfig{
			ID:           id,
			Name:         name,
			URL:          u,
			Username:     q.Username,
			Password:     q.Password,
			Enabled:      q.Enabled,
			PathMappings: mappings,
		})
	}

	seenExt := make(map[string]struct{}, len(in.IgnoreExtensions))
	for _, raw := range in.IgnoreExtensions {
		ext := strings.ToLower(strings.TrimSpace(raw))
		if ext == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		if _, exists := seenExt[ext]; exists {
			continue
		}
		seenExt[ext] = struct{}{}
		out.IgnoreExtensions = append(out.IgnoreExtensions, ext)
	}

	return out, nil
}

func cloneSettings(s AppSettings) AppSettings {
	out := AppSettings{
		ReadOnlyMode:     s.ReadOnlyMode,
		Folders:          make([]FolderConfig, len(s.Folders)),
		QbitInstances:    make([]QbitInstanceConfig, len(s.QbitInstances)),
		IgnoreExtensions: make([]string, len(s.IgnoreExtensions)),
		MinSizeBytes:     s.MinSizeBytes,
	}
	copy(out.Folders, s.Folders)
	copy(out.IgnoreExtensions, s.IgnoreExtensions)
	for i, q := range s.QbitInstances {
		qc := q
		qc.PathMappings = make([]PathMapping, len(q.PathMappings))
		copy(qc.PathMappings, q.PathMappings)
		out.QbitInstances[i] = qc
	}
	return out
}

func NewManager(configPath string) (*Manager, error) {
	resolved := strings.TrimSpace(configPath)
	if resolved == "" {
		resolved = strings.TrimSpace(os.Getenv("SEEDLINK_CONFIG_PATH"))
	}
	if resolved == "" {
		resolved = DefaultConfigPath
	}

	m := &Manager{
		configPath: resolved,
		settings:   defaultSettings(),
	}
	if err := m.loadOrInit(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) loadOrInit() error {
	data, err := os.ReadFile(m.configPath)
	if err == nil {
		var parsed AppSettings
		if jErr := json.Unmarshal(data, &parsed); jErr == nil {
			if norm, nErr := normalizeSettings(parsed); nErr == nil {
				m.settings = norm
				return nil
			}
		}
	}

	m.settings = defaultSettings()
	return m.writeLocked(m.settings)
}

func (m *Manager) writeLocked(s AppSettings) error {
	dir := filepath.Dir(m.configPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := m.configPath + ".tmp"
	if err := os.WriteFile(tmpPath, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, m.configPath)
}

func (m *Manager) Get() AppSettings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneSettings(m.settings)
}

func (m *Manager) Update(in AppSettings) (AppSettings, error) {
	norm, err := normalizeSettings(in)
	if err != nil {
		return AppSettings{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.writeLocked(norm); err != nil {
		return AppSettings{}, err
	}
	m.settings = norm
	return cloneSettings(m.settings), nil
}

func (m *Manager) SetReadOnly(enabled bool) (AppSettings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := cloneSettings(m.settings)
	next.ReadOnlyMode = enabled
	if err := m.writeLocked(next); err != nil {
		return AppSettings{}, err
	}
	m.settings = next
	return cloneSettings(m.settings), nil
}
