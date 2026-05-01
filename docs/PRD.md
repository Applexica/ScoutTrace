# ScoutTrace — Product Requirements Document

**Status:** Draft v0.1
**Owner:** ScoutTrace maintainers
**Last updated:** 2026-04-30
**License (intended):** Apache-2.0
**Repository (intended):** `github.com/webhookscout/scouttrace`

---

## 1. Summary

ScoutTrace is a **local, open-source CLI and MCP proxy** that makes LLM tool-call observability trivial. It transparently wraps Model Context Protocol (MCP) servers — and, in later milestones, raw SDK tool calls — capturing structured metadata about each call (tool name, arguments, results, latency, errors) and forwarding the captured payloads to **any HTTP endpoint** the user configures.

The default destination is **WebhookScout** (`https://api.webhookscout.com`), but ScoutTrace is **destination-agnostic**: users can point it at their own webhook, an internal observability sink, a local file, or `stdout`. ScoutTrace is privacy-first by default — capture is opt-in per field class, redaction is on by default, and **no network egress occurs without an explicit destination**.

ScoutTrace is the *open* on-ramp to the WebhookScout ecosystem. It must be useful, trustworthy, and complete on its own.

---

## 2. Vision

> *"`brew install scouttrace && scouttrace init` should turn any MCP-enabled assistant into a fully observable agent in under 2 minutes — without leaking a single token, secret, or PII string the user didn't approve."*

ScoutTrace exists because today, when something goes wrong inside an MCP-powered agent (a tool returns a 500, a model hallucinates a tool name, latency spikes), there is no out-of-the-box way to **see what happened**. Native MCP hosts (Claude Desktop, Cursor, Continue, Windsurf, Hermes) each produce only fragmentary, host-specific logs. SDK users have to instrument tool calls by hand.

ScoutTrace solves this with a single, host-agnostic, transport-level proxy that any host can be reconfigured to use in seconds.

---

## 3. Goals & Non-Goals

### 3.1 Goals (MVP)

1. **Frictionless install.** A single binary (`scouttrace`) installable via `brew`, `npm i -g`, `pipx`, or downloadable static binary. No daemon required.
2. **Zero-config default path.** `scouttrace init` patches the user's chosen host (Claude Desktop / Code / Cursor / Windsurf / Continue / Hermes) to route MCP servers through the proxy with sensible defaults.
3. **Transparent MCP stdio passthrough.** The proxy is byte-for-byte transparent at the MCP wire level. If ScoutTrace is unavailable, the host call must fail safely (with a clear, opt-in fallback to direct exec).
4. **Pluggable destination.** First-class support for sending to any HTTP(S) endpoint. WebhookScout is the default but never the only option.
5. **Privacy-first capture.** Redaction defaults that strip secrets, PII, and large blobs. Dry-run preview before any network egress.
6. **Local durable queue.** Captured events survive ScoutTrace and host restarts. At-least-once delivery with idempotency keys.
7. **Trust by inspection.** Every command supports `--dry-run`, `--print-config`, and `--diff`. No silent state changes.

### 3.2 Goals (post-MVP)

- Optional **HTTP proxy mode** for OpenAI/Anthropic/Bedrock SDK tool calls.
- **Native SDK shims** for Python (`anthropic`, `openai`) and TypeScript that wrap tool execution.
- **Plugin model** for non-HTTP destinations (S3, Kafka, OpenTelemetry/OTLP).
- **Trace correlation** via OpenTelemetry-compatible IDs.

### 3.3 Non-Goals

- ScoutTrace is **not** an LLM gateway — it does not load-balance, cache, retry, or rewrite model requests.
- ScoutTrace is **not** a UI. Visualization lives in the destination (WebhookScout, Grafana, etc.).
- ScoutTrace is **not** a hosted service. It runs entirely on the user's machine.
- ScoutTrace does **not** persist captured payloads beyond the durable queue (events are deleted post-ack).
- ScoutTrace does **not** support non-stdio MCP transports in MVP (HTTP/SSE/WebSocket MCP transports land in v0.3).

---

## 4. Personas

### P1. Solo Developer ("Dev Dana")
Uses Claude Desktop with 4–6 MCP servers (filesystem, GitHub, Postgres, Slack). Wants to debug "why did the agent call `delete_file` instead of `read_file`?" without writing any code. Cares about: zero setup, no leaked secrets, can pipe to a local file or her team's webhook.

### P2. Platform Engineer ("Platform Pat")
Owns agent infra at a 200-engineer company. Needs every MCP tool call from every employee's Cursor/Claude Code routed to the company's internal observability sink, with redaction policies enforced centrally. Cares about: config distribution, allowlists, audit trail, no employee opt-out for the destination URL but opt-in for what gets captured.

### P3. Agent Builder ("Builder Bo")
Builds agents using Anthropic SDK + custom tools, deployed to production. Wants the same observability surface in dev (via MCP) and prod (via SDK shim). Cares about: post-MVP SDK integration, OTel correlation IDs, structured logs.

### P4. Privacy-Conscious User ("Privacy Priya")
Uses Claude Desktop with personal MCP servers reading her email and notes. Will only adopt ScoutTrace if she can verify nothing leaves her machine without explicit consent. Cares about: dry-run, default destination = `file://`, source-readable redaction rules.

---

## 5. UX Principles

1. **Inspectable by default.** Every command can show what it *would* do before it does it.
2. **No silent network calls.** First send to any destination requires explicit confirmation (or `--yes`).
3. **One screenful per decision.** The setup wizard never shows more than one question's worth of context at a time.
4. **Reversible.** Every config patch generates a backup; `scouttrace undo` restores it.
5. **Quiet on success.** Streaming proxy mode emits nothing to stderr unless `--verbose`.

---

## 6. Setup Wizard (`scouttrace init`)

### 6.1 Goal
Get a new user from `brew install` to a captured tool call in **under 2 minutes**.

### 6.2 Wizard flow

