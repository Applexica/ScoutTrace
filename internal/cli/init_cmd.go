package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/webhookscout/scouttrace/internal/config"
	"github.com/webhookscout/scouttrace/internal/creds"
	"github.com/webhookscout/scouttrace/internal/event"
	"github.com/webhookscout/scouttrace/internal/hosts"
)

// CmdInit creates a fresh config + ScoutTrace home dir. With --yes it runs
// non-interactively from flags; without --yes it starts the setup wizard.
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
		if ok := runInitWizard(g, initWizardPointers{
			destName: destName, destType: destType, destURL: destURL,
			destination: destination, destPath: destPath, apiBase: apiBase,
			agentID: agentID, agentName: agentName, apiKey: apiKey, authRef: authRef,
			profile: profile, hostsFlag: hostsFlag, dryRun: dryRun,
		}); !ok {
			return 1
		}
	}
	_ = agentName // reserved for future WebhookScout display metadata
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

	// Dry-runs must not consume one-time setup tokens or write credentials.
	// Populate safe placeholder refs so config validation and JSON plan output
	// still show what would be written after a real exchange.
	if *dryRun && *setupToken != "" {
		if *agentID == "" {
			*agentID = "agent_pending_setup_token_exchange"
		}
		if *authRef == "" {
			*authRef = "encfile://default-api-key"
		}
		*setupToken = ""
	}

	// Setup tokens are exchanged immediately for a scoped WebhookScout API
	// key, then the token and key are wiped from command state. The generated
	// key must be stored in the encrypted credential store; otherwise the
	// one-time setup flow would leave the user with an unusable config.
	if *setupToken != "" {
		if os.Getenv("SCOUTTRACE_ENCFILE_PASSPHRASE") == "" {
			fmt.Fprintln(g.Stderr, "init: setup token exchange needs secure local storage before the one-time token is consumed.")
			fmt.Fprintln(g.Stderr, "init: export SCOUTTRACE_ENCFILE_PASSPHRASE and re-run, or use --auth-header-ref env://SCOUTTRACE_WEBHOOKSCOUT_API_KEY with a manually exported key.")
			return 1
		}
		exchangedAgentID, exchangedAPIKey, err := exchangeWebhookScoutSetupToken(ctx, *apiBase, *setupToken, *agentName)
		if err != nil {
			fmt.Fprintf(g.Stderr, "init: setup token exchange failed: %v\n", err)
			return 1
		}
		if exchangedAgentID != "" {
			*agentID = exchangedAgentID
		}
		ref, err := stashOrEnvRef(g, "default-api-key", exchangedAPIKey, "SCOUTTRACE_WEBHOOKSCOUT_API_KEY")
		if err != nil {
			fmt.Fprintf(g.Stderr, "init: exchanged API key could not be stored securely: %v\n", err)
			fmt.Fprintln(g.Stderr, "init: set SCOUTTRACE_ENCFILE_PASSPHRASE and re-run setup token exchange, or use --auth-header-ref env://SCOUTTRACE_WEBHOOKSCOUT_API_KEY with a manually exported key.")
			return 1
		}
		if *authRef == "" {
			*authRef = ref
		}
		fmt.Fprintf(g.Stdout, "init: setup token exchanged; WebhookScout API key stored as %s\n", ref)
		*setupToken = ""
	}
	if *apiKey != "" {
		ref, err := stashOrEnvRef(g, "default-api-key", *apiKey, "SCOUTTRACE_WEBHOOKSCOUT_API_KEY")
		if err != nil {
			fmt.Fprintln(g.Stderr, "init:", err)
			*apiKey = ""
			return 1
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

type initWizardPointers struct {
	destName    *string
	destType    *string
	destURL     *string
	destination *string
	destPath    *string
	apiBase     *string
	agentID     *string
	agentName   *string
	apiKey      *string
	authRef     *string
	profile     *string
	hostsFlag   *string
	dryRun      *bool
}

func runInitWizard(g *Globals, p initWizardPointers) bool {
	pr := &wizardPrompter{w: g.Stdout, r: bufio.NewReader(g.Stdin)}
	fmt.Fprintln(g.Stdout, "ScoutTrace setup wizard")
	fmt.Fprintln(g.Stdout, "This will create a local ScoutTrace config. Secrets are referenced through env://, keychain://, or encfile:// refs and are not written directly to config.")
	fmt.Fprintln(g.Stdout)

	choice := normalizeDestination(pr.ask("Destination (webhookscout, stdout, file, http)", "stdout"))
	if pr.sawEOF {
		fmt.Fprintln(g.Stderr, "init: interactive input unavailable. Re-run with --yes for scripted setup or run in an interactive terminal.")
		return false
	}
	*p.destination = ""
	*p.destType = choice
	switch choice {
	case "webhookscout":
		*p.apiBase = pr.ask("WebhookScout API base", defaultString(*p.apiBase, "https://api.webhookscout.com"))
		*p.agentID = pr.ask("WebhookScout agent ID", *p.agentID)
		if *p.agentID == "" {
			fmt.Fprintln(g.Stderr, "init: WebhookScout agent ID is required. Create/select an agent in the WebhookScout portal, then run init again.")
			return false
		}
		cred := pr.ask("Credential reference or WebhookScout API key", defaultString(*p.authRef, "env://SCOUTTRACE_WEBHOOKSCOUT_API_KEY"))
		if looksLikeCredentialRef(cred) {
			*p.authRef = cred
		} else if strings.TrimSpace(cred) != "" {
			*p.apiKey = cred
			*p.authRef = ""
			fmt.Fprintln(g.Stdout, "WebhookScout API key received; it will be stored securely and only a credential reference will be written to config.")
		}
	case "stdout":
		// No additional destination fields.
	case "file":
		*p.destPath = pr.ask("NDJSON output file", defaultString(*p.destPath, "./scouttrace-events.ndjson"))
	case "http":
		*p.destURL = pr.ask("HTTP destination URL", *p.destURL)
		if *p.destURL == "" {
			fmt.Fprintln(g.Stderr, "init: HTTP destination URL is required")
			return false
		}
		*p.authRef = pr.ask("Credential reference (blank for none)", *p.authRef)
	default:
		fmt.Fprintf(g.Stderr, "init: unsupported destination %q\n", choice)
		return false
	}
	*p.hostsFlag = pr.ask("Hosts to patch (comma-separated claude-desktop,claude-code,cursor or none)", defaultString(*p.hostsFlag, "none"))
	*p.profile = pr.ask("Redaction profile (strict, standard, permissive)", defaultString(*p.profile, "strict"))
	if pr.sawEOF {
		fmt.Fprintln(g.Stderr, "init: interactive input ended before confirmation; no files written. Re-run with --yes for scripted setup.")
		return false
	}
	if *p.destName == "" {
		*p.destName = "default"
	}
	fmt.Fprintln(g.Stdout)
	fmt.Fprintf(g.Stdout, "Plan: destination=%s name=%s hosts=%s redaction=%s config=%s\n", *p.destType, *p.destName, *p.hostsFlag, *p.profile, g.ConfigPath)
	if !yesish(pr.ask("Write this configuration?", "y")) || pr.sawEOF {
		fmt.Fprintln(g.Stdout, "Init cancelled; no files written.")
		return false
	}
	return true
}

type wizardPrompter struct {
	w      io.Writer
	r      *bufio.Reader
	sawEOF bool
}

func (p *wizardPrompter) ask(prompt, def string) string {
	if def != "" {
		fmt.Fprintf(p.w, "%s [%s]: ", prompt, def)
	} else {
		fmt.Fprintf(p.w, "%s: ", prompt)
	}
	line, err := p.r.ReadString('\n')
	if err == io.EOF {
		p.sawEOF = true
	} else if err != nil {
		p.sawEOF = true
		return def
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

func askWizard(w io.Writer, r *bufio.Reader, prompt, def string) string {
	if def != "" {
		fmt.Fprintf(w, "%s [%s]: ", prompt, def)
	} else {
		fmt.Fprintf(w, "%s: ", prompt)
	}
	line, err := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if err != nil && err != io.EOF {
		return def
	}
	if line == "" {
		return def
	}
	return line
}

func normalizeDestination(in string) string {
	s := strings.ToLower(strings.TrimSpace(in))
	switch s {
	case "", "1", "stdout", "local", "local-only":
		return "stdout"
	case "2", "webhookscout", "webhook", "whs":
		return "webhookscout"
	case "3", "file":
		return "file"
	case "4", "http", "https":
		return "http"
	default:
		return s
	}
}

func defaultString(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func looksLikeCredentialRef(v string) bool {
	s := strings.TrimSpace(v)
	return strings.HasPrefix(s, "env://") || strings.HasPrefix(s, "keychain://") || strings.HasPrefix(s, "encfile://")
}

func yesish(v string) bool {
	s := strings.ToLower(strings.TrimSpace(v))
	return s == "" || s == "y" || s == "yes" || s == "true" || s == "1"
}

type setupTokenExchangeResponse struct {
	AgentID string   `json:"agent_id"`
	APIKey  string   `json:"api_key"`
	Scopes  []string `json:"scopes,omitempty"`
}

func exchangeWebhookScoutSetupToken(ctx context.Context, apiBase, token, agentName string) (string, string, error) {
	apiBase = strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if apiBase == "" {
		return "", "", fmt.Errorf("api base required")
	}
	parsed, err := url.Parse(apiBase)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", "", fmt.Errorf("valid api base URL required")
	}
	if parsed.Scheme != "https" && !isLocalHTTPHost(parsed.Hostname()) {
		return "", "", fmt.Errorf("setup token exchange requires https api base; plain http is allowed only for localhost development")
	}
	if strings.TrimSpace(token) == "" {
		return "", "", fmt.Errorf("setup token required")
	}
	body := map[string]string{"token": token}
	if strings.TrimSpace(agentName) != "" {
		body["agent_name"] = agentName
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/v1/setup-tokens/exchange", bytes.NewReader(buf))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ScoutTrace")
	client := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		return "", "", fmt.Errorf("exchange endpoint returned HTTP %d", resp.StatusCode)
	}
	var out setupTokenExchangeResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", "", fmt.Errorf("decode exchange response: %w", err)
	}
	if out.AgentID == "" {
		return "", "", fmt.Errorf("exchange response missing agent_id")
	}
	if out.APIKey == "" {
		return "", "", fmt.Errorf("exchange response missing api_key")
	}
	return out.AgentID, out.APIKey, nil
}

func isLocalHTTPHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(h)
	return err == nil && addr.IsLoopback()
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
	keychainRef := "keychain://scouttrace/webhookscout/default"
	if err := creds.NewKeychainStore().Put(strings.TrimPrefix(keychainRef, "keychain://"), value); err == nil {
		return keychainRef, nil
	}
	if err := stashSecret(g, key, value); err == nil {
		return "encfile://" + key, nil
	}
	return "", fmt.Errorf("secure credential storage unavailable; set SCOUTTRACE_ENCFILE_PASSPHRASE and retry, or export %s and use --auth-header-ref env://%s", envFallback, envFallback)
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
