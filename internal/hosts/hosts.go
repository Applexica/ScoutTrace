// Package hosts implements detection and per-host patch/unpatch of MCP
// server entries. The MVP supports the JSON-shaped hosts (claude-desktop,
// claude-code, cursor); TOML/Continue hosts ship as descriptors only and
// should be filled in once their fixtures are added to testdata/.
package hosts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Host describes a supported MCP host.
type Host struct {
	ID          string
	DisplayName string
	DefaultPath func() (string, error)
	ServersKey  string // top-level key under which servers live
	Marker      MarkerStrategy
}

// MarkerStrategy: where to keep ScoutTrace's per-server backup metadata.
type MarkerStrategy string

const (
	// MarkerInline stores _scouttrace inside the server entry.
	MarkerInline MarkerStrategy = "inline"
	// MarkerExternal stores backup data in ~/.scouttrace/backups/<id>/markers.json
	// for hosts that strip unknown keys.
	MarkerExternal MarkerStrategy = "external"
)

// PatchResult describes a successful patch.
type PatchResult struct {
	BackupPath string
	WrittenAt  time.Time
	HashBefore string
	HashAfter  string
	Servers    []string
}

// ServerEntry is the typed view of an entry under mcpServers.<name>.
type ServerEntry struct {
	Command string         `json:"command"`
	Args    []string       `json:"args,omitempty"`
	Env     map[string]any `json:"env,omitempty"`
	Marker  *Marker        `json:"_scouttrace,omitempty"`
	Extra   map[string]any `json:"-"`
}

// Marker is the inline backup metadata.
type Marker struct {
	Managed  bool            `json:"managed"`
	Version  string          `json:"version"`
	Original *ServerOriginal `json:"original,omitempty"`
}

// ServerOriginal is the pre-patch snapshot of a server entry.
type ServerOriginal struct {
	Command string         `json:"command"`
	Args    []string       `json:"args,omitempty"`
	Env     map[string]any `json:"env,omitempty"`
}

// Registry returns the supported-host registry.
func Registry() map[string]*Host {
	return map[string]*Host{
		"claude-desktop": {
			ID: "claude-desktop", DisplayName: "Claude Desktop",
			DefaultPath: claudeDesktopPath,
			ServersKey:  "mcpServers", Marker: MarkerInline,
		},
		"claude-code": {
			ID: "claude-code", DisplayName: "Claude Code",
			DefaultPath: claudeCodePath,
			ServersKey:  "mcpServers", Marker: MarkerInline,
		},
		"cursor": {
			ID: "cursor", DisplayName: "Cursor",
			DefaultPath: cursorPath,
			ServersKey:  "mcpServers", Marker: MarkerInline,
		},
	}
}

func claudeDesktopPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	// macOS default; production discriminates on GOOS.
	return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json"), nil
}

func claudeCodePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "config.json"), nil
}

func cursorPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cursor", "mcp.json"), nil
}

// LookupHost returns a registered host by id.
func LookupHost(id string) (*Host, error) {
	h, ok := Registry()[id]
	if !ok {
		return nil, fmt.Errorf("hosts: unknown host %q", id)
	}
	return h, nil
}

// DetectResult describes whether a host is present.
type DetectResult struct {
	Installed  bool
	ConfigPath string
	Parsable   bool
}

// Detect tries to find a host's config file at the default location.
func Detect(h *Host) (DetectResult, error) {
	path, err := h.DefaultPath()
	if err != nil {
		return DetectResult{}, err
	}
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DetectResult{ConfigPath: path}, nil
		}
		return DetectResult{}, err
	}
	if st.IsDir() {
		return DetectResult{ConfigPath: path}, nil
	}
	res := DetectResult{Installed: true, ConfigPath: path}
	if b, err := os.ReadFile(path); err == nil {
		var any map[string]any
		if json.Unmarshal(b, &any) == nil {
			res.Parsable = true
		}
	}
	return res, nil
}