```mermaid
flowchart TD
    A[scouttrace init] --> B{Detect installed hosts}
    B -->|0 found| C[Prompt for manual host selection]
    B -->|1+ found| D[Show detected hosts<br/>checkboxes pre-selected]
    C --> E[Choose destination]
    D --> E
    E --> F{Destination type?}
    F -->|WebhookScout| G[Prompt for API key, setup token,<br/>or browser auto-provisioning<br/>API URL default: api.webhookscout.com]
    F -->|Custom HTTP| H[Prompt for URL + optional auth header]
    F -->|Local file| I[Prompt for path<br/>default: ~/.scouttrace/events.ndjson]
    F -->|stdout| J[No prompt]
    G --> K[Choose redaction profile]
    H --> K
    I --> K
    J --> K
    K --> L{Profile?}
    L -->|strict default| M[Show capture preview<br/>using a synthetic tool call]
    L -->|standard| M
    L -->|permissive| M
    L -->|custom| N[Open editor with policy YAML]
    N --> M
    M --> O{Approve?}
    O -->|yes| P[Backup host configs<br/>write ~/.scouttrace/config.yaml<br/>patch hosts atomically]
    O -->|no| Q[Exit, no changes]
    P --> R[Run scouttrace doctor<br/>verify each host can launch proxy]
    R --> S[Print success + next steps]
```

### 6.3 Wizard rules

- **Idempotent.** Re-running `init` detects existing config and offers `repair`, `migrate`, or `reconfigure` instead of clobbering.
- **No network calls during wizard** unless the user picks WebhookScout and explicitly chooses either API-key validation, setup-token exchange, or agent auto-provisioning (separate confirmation).
- **Editor invocation** uses `$EDITOR`, falling back to `nano`/`notepad`.
- **All choices are flag-able**, so the wizard is fully scriptable: `scouttrace init --hosts claude-desktop,cursor --destination https://my.sink/in --profile strict --yes`.

---

## 7. CLI Command Taxonomy

All commands accept the global flags listed in §7.13.

### 7.1 `scouttrace init`
Interactive wizard (see §6).

| Flag | Type | Default | Description |
|---|---|---|---|
| `--hosts` | csv | (detected) | Subset of `claude-desktop,claude-code,cursor,windsurf,continue,hermes` to patch. |
| `--destination` | URL or `file://...` or `stdout` | (prompt) | Destination URL. `webhookscout` is shorthand for the default WebhookScout adapter. |
| `--api-key` | string | none | WebhookScout API key for `--destination webhookscout`; immediately stored in OS keychain when available, never written to YAML. |
| `--setup-token` | string | none | Short-lived portal-generated token that exchanges for a scoped API key and default agent. Preferred WebhookScout setup path. |
| `--api-url` | URL | `https://api.webhookscout.com` | Base API URL for WebhookScout-compatible backends. |
| `--agent-name` | string | hostname + host name | Friendly agent/source name to create in WebhookScout. |
| `--auth-header` | string | none | Header sent with each upload for custom HTTP destinations; stored in OS keychain when available. |
| `--profile` | enum | `strict` | `strict`, `standard`, `permissive`, or path to custom policy file. |
| `--yes` / `-y` | bool | false | Non-interactive; accept defaults. |
| `--dry-run` | bool | false | Print intended changes; make none. |

### 7.2 `scouttrace proxy`
The transparent MCP stdio proxy. Invoked by host configs; rarely run directly.

```
scouttrace proxy -- <upstream-cmd> [upstream-args...]
```

| Flag | Type | Default | Description |
|---|---|---|---|
| `--server-name` | string | basename of upstream cmd | Logical name recorded in payloads. |
| `--config` | path | `~/.scouttrace/config.yaml` | Override config path. |
| `--profile` | string | (from config) | Override redaction profile for this server only. |
| `--no-capture` | bool | false | Run as pure passthrough (debug mode). |
| `--fail-open` | bool | true | If proxy can't talk to its sidecar/queue, still pass MCP traffic through. |
| `--fail-closed` | bool | false | Inverse of above; refuses to start if capture pipeline is unhealthy. |
| `--max-arg-bytes` | int | 65536 | Truncate captured args above this size. |
| `--max-result-bytes` | int | 262144 | Truncate captured results above this size. |

### 7.3 `scouttrace run`
Execute a one-off command with proxying enabled. For SDK/post-MVP users.

```
scouttrace run -- python my_agent.py
```

Sets `SCOUTTRACE_ENABLED=1` and (post-MVP) injects HTTP proxy env vars.

### 7.4 `scouttrace status`
Print health: queue depth, last successful upload, redaction profile, configured hosts, destination URL (auth headers redacted).

### 7.5 `scouttrace doctor`
Active diagnostics: launch each configured proxy, verify it can speak MCP `initialize` to its upstream, post a synthetic event to the destination, verify queue write/read.

### 7.6 `scouttrace tail`
Stream events from the local queue (pre- or post-redaction) to stdout. Read-only; does not consume.

| Flag | Default | Description |
|---|---|---|
| `--raw` | false | Show pre-redaction. **Requires `--i-understand-this-shows-secrets`.** |
| `--format` | `pretty` | `pretty`, `json`, `ndjson`. |
| `--filter` | none | jq-like filter expression. |

### 7.7 `scouttrace replay`
Re-emit queued or archived events to a (possibly different) destination.

```
scouttrace replay --since 1h --to https://my.alt.sink/in
```

| Flag | Type | Default | Description |
|---|---|---|---|
| `--since` | duration | none | Re-emit events captured within the given window (e.g. `1h`, `24h`). |
| `--until` | duration | none | Upper bound; pairs with `--since` for ranged replays. |
| `--to` | URL or `file://...` or `stdout` | (config default) | Override destination for this replay. Mutually exclusive with `--destination`. |
| `--destination` | name | (config default) | Use a named destination from `config.yaml`. |
| `--filter` | string | none | jq-like filter; only matching events are replayed. |
| `--dry-run` | bool | false | List events that would be sent, with target, without delivering. |

### 7.8 `scouttrace policy`
Subcommands: `show`, `edit`, `lint`, `test <event.json>`. Validates redaction policy YAML (see §13).

### 7.9 `scouttrace hosts`
Subcommands: `list`, `detect`, `add`, `remove`, `patch`, `unpatch`, `diff`. Manages host config integrations.

### 7.10 `scouttrace config`
Subcommands: `show`, `edit`, `set <key> <value>`, `validate`, `migrate`. Manages `~/.scouttrace/config.yaml`.

### 7.11 `scouttrace queue`
Subcommands: `stats`, `flush`, `purge`, `export`, `import`, `pause`, `resume`.

