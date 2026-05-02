// Package hosts implements detection and per-host patch/unpatch of MCP
// server entries. JSON hosts (claude-desktop, claude-code, cursor,
// opencode) use inline markers; TOML (codex) and YAML (hermes) use an
// external markers.json under each host's backup directory so the source
// formatting stays untouched and env values are never duplicated.
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

// Format identifies the on-disk shape of a host's MCP config.
type Format string

const (
	FormatJSON Format = "json"
	FormatTOML Format = "toml"
	FormatYAML Format = "yaml"
)

// Host describes a supported MCP host.
type Host struct {
	ID          string
	DisplayName string
	Format      Format
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
			Format:      FormatJSON,
			DefaultPath: claudeDesktopPath,
			ServersKey:  "mcpServers", Marker: MarkerInline,
		},
		"claude-code": {
			ID: "claude-code", DisplayName: "Claude Code",
			Format:      FormatJSON,
			DefaultPath: claudeCodePath,
			ServersKey:  "mcpServers", Marker: MarkerInline,
		},
		"cursor": {
			ID: "cursor", DisplayName: "Cursor",
			Format:      FormatJSON,
			DefaultPath: cursorPath,
			ServersKey:  "mcpServers", Marker: MarkerInline,
		},
		"codex": {
			ID: "codex", DisplayName: "Codex",
			Format:      FormatTOML,
			DefaultPath: codexPath,
			ServersKey:  "mcp_servers", Marker: MarkerExternal,
		},
		"opencode": {
			ID: "opencode", DisplayName: "OpenCode",
			Format:      FormatJSON,
			DefaultPath: openCodePath,
			ServersKey:  "mcp", Marker: MarkerInline,
		},
		"openclaw": {
			ID: "openclaw", DisplayName: "OpenClaw / OpenCode",
			Format:      FormatJSON,
			DefaultPath: openCodePath,
			ServersKey:  "mcp", Marker: MarkerInline,
		},
		"hermes": {
			ID: "hermes", DisplayName: "Hermes",
			Format:      FormatYAML,
			DefaultPath: hermesPath,
			ServersKey:  "mcp_servers", Marker: MarkerExternal,
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

func codexPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

func openCodePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "opencode", "opencode.json"), nil
}

func hermesPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".hermes", "config.yaml"), nil
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
		switch h.Format {
		case FormatJSON, "":
			var any map[string]any
			if json.Unmarshal(b, &any) == nil {
				res.Parsable = true
			}
		case FormatTOML:
			if _, err := loadTOMLServers(b, h.ServersKey); err == nil {
				res.Parsable = true
			}
		case FormatYAML:
			if _, err := loadYAMLServers(b, h.ServersKey); err == nil {
				res.Parsable = true
			}
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
	var (
		patched      []byte
		patchedNames []string
	)
	switch h.Format {
	case FormatTOML:
		patched, patchedNames, err = patchBytesTOMLNamed(bytesBefore, h.ServersKey, servers, proxyExe, backupDir)
	case FormatYAML:
		patched, patchedNames, err = patchBytesYAMLNamed(bytesBefore, h.ServersKey, servers, proxyExe, backupDir)
	default:
		patched, err = patchBytes(bytesBefore, h.ServersKey, servers, proxyExe)
		patchedNames = servers
	}
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
		HashBefore: hashBefore, HashAfter: hashAfter, Servers: patchedNames,
	}, nil
}

// Unpatch reverses Patch. JSON hosts use the inline `_scouttrace.original`
// marker; TOML/YAML hosts read the external markers store under the host's
// backup directory (~/.scouttrace/backups/<id>/markers.json). For TOML/YAML
// hosts callers should pass the same backupDir used at Patch time; passing
// an empty string falls back to a sibling .scouttrace-markers.json beside
// the config file (for ad-hoc unpatching).
func Unpatch(h *Host, configPath string, backupDirs ...string) (*PatchResult, error) {
	backupDir := ""
	if len(backupDirs) > 0 {
		backupDir = backupDirs[0]
	}
	bytesBefore, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	hashBefore := HashConfigBytes(bytesBefore)
	var (
		patched  []byte
		restored []string
	)
	switch h.Format {
	case FormatTOML:
		patched, restored, err = unpatchBytesTOML(bytesBefore, h.ServersKey, externalMarkerDir(configPath, backupDir))
	case FormatYAML:
		patched, restored, err = unpatchBytesYAML(bytesBefore, h.ServersKey, externalMarkerDir(configPath, backupDir))
	default:
		patched, restored, err = unpatchBytes(bytesBefore, h.ServersKey)
	}
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

// externalMarkerDir resolves where to read/write markers.json for hosts
// that don't support inline markers. Empty backupDir means "use markers.json
// in the config directory" (used by ad-hoc tests/CLI invocations).
func externalMarkerDir(configPath, backupDir string) string {
	if backupDir != "" {
		return backupDir
	}
	return filepath.Dir(configPath)
}

// UndoFromBackup restores the most recent backup file over configPath.
// Callers pass the Host so the backup can be validated against the format
// before it's restored — refusing to write back garbage is the only way
// `scouttrace undo` is safe to run unattended.
func UndoFromBackup(h *Host, configPath, backupDir string) (string, error) {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return "", err
	}
	var bks []string
	for _, e := range entries {
		// markers.json sits beside the timestamped .bak files; never restore it.
		if e.IsDir() || e.Name() == "markers.json" {
			continue
		}
		bks = append(bks, e.Name())
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
	if h != nil {
		switch h.Format {
		case FormatJSON, "":
			if !json.Valid(bytesBak) {
				return "", errors.New("hosts: backup is not valid JSON; refusing to restore")
			}
		case FormatTOML:
			if _, err := loadTOMLServers(bytesBak, h.ServersKey); err != nil {
				return "", fmt.Errorf("hosts: backup is not parseable TOML; refusing to restore: %w", err)
			}
		case FormatYAML:
			if _, err := loadYAMLServers(bytesBak, h.ServersKey); err != nil {
				return "", fmt.Errorf("hosts: backup is not parseable YAML; refusing to restore: %w", err)
			}
		}
	} else if !json.Valid(bytesBak) {
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
