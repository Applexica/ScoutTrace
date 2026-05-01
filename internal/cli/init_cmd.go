package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/webhookscout/scouttrace/internal/config"
	"github.com/webhookscout/scouttrace/internal/creds"
	"github.com/webhookscout/scouttrace/internal/event"
	"github.com/webhookscout/scouttrace/internal/hosts"
)

// CmdInit creates a fresh config + ScoutTrace home dir. The MVP build is
// strictly non-interactive: callers must pass --yes, --destination,
// --type, and either --url/--path or rely on stdout type. The wizard
// described in PRD §6 is post-MVP and lives behind --interactive (TODO).
func CmdInit(ctx context.Context, g *Globals, args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(g.Stderr)
	yes := fs.Bool("yes", false, "non-interactive mode")
	dryRun := fs.Bool("dry-run", false, "print plan and exit without writing")
	hostsFlag := fs.String("hosts", "", "comma-separated host ids to patch (or 'none')")
	destName := fs.String("destination-name", "default", "name of the destination")
	destType := fs.String("type", "stdout", "destination type: http|file|stdout|webhookscout")
	destURL := fs.String("url", "", "url for http/webhookscout destination")
	destination := fs.String("destination", "", "destination shorthand: webhookscout|stdout|file://...|https://...")
	destPath := fs.String("path", "", "path for file destination")
	apiBase := fs.String("api-base", "https://api.webhookscout.com", "api base for webhookscout")
	apiURL := fs.String("api-url", "", "alias for --api-base")
	agentID := fs.String("agent-id", "", "agent id for webhookscout")
	agentName := fs.String("agent-name", "", "friendly agent/source name")
	setupToken := fs.String("setup-token", "", "short-lived WebhookScout setup token (not persisted)")
	apiKey := fs.String("api-key", "", "WebhookScout API key env/keychain source (not written to config)")
	authHeader := fs.String("auth-header", "", "custom auth header; use env/keychain in production")
	authRef := fs.String("auth-header-ref", "", "credential ref like env://NAME or keychain://...")
	profile := fs.String("profile", "strict", "redaction profile: strict|standard|permissive")
	apply := withSubJSON(fs, g)
	if err := fs.Parse(args); err != nil {
		return 64
	}
	apply()
	if !*yes {
		fmt.Fprintln(g.Stderr, "scouttrace init: interactive wizard not implemented in MVP. Re-run with --yes.")
		return 64
	}
	_ = agentName // surfaced via webhookscout adapter post-MVP
	if *apiURL != "" {
		*apiBase = *apiURL
	}
	if *destination != "" {
		switch {
		case *destination == "webhookscout":
			*destType = "webhookscout"
		case *destination == "stdout":
			*destType = "stdout"
		case len(*destination) >= 8 && (*destination)[:8] == "https://":
			*destType = "http"
			*destURL = *destination
		case len(*destination) >= 7 && (*destination)[:7] == "http://":
			*destType = "http"
			*destURL = *destination
		case len(*destination) >= 7 && (*destination)[:7] == "file://":
			*destType = "file"
			*destPath = (*destination)[7:]
		default:
			fmt.Fprintf(g.Stderr, "unsupported --destination %q\n", *destination)
			return 64
		}
	}
	// If the user picked any webhookscout-flavoured flag, default the
	// destination type to webhookscout so the resulting config is coherent.
	if *setupToken != "" || *apiKey != "" {
		if *destType != "webhookscout" {
			*destType = "webhookscout"
		}
	}

	// Secure secret handling: tokens are never written to config in
	// plaintext. We stash them in the encrypted-file store when a
	// passphrase is available; otherwise we record an env-ref the user
	// can populate later.
	//
	// IMPORTANT: a setup token is NOT a usable Authorization header — it
	// must be exchanged for an api_key first. We persist it for the
	// post-MVP exchange flow but never set auth_header_ref to point at it.
	if *setupToken != "" {
		if err := stashSecret(g, "default-setup-token", *setupToken); err != nil {
			fmt.Fprintf(g.Stderr, "init: setup token not stashed: %v\n", err)
			fmt.Fprintln(g.Stderr, "init: run with SCOUTTRACE_ENCFILE_PASSPHRASE set, or pass --api-key once you exchange the token.")
		} else {
			fmt.Fprintln(g.Stdout, "init: setup token stashed (encfile://default-setup-token). Exchange via the WebhookScout console; then re-run init with --api-key.")
		}
		*setupToken = ""
	}
	if *apiKey != "" {
		ref, err := stashOrEnvRef(g, "default-api-key", *apiKey, "SCOUTTRACE_WEBHOOKSCOUT_API_KEY")
		if err != nil {
			fmt.Fprintln(g.Stderr, "init:", err)
		}
		*apiKey = ""
		if *authRef == "" {
			*authRef = ref
		}
	}
	if *authHeader != "" && *authRef == "" {
		*authRef = "env://SCOUTTRACE_AUTH_HEADER"
	}

	// WebhookScout requires a non-empty agent_id at adapter construction
	// time. If the user did not pass one, synthesize a placeholder so the
	// resulting config is coherent (the real id arrives during exchange).
	if *destType == "webhookscout" && *agentID == "" {
		*agentID = "agent_pending_" + event.NewULID()
		fmt.Fprintf(g.Stderr, "init: synthesized placeholder agent_id=%s; replace with the real id once available.\n", *agentID)
	}
	if err := ensureHome(g); err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	c := &config.Config{
		SchemaVersion:      1,
		DefaultDestination: *destName,
		Hosts:              map[string]config.HostRef{},
		Servers:            []config.ServerEntry{{NameGlob: "*", Destination: *destName}},
		Destinations: []config.DestinationEntry{{
			Name:          *destName,
			Type:          *destType,
			URL:           *destURL,
			Path:          *destPath,
			APIBase:       *apiBase,
			AgentID:       *agentID,
			AuthHeaderRef: *authRef,
		}},
		Redaction: config.RedactionConfig{Profile: *profile},
		Queue: config.QueueConfig{
			Path:         g.queuePath(),
			MaxRowBytes:  2 * 1024 * 1024,
			MaxAgeDays:   7,
			DropWhenFull: "oldest",
		},
		Delivery: config.DeliveryConfig{
			InitialBackoffMS: 500, MaxBackoffMS: 60_000, MaxRetries: 8, Jitter: true,
		},
		Capture: config.CaptureConfig{MaxArgBytes: 64 * 1024, MaxResultBytes: 256 * 1024},
	}
	if err := c.Validate(); err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 78
	}
	if *dryRun {
		_ = printJSON(g.Stdout, c, true)
		return 0
	}
	if err := config.Save(g.ConfigPath, c); err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	if err := os.MkdirAll(g.queuePath(), 0o700); err != nil {
		fmt.Fprintln(g.Stderr, err)
		return 1
	}
	if a := newAudit(g); a != nil {
		_ = a.Append("cli", "config_load", map[string]any{"path": g.ConfigPath})
	}
	patched := patchSelectedHosts(g, c, *hostsFlag)
	if !g.JSON {
		fmt.Fprintf(g.Stdout, "Wrote %s\n", g.ConfigPath)
		fmt.Fprintf(g.Stdout, "Default destination: %s (type=%s)\n", *destName, *destType)
		if len(patched) > 0 {
			fmt.Fprintf(g.Stdout, "Patched hosts: %s\n", strings.Join(patched, ", "))
		}
	}
	return 0
}