### 7.12 `scouttrace undo`
Restore the most recent backup of any patched host config. `scouttrace undo --list` shows available backups; `--all` reverts every change ScoutTrace has made.

### 7.13 Global flags

| Flag | Description |
|---|---|
| `--config <path>` | Override config file. |
| `--quiet` / `-q` | Suppress non-error output. |
| `--verbose` / `-v` | Emit operational logs to stderr. |
| `--json` | Machine-readable output for that command. |
| `--no-color` | Disable ANSI colors. |
| `--version` | Print version and exit. |

---

## 8. Configuration File Schema

**Location:** `~/.scouttrace/config.yaml` (override via `$SCOUTTRACE_CONFIG` or `--config`).

```yaml
# ~/.scouttrace/config.yaml
schema_version: 1

# --- Destinations ---
destinations:
  - name: default
    type: webhookscout                  # webhookscout | http | file | stdout | plugin
    api_url: https://api.webhookscout.com
    agent_id: agent_8e0f...              # created by wizard or provided manually
    auth_header_ref: keychain://scouttrace/default # opaque API-key ref, never raw secret
    timeout_ms: 5000
    compression: gzip                   # none | gzip | zstd
    headers:
      X-Source: scouttrace
  - name: backup
    type: file
    path: ~/.scouttrace/events.ndjson
    rotate_mb: 100

# Default destination if a server doesn't override
default_destination: default

# --- Capture & redaction ---
capture:
  profile: strict                       # strict | standard | permissive | custom
  custom_policy_path: null              # used only when profile == custom
  max_arg_bytes: 65536
  max_result_bytes: 262144
  capture_results: true
  capture_args: true
  capture_errors: true

# --- Servers (proxied MCP servers) ---
servers:
  - name: filesystem
    upstream:
      command: npx
      args: ["-y", "@modelcontextprotocol/server-filesystem", "~/code"]
      env: {}
    destination: default                # references destinations[].name
    profile: strict                     # optional override
    enabled: true

  - name: github
    upstream:
      command: docker
      args: ["run", "-i", "--rm", "ghcr.io/github/github-mcp-server"]
      env_passthrough: ["GITHUB_TOKEN"]
    destination: default

# --- Queue ---
queue:
  path: ~/.scouttrace/queue
  engine: sqlite                        # sqlite (MVP) | bolt (post-MVP)
  max_bytes: 524288000                  # 500 MB
  max_age_days: 14
  drop_when_full: oldest                # oldest | newest | block

# --- Delivery ---
delivery:
  max_retries: 8
  initial_backoff_ms: 250
  max_backoff_ms: 60000
  jitter: true
  concurrency: 4
  batch_max_events: 50
  batch_max_bytes: 1048576

# --- Hosts (managed integrations) ---
hosts:
  - id: claude-desktop
    config_path: ~/Library/Application Support/Claude/claude_desktop_config.json
    managed_servers: [filesystem, github]
    last_patched_at: 2026-04-30T12:34:56Z
    backup_path: ~/.scouttrace/backups/claude-desktop/2026-04-30T12-34-56Z.json

# --- Telemetry (about ScoutTrace itself) ---
telemetry:
  anonymous_usage: false                # OFF by default
  crash_reports: false                  # OFF by default
```

### 8.1 Schema rules
- All paths support `~` and `$VAR` expansion.
- `type: webhookscout` is a convenience adapter that posts sanitized tool-call events to the WebhookScout MCP/agent observability API. It is implemented as an ordinary destination plugin, not a privileged code path.
- `type: http` requires `url`; `type: webhookscout` requires `api_url`, `agent_id`, and `auth_header_ref`.
- `auth_header_ref` is **always** an opaque reference. ScoutTrace refuses to load configs that contain raw `Authorization` values in plaintext (warning + abort).
- `schema_version` enables `scouttrace config migrate`.
- Unknown fields are rejected by the validator (`scouttrace config validate`).

---

## 9. Data Processing Architecture

```mermaid
flowchart LR
    Host[MCP Host<br/>e.g. Claude Desktop] -->|stdio JSON-RPC| Proxy[scouttrace proxy]
    Proxy -->|stdio JSON-RPC| Upstream[Upstream MCP server]
    Upstream -->|stdio JSON-RPC| Proxy
    Proxy -->|stdio JSON-RPC| Host

    Proxy -->|raw event| Capture[Capture stage]
    Capture -->|envelope| Redact[Redaction stage]
    Redact -->|sanitized envelope| Queue[(Durable queue<br/>SQLite)]
    Queue --> Dispatcher[Dispatcher]
    Dispatcher -->|HTTP POST batch| Dest1[(Destination)]
    Dispatcher -->|fallback| Dest2[(Destination)]

    Doctor[scouttrace doctor] -.read.-> Queue
    Tail[scouttrace tail] -.read.-> Queue
    Replay[scouttrace replay] -.read.-> Queue
```

### 9.1 Stages

1. **Frame parser.** Reads newline-delimited JSON-RPC messages from host→upstream and upstream→host. Maintains zero-copy pass-through on the wire.
2. **Capture.** Builds a request/response correlation map keyed by JSON-RPC `id`. On `tools/call` request and matching response (or error), produces a `ToolCallEvent` envelope.
3. **Redaction.** Applies the active policy to args and results. Strips/replaces values; never mutates the in-flight wire bytes.
4. **Queue.** Append-only SQLite WAL. Each row holds one envelope and an idempotency key.
5. **Dispatcher.** Reads in batches, POSTs to destination(s), acks on 2xx.

### 9.2 Concurrency model

- **Wire goroutines/threads (2 per server):** read/write loops. Hot path; never block on capture.
- **Capture queue:** lock-free ring buffer (bounded). Dropped events recorded as a counter (visible in `status`).
- **Redaction worker pool:** `min(4, ncpu)` workers.
- **Dispatcher:** single goroutine per destination, with `delivery.concurrency` parallel HTTP requests.

### 9.3 Failure isolation

- A bug in the capture/redaction/queue/dispatcher path **must never** affect the wire path. The wire path catches all panics from capture and increments a `capture_errors_total` counter.

---

## 10. MCP stdio Proxy Behavior

### 10.1 Wire compatibility

