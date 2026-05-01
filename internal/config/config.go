// Package config loads, validates, and migrates the ScoutTrace user config.
//
// MVP file format is JSON. The schema is otherwise the YAML described in
// PRD §8 / TECHNICAL_DESIGN §14: hosts + destinations + delivery + queue +
// redaction blocks.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Config is the top-level user config.
type Config struct {
	SchemaVersion      int                 `json:"schema_version"`
	DefaultDestination string              `json:"default_destination,omitempty"`
	Hosts              map[string]HostRef  `json:"hosts,omitempty"`
	Servers            []ServerEntry       `json:"servers,omitempty"`
	Destinations       []DestinationEntry  `json:"destinations,omitempty"`
	Delivery           DeliveryConfig      `json:"delivery,omitempty"`
	Queue              QueueConfig         `json:"queue,omitempty"`
	Redaction          RedactionConfig     `json:"redaction,omitempty"`
	Capture            CaptureConfig       `json:"capture,omitempty"`
	SelfTelemetry      SelfTelemetryConfig `json:"self_telemetry,omitempty"`
}

// HostRef carries patch bookkeeping for a host.
type HostRef struct {
	ConfigPath      string `json:"config_path"`
	LastPatchedAt   string `json:"last_patched_at,omitempty"`
	LastPatchedHash string `json:"last_patched_hash,omitempty"`
	BackupPath      string `json:"backup_path,omitempty"`
	VersionSeen     string `json:"version_seen,omitempty"`
}

// ServerEntry binds a server-name pattern to a destination.
type ServerEntry struct {
	NameGlob    string `json:"name_glob"`
	Destination string `json:"destination,omitempty"`
}

