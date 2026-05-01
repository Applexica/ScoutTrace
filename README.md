# ScoutTrace

A local, open-source CLI and MCP proxy that makes LLM tool-call observability trivial.

ScoutTrace transparently wraps MCP servers, captures structured metadata about each tool call (with privacy-first redaction defaults), and forwards events to **any HTTP endpoint you configure**. The default destination is [WebhookScout](https://api.webhookscout.com), but ScoutTrace is destination-agnostic — point it at your own webhook, an internal sink, a local file, or `stdout`.

> **Status:** Pre-alpha. The PRD is finalized; implementation is starting at M0.

## Quick start (planned UX)

```sh
brew install scouttrace
scouttrace init
```

The wizard detects installed hosts (Claude Desktop, Claude Code, Cursor, Windsurf, Continue, Hermes), patches them to route MCP servers through the proxy, and — for the WebhookScout destination — prompts for an API key, a portal setup token, or browser-based auto-provisioning. Any other destination needs only its URL (and optional auth header). You preview what will be captured before any network egress.

## Privacy & trust

- **No network egress without an explicit destination.** `stdout`, `file://...`, or a custom HTTP URL are first-class alternatives to WebhookScout.
- **Redaction on by default.** The `strict` profile strips well-known secret patterns and PII, normalizes paths, and truncates oversized payloads.
- **Capture-level deny.** Fields you don't capture can't leak, even if a redaction rule has a bug.
- **Inspectable everywhere.** Every state-changing command supports `--dry-run`, `--print-config`, and `--diff`. `scouttrace undo` reverts host patches from on-disk backups.
- **No hidden phone-home.** Self-telemetry is off by default; auto-update is never performed.

See [§17 Security & Threat Model](./docs/PRD.md#17-security--threat-model) and [§13 Redaction & Capture Policies](./docs/PRD.md#13-redaction--capture-policies) for details.

## Documentation

- [**Product Requirements Document**](./docs/PRD.md) — full spec: CLI taxonomy, config schema, payload schema, redaction policies, host-config patching, security model, MVP milestones, acceptance criteria, and testing strategy.

## License

Apache-2.0 (intended).