- Reads stdin, writes stdout. Each direction is a stream of newline-terminated JSON-RPC 2.0 messages.
- ScoutTrace **does not modify** any field of any message. It does not inject `_meta`, does not rewrite IDs, does not reorder.
- ScoutTrace forwards EOF in either direction within 50ms.
- ScoutTrace forwards SIGTERM/SIGINT to the upstream and waits up to `shutdown_grace_ms` (default 3000) before SIGKILL.

### 10.2 Initialization handshake

The MCP `initialize` exchange must complete before any other message. ScoutTrace records the negotiated protocol version and server capabilities into the first event of each session for debugging, but does not interfere.

### 10.3 Captured event types (MVP)

| JSON-RPC method | Captured? | Notes |
|---|---|---|
| `initialize` | metadata only | Records capabilities, server name, version. |
| `tools/list` | metadata only | Records returned tool schemas hash. |
| `tools/call` (request + response/error) | **full event** | Primary signal. |
| `resources/read`, `resources/list` | metadata only | Counts only in MVP; full capture in v0.4. |
| `prompts/get`, `prompts/list` | metadata only | Counts only. |
| `notifications/*` | counted | Latency/error notifications counted. |
| `ping` | suppressed | No event. |

### 10.4 Crash & restart

- If the upstream server exits non-zero, ScoutTrace writes a `server_crashed` event with exit code, last-25 captured tool names, and (optionally) tail of stderr (size-bounded, redacted).
- If ScoutTrace itself crashes, the host sees a normal stdio EOF; the queue (SQLite WAL) is durable across restarts.

### 10.5 Fail-open vs fail-closed

Default is `--fail-open`: if the queue/dispatcher subsystem is unhealthy, MCP traffic continues uninterrupted; events are dropped to a counter. Platform engineers can flip to `--fail-closed` for environments where missing audit data is unacceptable.

---

## 11. Optional HTTP Proxy / SDK Future (post-MVP)

### 11.1 HTTP proxy mode

`scouttrace run -- python agent.py` injects:

```
HTTPS_PROXY=http://127.0.0.1:<port>
SCOUTTRACE_CA_BUNDLE=~/.scouttrace/ca.pem
```

ScoutTrace launches a local MITM proxy with a per-machine CA, scoped only to allowlisted hosts (`api.anthropic.com`, `api.openai.com`, etc.). It parses outgoing tool definitions and incoming tool-use blocks, generating the same `ToolCallEvent` schema as the MCP proxy.

**Risks:** CA installation is intrusive. This mode is **explicitly opt-in** and gated behind `scouttrace http-proxy enable` with a multi-line confirmation.

### 11.2 Native SDK shim

Lightweight wrappers:

```python
from scouttrace.sdk import patch_anthropic
patch_anthropic()  # auto-captures tool_use/tool_result blocks
```

```ts
import { withScoutTrace } from "@scouttrace/sdk";
const client = withScoutTrace(new Anthropic());
```

Shims emit envelopes via the same local Unix socket the MCP proxy uses, so a single ScoutTrace queue/dispatcher serves all sources.

---

## 12. Payload Schema (`ToolCallEvent`)

All events are JSON. Top-level wire format (sent to destination):

```json
{
  "schema": "scouttrace.toolcall.v1",
  "events": [ /* one or more ToolCallEvent objects */ ]
}
```

### 12.1 `ToolCallEvent`

```json
{
  "id": "01JX2K6F8E3N7Z9C4QH1TBV3WD",          // ULID, idempotency key
  "schema": "scouttrace.toolcall.v1",
  "captured_at": "2026-04-30T18:22:31.412Z",   // RFC3339, ms precision
  "session_id": "01JX2K5...",                  // per upstream-server-process
  "trace_id": "0af7651916cd43dd8448eb211c80319c", // OTel-compatible (optional)
  "span_id": "b7ad6b7169203331",
  "source": {
    "kind": "mcp_stdio",                       // mcp_stdio | http_proxy | sdk_shim
    "host": "claude-desktop",                  // best-effort, may be "unknown"
    "host_version": "0.7.11",
    "scouttrace_version": "0.1.0"
  },
  "server": {
    "name": "filesystem",
    "command_hash": "sha256:5f...e1",          // hash, not raw cmd
    "protocol_version": "2025-06-18",
    "capabilities": ["tools", "resources"]
  },
  "tool": {
    "name": "read_file",
    "schema_hash": "sha256:9c...11"
  },
  "request": {
    "json_rpc_id": "42",
    "args": { /* redacted */ },
    "args_truncated": false,
    "args_bytes_original": 312
  },
  "response": {
    "ok": true,
    "result": { /* redacted */ },
    "result_truncated": false,
    "result_bytes_original": 1840,
    "error": null                              // {code, message} when ok=false
  },
  "timing": {
    "started_at": "2026-04-30T18:22:31.402Z",
    "ended_at":   "2026-04-30T18:22:31.412Z",
    "latency_ms": 10
  },
  "redaction": {
    "policy_name": "strict",
    "policy_hash": "sha256:1a...77",
    "fields_redacted": ["request.args.path"],
    "rules_applied": ["path_normalize", "max_bytes"]
  }
}
```

### 12.2 Stability guarantees

- `schema` field is **append-only** across minor versions. Removing or retyping a field is a major-version bump.
- Receivers can rely on `id` for dedup across retries.
- All timestamps UTC. All durations in ms.

---

## 13. Redaction & Capture Policies

Policies are YAML, loaded at startup and on `SIGHUP`. Built-in profiles ship in the binary; custom policies are paths.

### 13.1 Built-in profile: `strict` (default)