// DestinationEntry is a single destination configuration.
type DestinationEntry struct {
	Name          string            `json:"name"`
	Type          string            `json:"type"` // http | file | stdout | webhookscout
	URL           string            `json:"url,omitempty"`
	Path          string            `json:"path,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	AuthHeaderRef string            `json:"auth_header_ref,omitempty"`
	APIBase       string            `json:"api_base,omitempty"`
	AgentID       string            `json:"agent_id,omitempty"`
	UseGzip       bool              `json:"use_gzip,omitempty"`
	RotateMB      int               `json:"rotate_mb,omitempty"`
	Keep          int               `json:"keep,omitempty"`
	TimeoutMS     int               `json:"timeout_ms,omitempty"`
}

// DeliveryConfig controls retries.
type DeliveryConfig struct {
	InitialBackoffMS int  `json:"initial_backoff_ms,omitempty"`
	MaxBackoffMS     int  `json:"max_backoff_ms,omitempty"`
	MaxRetries       int  `json:"max_retries,omitempty"`
	Jitter           bool `json:"jitter,omitempty"`
}

// QueueConfig controls the local queue.
type QueueConfig struct {
	Path         string `json:"path,omitempty"`
	MaxRowBytes  int    `json:"max_row_bytes,omitempty"`
	MaxBytes     int64  `json:"max_bytes,omitempty"`
	DropWhenFull string `json:"drop_when_full,omitempty"`
	MaxAgeDays   int    `json:"max_age_days,omitempty"`
}

// RedactionConfig selects a built-in profile or path to a custom one.
type RedactionConfig struct {
	Profile string `json:"profile,omitempty"`
	Path    string `json:"path,omitempty"`
}

// CaptureConfig configures capture-level rules.
type CaptureConfig struct {
	MaxArgBytes    int                  `json:"max_arg_bytes,omitempty"`
	MaxResultBytes int                  `json:"max_result_bytes,omitempty"`
	Servers        []CaptureServerEntry `json:"servers,omitempty"`
}

// CaptureServerEntry mirrors redact.CaptureServer at the config layer.
type CaptureServerEntry struct {
	NameGlob      string `json:"name_glob"`
	CaptureArgs   *bool  `json:"capture_args,omitempty"`
	CaptureResult *bool  `json:"capture_result,omitempty"`
}

// SelfTelemetryConfig controls anonymous self-telemetry (off by default).
type SelfTelemetryConfig struct {
	Enabled bool `json:"enabled,omitempty"`
}

// Errors / codes.
const (
	ErrCodeConfigParse        = "E_CONFIG_PARSE"
	ErrCodeConfigUnknownField = "E_CONFIG_UNKNOWN_FIELD"
	ErrCodeConfigRefInvalid   = "E_CONFIG_REF_INVALID"
	ErrCodePlaintextAuth      = "E_PLAINTEXT_AUTH"
	ErrCodeDestNotFound       = "E_DEST_NOT_FOUND"
	ErrCodeDestDuplicate      = "E_DEST_DUPLICATE_NAME"
)

// Error wraps a code + message.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func newErr(code, msg string) error { return &Error{Code: code, Message: msg} }

// CredentialRefRegex matches a valid auth_header_ref.
var credentialRefRegex = regexp.MustCompile(`^(keychain|env|encfile)://`)

// authHeaderNameRegex matches header names that are credential-shaped.
var authHeaderNameRegex = regexp.MustCompile(`(?i)^(?:x-.*)?(?:api[-_]?key|auth[-_]?token|secret|token)$`)

// Load reads a config file and returns a fully-validated Config.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse parses raw bytes into a Config and validates them. The bytes may
// be JSON or a small YAML subset (see yaml.go). Unknown top-level fields
// are rejected, mirroring yaml.v3's KnownFields(true).
func Parse(b []byte) (*Config, error) {
	jsonBytes, err := normalizeToJSON(b)
	if err != nil {
		return nil, newErr(ErrCodeConfigParse, err.Error())
	}
	var c Config
	dec := json.NewDecoder(stripBOM(jsonBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, newErr(ErrCodeConfigParse, err.Error())
	}
	if err := c.expand(); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// normalizeToJSON returns JSON bytes for `in`, accepting either JSON or a
// minimal YAML subset. JSON is tried first because it is unambiguous; any
// JSON document is also valid YAML, so the YAML path only runs when the
// JSON parser fails.
func normalizeToJSON(in []byte) ([]byte, error) {
	trimmed := trimSpaceBytes(in)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		// JSON shape; return as-is.
		var probe any
		if err := json.Unmarshal(trimmed, &probe); err == nil {
			return in, nil
		}
	}
	// Try YAML.
	out, err := yamlToJSON(in)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func trimSpaceBytes(b []byte) []byte {
	i := 0
	for i < len(b) {
		c := b[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			i++
			continue
		}
		break
	}
	return b[i:]
}

func stripBOM(b []byte) *bomReader {
	return &bomReader{b: b}
}

type bomReader struct {
	b []byte
	i int
}

func (r *bomReader) Read(p []byte) (int, error) {
	if r.i == 0 && len(r.b) >= 3 && r.b[0] == 0xEF && r.b[1] == 0xBB && r.b[2] == 0xBF {
		r.i = 3
	}
	if r.i >= len(r.b) {
		return 0, errEOFCfg
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

var errEOFCfg = errors.New("EOF")

// expand expands ~ and $VAR in path-shaped fields.
func (c *Config) expand() error {
	home, _ := os.UserHomeDir()
	expand := func(s string) string {
		if strings.HasPrefix(s, "~/") {
			return filepath.Join(home, s[2:])
		}
		return os.ExpandEnv(s)
	}
	c.Queue.Path = expand(c.Queue.Path)
	for k, v := range c.Hosts {
		v.ConfigPath = expand(v.ConfigPath)
		c.Hosts[k] = v
	}
	for i := range c.Destinations {
		c.Destinations[i].Path = expand(c.Destinations[i].Path)
	}
	c.Redaction.Path = expand(c.Redaction.Path)
	return nil
}

// Validate enforces the §14.2 rules.
func (c *Config) Validate() error {
	seen := map[string]struct{}{}
	for _, d := range c.Destinations {
		if d.Name == "" {
			return newErr(ErrCodeConfigParse, "destination missing name")
		}
		if _, dup := seen[d.Name]; dup {
			return newErr(ErrCodeDestDuplicate, "destination name duplicated: "+d.Name)
		}
		seen[d.Name] = struct{}{}
		if err := validateDest(d); err != nil {
			return err
		}
	}
	if c.DefaultDestination != "" {
		if _, ok := seen[c.DefaultDestination]; !ok {
			return newErr(ErrCodeDestNotFound, "default_destination references unknown: "+c.DefaultDestination)
		}
	}
	for _, s := range c.Servers {
		if s.Destination != "" {
			if _, ok := seen[s.Destination]; !ok {
				return newErr(ErrCodeDestNotFound, "server "+s.NameGlob+" → unknown destination: "+s.Destination)
			}
		}
	}
	if c.Delivery.MaxBackoffMS != 0 && c.Delivery.InitialBackoffMS != 0 &&
		c.Delivery.MaxBackoffMS < c.Delivery.InitialBackoffMS {
		return newErr(ErrCodeConfigParse, "delivery.max_backoff_ms < initial_backoff_ms")
	}
	if c.Capture.MaxArgBytes > 16*1024*1024 {
		return newErr(ErrCodeConfigParse, "capture.max_arg_bytes > 16 MiB cap")
	}
	if c.Capture.MaxResultBytes > 16*1024*1024 {
		return newErr(ErrCodeConfigParse, "capture.max_result_bytes > 16 MiB cap")
	}
	return nil
}

func validateDest(d DestinationEntry) error {
	switch d.Type {
	case "http", "webhookscout":
		// Type-specific
	case "file":
		if d.Path == "" {
			return newErr(ErrCodeConfigParse, fmt.Sprintf("destination %s: file requires path", d.Name))
		}
	case "stdout":
		// no fields required
	default:
		return newErr(ErrCodeConfigParse, fmt.Sprintf("destination %s: unknown type %q", d.Name, d.Type))
	}
	if d.AuthHeaderRef != "" && !credentialRefRegex.MatchString(d.AuthHeaderRef) {
		return newErr(ErrCodeConfigRefInvalid,
			fmt.Sprintf("destination %s: auth_header_ref must be keychain://, env://, or encfile://", d.Name))
	}
	for k, v := range d.Headers {
		if strings.EqualFold(k, "Authorization") {
			return newErr(ErrCodePlaintextAuth,
				fmt.Sprintf("destination %s: plaintext Authorization header rejected; use auth_header_ref", d.Name))
		}
		if authHeaderNameRegex.MatchString(k) {
			return newErr(ErrCodePlaintextAuth,
				fmt.Sprintf("destination %s: header %q looks like a credential; use auth_header_ref", d.Name, k))
		}
		if looksLikeSecret(v) {
			return newErr(ErrCodePlaintextAuth,
				fmt.Sprintf("destination %s: header %q value looks like a credential", d.Name, k))
		}
	}
	return nil
}

// looksLikeSecret runs the strict redaction corpus regexes against `v`.
func looksLikeSecret(v string) bool {
	for _, pat := range secretValueRegexps {
		if pat.MatchString(v) {
			return true
		}
	}
	return false
}

var secretValueRegexps = []*regexp.Regexp{
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`ghp_[A-Za-z0-9]{36,}`),
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`whs_(?:live|test)_[A-Za-z0-9]{16,}`),
	regexp.MustCompile(`(?i)^bearer\s+[A-Za-z0-9._\-]{16,}$`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_=-]{8,}\.[A-Za-z0-9_=-]{8,}\.[A-Za-z0-9_.+/=-]{8,}`),
}

// Save writes the config back to disk in JSON form, with mode 0600.
func Save(path string, c *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LookupDestination returns a destination by name, or nil if not found.
func (c *Config) LookupDestination(name string) *DestinationEntry {
	for i := range c.Destinations {
		if c.Destinations[i].Name == name {
			return &c.Destinations[i]
		}
	}
	return nil
}