// HashConfigBytes returns a stable hex SHA-256 of the supplied bytes.
func HashConfigBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Patch wraps the listed server entries with a `scouttrace proxy` shim.
//
//   - Reads original bytes, computes a hash.
//   - Stages a temp file in the same directory as the target.
//   - fsyncs and atomically renames over the original.
//   - Re-reads to verify; on failure, restores the backup.
//
// `proxyExe` is the absolute path to the scouttrace binary; the patched
// entries will spawn it with `proxy --server-name <name> -- <orig...>`.
func Patch(h *Host, configPath string, servers []string, proxyExe string, backupDir string, force bool, recordedHash string) (*PatchResult, error) {
	if configPath == "" {
		var err error
		configPath, err = h.DefaultPath()
		if err != nil {
			return nil, err
		}
	}
	bytesBefore, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("hosts: read %s: %w", configPath, err)
	}
	hashBefore := HashConfigBytes(bytesBefore)
	if recordedHash != "" && recordedHash != hashBefore && !force {
		return nil, fmt.Errorf("E_HOST_CONFIG_DRIFT: %s changed since last patch; use --force", configPath)
	}
	// Backup.
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return nil, err
	}
	bakPath := filepath.Join(backupDir, time.Now().UTC().Format("2006-01-02T15-04-05Z")+".bak")
	if err := os.WriteFile(bakPath, bytesBefore, 0o600); err != nil {
		return nil, err
	}
	// Patch.
	patched, err := patchBytes(bytesBefore, h.ServersKey, servers, proxyExe)
	if err != nil {
		return nil, err
	}
	// Atomic write.
	tmp := configPath + ".scouttrace-tmp"
	if err := os.WriteFile(tmp, patched, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, configPath); err != nil {
		return nil, err
	}
	// Verify re-read.
	verify, err := os.ReadFile(configPath)
	if err != nil {
		_ = os.Rename(bakPath, configPath)
		return nil, fmt.Errorf("hosts: verify read failed: %w", err)
	}
	hashAfter := HashConfigBytes(verify)
	return &PatchResult{
		BackupPath: bakPath, WrittenAt: time.Now().UTC(),
		HashBefore: hashBefore, HashAfter: hashAfter, Servers: servers,
	}, nil
}

// Unpatch reverses Patch by reading the inline `_scouttrace.original`
// marker on each managed server entry.
func Unpatch(h *Host, configPath string) (*PatchResult, error) {
	bytesBefore, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	hashBefore := HashConfigBytes(bytesBefore)
	patched, restored, err := unpatchBytes(bytesBefore, h.ServersKey)
	if err != nil {
		return nil, err
	}
	tmp := configPath + ".scouttrace-tmp"
	if err := os.WriteFile(tmp, patched, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, configPath); err != nil {
		return nil, err
	}
	verify, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	return &PatchResult{
		WrittenAt:  time.Now().UTC(),
		HashBefore: hashBefore, HashAfter: HashConfigBytes(verify),
		Servers: restored,
	}, nil
}

// UndoFromBackup restores the most recent backup file over configPath.
func UndoFromBackup(configPath, backupDir string) (string, error) {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return "", err
	}
	var bks []string
	for _, e := range entries {
		if !e.IsDir() {
			bks = append(bks, e.Name())
		}
	}
	if len(bks) == 0 {
		return "", errors.New("hosts: no backups available")
	}
	sort.Strings(bks)
	latest := filepath.Join(backupDir, bks[len(bks)-1])
	bytesBak, err := os.ReadFile(latest)
	if err != nil {
		return "", err
	}
	if !json.Valid(bytesBak) {
		return "", errors.New("hosts: backup is not valid JSON; refusing to restore")
	}
	tmp := configPath + ".scouttrace-tmp"
	if err := os.WriteFile(tmp, bytesBak, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, configPath); err != nil {
		return "", err
	}
	return latest, nil
}