```yaml
name: strict
version: 1

# Order: applied top-to-bottom; later rules see earlier rules' output.
rules:
  # 1. Drop oversized payloads first (limits match capture.max_*_bytes in §8)
  - id: max_arg_bytes
    type: truncate
    fields: ["request.args"]
    limit_bytes: 65536
    placeholder: "[truncated:${original_bytes}b]"
  - id: max_result_bytes
    type: truncate
    fields: ["response.result"]
    limit_bytes: 262144
    placeholder: "[truncated:${original_bytes}b]"

  # 2. Strip well-known secret-looking strings
  - id: secret_patterns
    type: redact_pattern
    patterns:
      - name: aws_access_key
        regex: "AKIA[0-9A-Z]{16}"
      - name: bearer_token
        regex: "(?i)bearer\\s+[a-z0-9._-]{20,}"
      - name: github_pat
        regex: "ghp_[A-Za-z0-9]{36}"
      - name: anthropic_key
        regex: "sk-ant-[A-Za-z0-9_-]{20,}"
    placeholder: "[redacted:${pattern_name}]"

  # 3. PII heuristics
  - id: pii
    type: redact_pattern
    patterns:
      - name: email
        regex: "[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\\.[a-zA-Z]{2,}"
      - name: us_ssn
        regex: "\\b\\d{3}-\\d{2}-\\d{4}\\b"
      - name: credit_card
        regex: "\\b(?:\\d[ -]*?){13,16}\\b"
    placeholder: "[redacted:${pattern_name}]"

  # 4. Path normalization (don't leak home dirs)
  - id: path_normalize
    type: transform
    fields_match_regex: "(?i)(path|file|dir|cwd)$"
    replace:
      - from: "${HOME}"
        to: "~"

  # 5. Drop fields entirely
  - id: drop_fields
    type: drop
    field_paths:
      - "request.args.password"
      - "request.args.api_key"
      - "request.args.secret"
      - "request.args.token"
      - "response.result.raw_html"
```

### 13.2 Built-in profile: `standard`

Same as `strict` but disables the `pii` rule and raises both truncation limits to 256 KB (`max_arg_bytes`) and 1 MB (`max_result_bytes`).

### 13.3 Built-in profile: `permissive`

Only `secret_patterns` and the truncation rules (1 MB args, 4 MB results). Useful for local-only debugging.

### 13.4 Capture policy (separate from redaction)

Applied **before** capture, controls what is even read into ScoutTrace's memory:

```yaml
capture:
  servers:
    - name_glob: "*"
      capture_args: true
      capture_results: true
    - name_glob: "secret-*"          # any server starting with "secret-"
      capture_args: false            # never read args
      capture_results: false
  tools:
    - name_glob: "*password*"
      capture_args: false
```

A field that is never *captured* cannot be leaked even if redaction has a bug. This is the privacy backstop.

### 13.5 Policy testing

```
scouttrace policy test ./fixtures/event.json --profile strict
```
Prints the before/after diff and exit-codes non-zero if any "must-redact" assertion fails. Suitable for CI.

---

## 14. Destination Plugin Model

### 14.1 First-class destination types

| Type | Behavior |
|---|---|
| `http` | POST batch JSON to `url`. 2xx = ack. 4xx = ack with warning (non-retriable). 5xx/network = retry. |
| `file` | Append NDJSON to file. Always-ack. Rotates by size. |
| `stdout` | Print NDJSON to stdout. Always-ack. |
| `plugin` | Spawns a subprocess implementing the plugin protocol (§14.3). |

### 14.2 HTTP destination contract

- **Method:** `POST`
- **Content-Type:** `application/json`
- **Content-Encoding:** `gzip` (configurable)
- **Body:** `{"schema": "scouttrace.toolcall.v1", "events": [...]}`
- **Idempotency:** `Idempotency-Key: <batch-ulid>` header. Receivers SHOULD dedupe events by `event.id` over a 24h window.
- **Auth:** any header configured via `auth_header_ref`.

### 14.3 Plugin protocol (post-MVP)

```mermaid
sequenceDiagram
    participant ST as ScoutTrace
    participant PL as Plugin (subprocess)
    ST->>PL: spawn with stdin/stdout pipes
    ST->>PL: HELLO {schema: "scouttrace.plugin.v1"}
    PL->>ST: HELLO_ACK {plugin: "s3", version: "0.1"}
    loop per batch
        ST->>PL: BATCH {events: [...]}
        PL->>ST: ACK {batch_id} | NACK {batch_id, retriable: bool, reason}
    end
    ST->>PL: SHUTDOWN
```

Plugins are arbitrary executables. Discovery: `scouttrace plugin install <path|url>`. Plugins are **not** sandboxed in MVP — the install command shows a clear warning.

---

## 15. Local Durable Queue

### 15.1 Engine

SQLite (WAL mode) for MVP. Single file at `~/.scouttrace/queue/events.db`.

```sql
CREATE TABLE events (
  id            TEXT PRIMARY KEY,        -- ULID
  destination   TEXT NOT NULL,
  enqueued_at   INTEGER NOT NULL,        -- unix ms
  next_attempt  INTEGER NOT NULL,
  attempts      INTEGER NOT NULL DEFAULT 0,
  payload       BLOB NOT NULL,           -- gzipped JSON
  status        TEXT NOT NULL            -- pending | inflight | dead
);
CREATE INDEX idx_pending ON events(destination, status, next_attempt);
```

### 15.2 Reasoning

- SQLite gives us atomicity and crash safety with zero ops burden.
- WAL mode permits concurrent readers (`tail`, `status`) while a writer is active.
- Single-file queue keeps `purge`/`export`/`backup` trivial.

### 15.3 Limits

- Max queue size enforced by `queue.max_bytes`. Eviction strategy per `queue.drop_when_full`.
- Events older than `queue.max_age_days` are moved to `dead` and pruned.

---

## 16. Retry & Backpressure

### 16.1 Retry policy

Exponential backoff with jitter. Configured by `delivery.*`. Pseudocode:

```
delay = min(initial_backoff * 2^attempts, max_backoff)
if jitter: delay = random_between(delay/2, delay)
```

- 4xx (except 408, 425, 429): no retry; mark `dead` with reason; emit local warning.
- 5xx, network errors, 408/425/429 (honoring `Retry-After`): retry up to `max_retries`, then `dead`.

### 16.2 Backpressure

- Queue full → behavior per `queue.drop_when_full`. Default `oldest`: oldest pending events evicted to a `dead_overflow` table (kept until aged out).
- Dispatcher applies a token bucket per destination (default 100 req/s).
- Capture ring buffer full → drop new events, increment `capture_dropped_total`. (Wire path is never blocked.)

### 16.3 Observability of the pipeline itself

`scouttrace status --json` exposes:

```json
{
  "queue": {"pending": 12, "inflight": 2, "dead": 0, "bytes": 184320},
  "dispatcher": {"last_success_at": "...", "last_error": null, "in_flight": 1},
  "capture": {"events_total": 4218, "dropped_total": 0, "errors_total": 0}
}
```