// stashSecret writes a secret into the encrypted-file store when the user
// has set SCOUTTRACE_ENCFILE_PASSPHRASE; otherwise it returns an error so
// the caller can decide on the fallback. Production builds add OS keychain.
func stashSecret(g *Globals, key, value string) error {
	pp := os.Getenv("SCOUTTRACE_ENCFILE_PASSPHRASE")
	if pp == "" {
		return fmt.Errorf("encfile passphrase not set; export SCOUTTRACE_ENCFILE_PASSPHRASE or pre-populate the env var pointed to by --auth-header-ref")
	}
	if err := os.MkdirAll(g.Home, 0o700); err != nil {
		return err
	}
	st := creds.NewEncFileStore(filepath.Join(g.Home, "credentials.enc"), []byte(pp))
	if err := st.Put(key, value); err != nil {
		return fmt.Errorf("encfile.Put: %w", err)
	}
	return nil
}

// stashOrEnvRef tries to stash the secret in the encrypted-file store. If
// that fails (no passphrase), the user is told to populate envFallback
// and the function returns env://envFallback so the config still resolves.
func stashOrEnvRef(g *Globals, key, value, envFallback string) (string, error) {
	if err := stashSecret(g, key, value); err == nil {
		return "encfile://" + key, nil
	}
	return "env://" + envFallback, fmt.Errorf("encfile not configured; set %s before running ScoutTrace", envFallback)
}

// patchSelectedHosts patches each host id listed in flag (comma-separated).
// "none" or empty are no-ops. Failures are reported but do not block init —
// users can re-run `scouttrace hosts patch` later.
func patchSelectedHosts(g *Globals, c *config.Config, flag string) []string {
	flag = strings.TrimSpace(flag)
	if flag == "" || flag == "none" {
		return nil
	}
	var out []string
	for _, id := range strings.Split(flag, ",") {
		id = strings.TrimSpace(id)
		h, err := hosts.LookupHost(id)
		if err != nil {
			fmt.Fprintln(g.Stderr, "init: skip host:", err)
			continue
		}
		path, err := h.DefaultPath()
		if err != nil {
			fmt.Fprintln(g.Stderr, "init: skip host:", err)
			continue
		}
		if _, err := os.Stat(path); err != nil {
			fmt.Fprintf(g.Stderr, "init: skip %s: config not found at %s\n", id, path)
			continue
		}
		bak := filepath.Join(g.Home, "backups", id)
		res, err := hosts.Patch(h, path, nil, g.ScoutBinary, bak, false, "")
		if err != nil {
			fmt.Fprintf(g.Stderr, "init: skip %s: %v\n", id, err)
			continue
		}
		if c.Hosts == nil {
			c.Hosts = map[string]config.HostRef{}
		}
		c.Hosts[id] = config.HostRef{
			ConfigPath: path, LastPatchedAt: res.WrittenAt.Format("2006-01-02T15-04-05Z"),
			LastPatchedHash: res.HashAfter, BackupPath: res.BackupPath,
		}
		_ = config.Save(g.ConfigPath, c)
		if a := newAudit(g); a != nil {
			_ = a.Append("cli", "hosts_patch", map[string]any{"host": id, "via": "init"})
		}
		out = append(out, id)
	}
	return out
}