---

## 17. Security & Threat Model

### 17.1 Assets to protect
- User secrets present in tool args/results.
- Destination credentials (auth headers).
- The host application's config files.
- The ScoutTrace binary's integrity.

### 17.2 Adversary model
- **A1. Curious developer with shell access.** Can read `~/.scouttrace/`. We protect via OS keychain for secrets; mode 0600 on config; no plaintext auth in YAML.
- **A2. Malicious upstream MCP server.** A rogue MCP server cannot escalate via ScoutTrace because the proxy only forwards stdio bytes; there is no eval, no injection point. ScoutTrace caps memory per server.
- **A3. Malicious destination URL.** A typo could send all tool calls to attacker.com. Mitigated by: explicit confirmation on first send to a new host; printed destination in `status`; allowlist mode where new destinations require `scouttrace destination approve`.
- **A4. Malicious plugin.** Plugins are full-trust subprocesses. Install flow shows explicit "this is arbitrary code" warning; signed-plugin support is post-MVP.
- **A5. Tampering with proxy binary.** Out of scope for ScoutTrace itself; rely on package manager signatures (Homebrew, npm).

### 17.3 Defense in depth
- **Capture-level deny.** Configurable per server/tool. Field never read.
- **Redact-level strip.** Built-in patterns + custom rules.
- **Egress allowlist.** `destinations.allowlist_hosts` config (post-MVP) refuses to send to unapproved hostnames.
- **Audit log.** `~/.scouttrace/audit.log` records every config change, destination change, and policy change with timestamp and process info. Append-only; rotated at 10 MB.

### 17.4 What ScoutTrace will NOT do
- Will not phone home to ScoutTrace/WebhookScout for telemetry by default.
- Will not auto-update.
- Will not store secrets in plaintext anywhere.
- Will not write outside `~/.scouttrace/`, the patched host config files, and the configured destinations (file destinations limited to user-writable paths).

---

## 18. Open-Source Trust Model

ScoutTrace's value proposition depends on users trusting it with their tool calls. The repo will be:

1. **Apache-2.0 licensed.** Permissive. No CLA.
2. **Reproducibly built.** CI publishes SLSA-provenance attestations for every release.
3. **Single-binary releases.** Static binaries for darwin-arm64, darwin-x64, linux-x64, linux-arm64, windows-x64. SHA256SUMS + minisign signature.
4. **Well-defined "WebhookScout-specific" boundary.** Anything specific to WebhookScout (e.g. a default header, the `webhookscout` destination shorthand) lives in a clearly named package (`destinations/webhookscout/`) so external auditors can verify it carries no privileged behavior.
5. **No license-keyed features.** All features are in the OSS binary. WebhookScout monetizes the destination, not the agent.
6. **Public-by-default issue tracking & roadmap.**
7. **Reproducible policy fixtures.** A corpus of synthetic tool calls + expected redaction outputs lives in `testdata/`, runnable via `scouttrace policy test`.

---

## 19. Telemetry, Preview & Dry-Run

### 19.1 Self-telemetry (about ScoutTrace)
- Default **OFF**.
- When opted in: anonymous version + OS + command counts only. **No tool names, no payloads, no destinations.** Sent once per day to a published, separate endpoint (not WebhookScout). Source code reviewable in `internal/selftelemetry/`.

### 19.2 Preview
`scouttrace preview --server <name>` runs a **synthetic** tool call through the configured capture + redaction + queue path **without delivering**, prints the would-be payload, and shows a colorized diff against the raw call. Used inside the wizard.

### 19.3 Dry-run
Every state-changing command supports `--dry-run`. Specifically:
- `init --dry-run` prints proposed file writes/diffs.
- `hosts patch --dry-run` shows JSON-diff of host config.
- `policy edit --dry-run` validates and prints the resulting effective policy.
- `replay --dry-run` lists which events would be sent and to where.

---

## 20. Host Configuration Detection & Patching

### 20.1 Supported hosts (MVP)

| ID | Detected via | Config path (macOS / Linux / Win) | Format |
|---|---|---|---|
| `claude-desktop` | App bundle exists | `~/Library/Application Support/Claude/claude_desktop_config.json` (mac), `~/.config/Claude/claude_desktop_config.json` (linux), `%APPDATA%\Claude\claude_desktop_config.json` (win) | JSON |
| `claude-code` | `claude` in PATH or `~/.claude/` exists | `~/.claude/settings.json` and `~/.claude/mcp.json` | JSON |
| `cursor` | `cursor` in PATH or app bundle | `~/.cursor/mcp.json` | JSON |
| `windsurf` | App bundle | `~/.codeium/windsurf/mcp_config.json` | JSON |
| `continue` | `~/.continue/` exists | `~/.continue/config.json` | JSON |
| `hermes` | `hermes` in PATH | `~/.hermes/config.toml` | TOML |

`scouttrace hosts list --json` returns the live detection result.

### 20.2 Patch strategy

For each host, ScoutTrace knows the JSON path for MCP servers (e.g. `mcpServers` for Claude Desktop, `mcp.servers` for some Cursor versions). Patching:

1. Read current config bytes.
2. Compute SHA256 → write to `~/.scouttrace/backups/<host>/<iso-timestamp>.json` along with original bytes.
3. Parse JSON (or TOML).
4. For each managed server entry, replace its `command` and `args` so the host invokes:
   ```json
   {
     "command": "scouttrace",
     "args": ["proxy", "--server-name", "<name>", "--", "<original-cmd>", "<original-args...>"]
   }
   ```
5. Annotate the entry with a marker key the host ignores (e.g. `"_scouttrace": {"managed": true, "original": {...}, "version": "0.1.0"}`). For hosts that strip unknown keys, store the marker only in our backup metadata.
6. Serialize with the same indentation/EOL as the original.
7. Write to a temp file in the same directory; `fsync`; atomic `rename` over the original.
8. Verify by re-reading and parsing. On any failure, restore from backup and abort.

### 20.3 Conflict handling

- If a host config has changed since our last patch (checksum mismatch with our recorded `last_patched_at`), `scouttrace hosts patch` refuses unless `--force`. This protects user edits.
- `scouttrace hosts diff <id>` shows current host config vs. our recorded baseline.

### 20.4 Unpatch / undo

- `scouttrace undo` restores the most recent backup for each affected host.
- `scouttrace hosts unpatch <id>` removes only our markers and restores `command`/`args` to original.

```mermaid
flowchart TD
    A[scouttrace hosts patch claude-desktop] --> B{Read config}
    B --> C[SHA256 + save backup]
    C --> D[Parse JSON]
    D --> E[For each managed server:<br/>rewrite command/args]
    E --> F[Write to temp file<br/>fsync]
    F --> G[Atomic rename]
    G --> H{Re-read & parse OK?}
    H -->|yes| I[Update config.yaml<br/>last_patched_at + checksum]
    H -->|no| J[Restore from backup<br/>print error<br/>exit non-zero]
```

---

## 21. Validation

- **Config validation:** `scouttrace config validate` runs JSON-Schema against `config.yaml`. Schema lives at `schemas/config.v1.json`.
- **Policy validation:** `scouttrace policy lint` checks rule references, regex compilability, and field-path syntax.
- **Host config validation:** Before patch, parse the host file with the same parser the host uses (verified against fixtures in `testdata/hosts/`).
- **Wire validation:** Proxy rejects malformed JSON-RPC frames from the host; logs + counter; passes through unmodified to upstream (the upstream may have its own quirks; we are not the protocol police).

---

## 22. UX Flows (sample transcripts)

### 22.1 First-time setup

```
$ scouttrace init

ScoutTrace v0.1.0  •  https://github.com/webhookscout/scouttrace

Detecting installed hosts...
  ✓ Claude Desktop  (5 MCP servers)
  ✓ Cursor          (2 MCP servers)
  ✗ Windsurf        (not installed)

? Patch which hosts?  ◉ Claude Desktop  ◉ Cursor

? Where should captured tool calls go?
  ▸ WebhookScout (default, free dev tier)
    Custom HTTP URL
    Local file (~/.scouttrace/events.ndjson)
    stdout (for piping)

? Connect WebhookScout how?
  ▸ Paste API key now
    Use portal setup token
    Open browser to create/connect agent

? WebhookScout API key?  whs_...  (stored in OS keychain; never written to YAML)
? WebhookScout API URL?  https://api.webhookscout.com
? Agent/source name?     Claude Desktop + Cursor on Dana-MacBook

  ✓ Created WebhookScout agent: agent_8e0f...
  ✓ Destination adapter: webhookscout

? Redaction profile?
  ▸ strict      (recommended; redacts secrets + PII, truncates large payloads)
    standard    (redacts secrets only)
    permissive  (1 MB cap, redacts only obvious secrets)
    custom...   (open editor)

Generating preview using a synthetic 'read_file' call:

  - args.path:  "/Users/dana/secrets.txt"
  + args.path:  "~/secrets.txt"          (rule: path_normalize)

  - result:     "AKIAIOSFODNN7EXAMPLE\nhello world"
  + result:     "[redacted:aws_access_key]\nhello world"

? Apply this configuration? [y/N] y

  ✓ Backed up Claude Desktop config → ~/.scouttrace/backups/claude-desktop/2026-04-30T18-22-31Z.json
  ✓ Backed up Cursor config         → ~/.scouttrace/backups/cursor/2026-04-30T18-22-31Z.json
  ✓ Wrote ~/.scouttrace/config.yaml
  ✓ Patched 5 servers in Claude Desktop
  ✓ Patched 2 servers in Cursor
  ✓ Doctor: all 7 servers healthy
  ✓ Test event delivered to WebhookScout

Restart Claude Desktop and Cursor for changes to take effect.
Run `scouttrace tail` to watch tool calls live.
Run `scouttrace undo` to revert at any time.
```

### 22.2 Live tail

```
$ scouttrace tail
18:24:10.213  claude-desktop  filesystem  read_file        ok    11ms   args.path=~/code/README.md
18:24:10.448  claude-desktop  filesystem  list_directory   ok     3ms   args.path=~/code
18:24:11.901  claude-desktop  github      search_issues    ok   412ms   args.q="bug label:p0"
18:24:12.117  cursor          postgres    query           err     8ms   error=relation "users" does not exist
```

### 22.3 Switching destinations

```
$ scouttrace config edit --dry-run
# (opens $EDITOR; user changes destinations[0] from webhookscout to http)

Will change ~/.scouttrace/config.yaml:
  destinations[0]:
    -  type: webhookscout
    -  api_url: https://api.webhookscout.com
    +  type: http
    +  url: https://my.internal.sink/in

⚠  This destination has not been used before.
   On first send, ScoutTrace will require explicit confirmation.

Re-run without --dry-run to apply.
```

For a single-field edit (e.g. just rotating an `http` destination's URL), the
two-arg form works directly:

```
$ scouttrace config set destinations.default.url https://my.internal.sink/in
```

---

## 23. MVP Milestones

### M0 — Skeleton (week 1)
- Repo scaffolding, CI, release pipeline (signed binaries).
- `scouttrace --version`, `scouttrace doctor` (no-op).
- Config file loader + schema validator.

### M1 — Wire proxy (weeks 2–3)
- `scouttrace proxy` with byte-perfect stdio passthrough.
- JSON-RPC frame parser, `tools/call` correlation.
- Conformance test suite using a reference MCP echo server.

### M2 — Capture + queue + file destination (weeks 3–4)
- ToolCallEvent envelope.
- SQLite WAL queue.
- `file` and `stdout` destinations.
- `scouttrace tail`, `scouttrace status`.

### M3 — HTTP destination + retry (week 4)
- `http` destination with batching, gzip, retry.
- Idempotency keys.
- `scouttrace replay`.

### M4 — Redaction (week 5)
- Built-in `strict`/`standard`/`permissive` profiles.
- `scouttrace policy show|lint|test`.
- Capture-level deny.

### M5 — Host integrations + wizard (weeks 5–6)
- Detection + patching for Claude Desktop, Claude Code, Cursor.
- Backup + atomic rename + verify.
- `scouttrace init` interactive wizard.
- `scouttrace undo`.

### M6 — Polish + remaining hosts (week 7)
- Windsurf, Continue, Hermes.
- `preview`, full `--dry-run` coverage.
- Docs site.

### M7 — Beta release (week 8)
- Homebrew + npm package + standalone binaries.
- Acceptance criteria (§24) all green.

### Post-MVP (sequenced, not week-bound)
- v0.2: HTTP proxy mode (opt-in MITM).
- v0.3: HTTP/SSE MCP transport support.
- v0.4: Native SDK shims (Python, TypeScript).
- v0.5: Plugin protocol + first non-HTTP plugins (S3, OTLP).
- v0.6: Trace correlation with OpenTelemetry context propagation.

---

## 24. Acceptance Criteria

A release is shippable when **all** of the following are true. Each must have an automated test or documented manual test.

### Wire correctness
- **AC-W1.** Reference MCP server reports identical `tools/list`, `tools/call`, and `resources/read` results when invoked through `scouttrace proxy` vs. directly. Diff is empty over a 1000-call corpus.
- **AC-W2.** With `--no-capture`, byte-level passthrough verified by hashing stdin/stdout streams through both paths.
- **AC-W3.** Proxy survives a `kill -9` of the upstream and reports a `server_crashed` event.

### Capture & redaction
- **AC-R1.** `policy test` against the standard fixture corpus shows zero secrets pattern matches in any output payload.
- **AC-R2.** Capture-level deny (`capture_args: false`) verified by a test that injects a fake secret in args and asserts it never enters the queue (queue file scanned post-test).
- **AC-R3.** Truncation does not break JSON validity of captured payloads.

### Queue & delivery
- **AC-Q1.** Kill `scouttrace` mid-batch; on restart, the in-flight batch is re-attempted exactly once with the same idempotency key.
- **AC-Q2.** With destination returning 500, retries follow the documented backoff curve within ±15%.
- **AC-Q3.** Queue at `max_bytes` with `drop_when_full: oldest` evicts oldest events; counter incremented; no panic.

### Hosts
- **AC-H1.** Patch + unpatch round-trip leaves Claude Desktop / Cursor / Windsurf / Continue / Claude Code / Hermes config byte-identical (modulo intentional markers).
- **AC-H2.** Patch with simulated mid-write crash (kill between temp write and rename) leaves original config intact.
- **AC-H3.** `scouttrace undo` restores most recent backup and the host launches normally.

### Wizard
- **AC-W4.** `init --yes --hosts none --destination stdout --profile strict` completes non-interactively and exits 0.
- **AC-W5.** `init` aborts cleanly on Ctrl-C without writing anything.

### Security
- **AC-S1.** Loading a config with a plaintext `Authorization` header in `headers:` aborts with a non-zero exit and a clear error.
- **AC-S2.** First send to a new destination host requires `--yes` or interactive confirm.
- **AC-S3.** No outbound network connection is initiated on `scouttrace --help`, `init --dry-run`, `policy test`, or any read-only command.

### Performance
- **AC-P1.** Wire latency overhead (p50/p99) ≤ 0.5 ms / 2 ms over a localhost MCP echo server at 100 calls/s.
- **AC-P2.** Memory footprint of `scouttrace proxy` ≤ 50 MB RSS at 1000 events/min steady state.

### UX
- **AC-U1.** `scouttrace --help` is ≤ 30 lines.
- **AC-U2.** Every command supports `--json`.

---

## 25. Testing Strategy

### 25.1 Layers

| Layer | Tooling | Scope |
|---|---|---|
| Unit | language-native | Frame parser, redaction rules, policy engine, queue ops, host-config patchers. |
| Conformance | `scouttrace conformance` (ships in repo) | Reference MCP echo server + golden corpus of 1000 representative tool calls. Detects any wire-level deviation. |
| Integration | docker-compose | Real upstream servers (filesystem, sqlite, github mock) behind the proxy; assert end-to-end events arrive at a fake HTTP sink. |
| Host-config fixtures | `testdata/hosts/` | Snapshots of each supported host's config at multiple versions. Patcher tested against all. |
| Fault injection | toxiproxy / custom | Destination returns 5xx, slow, drops connection. Queue full. Disk full. |
| Property-based | hypothesis / fast-check | Redaction rules: arbitrary JSON in → assert no rule's pattern matches output. |
| Performance | k6 / locust against echo MCP | AC-P1, AC-P2 enforcement. |
| Manual smoke | checklist in `docs/release-checklist.md` | Each release: run wizard against real Claude Desktop on macOS + Linux + Windows. |

### 25.2 Golden corpus

`testdata/calls/` contains ~1000 anonymized real-shape tool-call request/response pairs (synthesized; no production data). Each release runs:

```
scouttrace conformance --corpus testdata/calls --profile strict
```

Output is diffed against committed expectations. Diffs require explicit review.

### 25.3 Fuzzing

The JSON-RPC frame parser and redaction regex engine are continuously fuzzed in CI (libFuzzer / Atheris equivalents, depending on impl language).

---

## 26. Open Questions

1. **Implementation language.** Go and Rust both viable. Go favored for simpler cross-compilation and easier contribution; Rust favored for memory-footprint guarantees on the wire path. **Recommendation:** Go, with a `wire/` package designed to be portable to Rust later if needed.
2. **MCP host marker keys.** Some hosts strip unknown JSON keys; need to catalog per-host behavior before relying on inline `_scouttrace` markers vs. external metadata.
3. **Auto-provisioning WebhookScout agents.** Whether `init` can spawn a browser to mint a setup token / scoped API key / agent ID without forcing users to paste API keys. Trades off frictionlessness against making WebhookScout feel privileged; the same flow must remain reproducible with a custom HTTP destination.
4. **Plugin signing.** When (not whether) to require signed plugins.
5. **Schema evolution policy.** v1 → v2 envelope migration story for downstream consumers.

---

## 27. Glossary

- **MCP** — Model Context Protocol. Transport-agnostic JSON-RPC protocol for tool/resource exposure to LLM hosts.
- **Host** — The application embedding an LLM that connects to MCP servers (Claude Desktop, Cursor, etc.).
- **Upstream MCP server** — A user-installed MCP server (filesystem, github, etc.) that ScoutTrace wraps.
- **Destination** — Where captured events are sent (HTTP URL, file, plugin).
- **Envelope** — A single `ToolCallEvent` JSON object.
- **Profile** — A named bundle of redaction + capture rules.
- **Patch** — Modification of a host's config file to route MCP servers through ScoutTrace.
