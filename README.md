# localbridge

Local MCP gateway that exposes native tools and proxied
downstream tools to ChatGPT through an OpenAI tunnel.

## 1. Overview

A single MCP endpoint combining three capabilities:

- **17 read-only natives** — 8 filesystem (`read_file`,
  `read_multiple_files`, `search_files`, `search_within_files`,
  `list_directory`, `tree`, `get_file_info`,
  `list_allowed_directories`), 4 git (`git_status`, `git_diff`,
  `git_log`, `git_show`), 4 jj (`jj_status`, `jj_diff`, `jj_log`,
  `jj_show`), plus `echo`.
- **Downstream proxy by profile** — external MCP servers (local stdio
  or remote HTTP) selected per profile, namespaced `server__tool`
(e.g. `filesystem__read_file`), with deny rules and secret masking
on supported textual outputs (`TextContent` text + embedded textual
resources; binary/structured unchanged).
- **Tunnel attach** — the composed server is served over an in-memory
  transport bridged to ChatGPT via the OpenAI tunnel client, so no
  inbound ports are ever opened.

## 2. Architecture

| Layer | Code | Role |
|---|---|---|
| Natives | `internal/tools/*` | 17 tools, shared Hider, no network |
| Config | `internal/config` | YAML parse/validate, profile selection, sanitized child env |
| Sessions + forwarding | `internal/proxy` | downstream sessions (timeouts, typed errors), `__` namespace, deny filter, output masking |
| Composition | `internal/gateway` | staged startup, native registry, rollback, serving |
| Bootstrap | `cmd/main.go` | flags/env, tunnel attach (last) |

Profiles are repeatable `--profile` flags; the gateway composes
(natives + selected downstreams) before attaching the tunnel.
Container: rootless Docker primary model, service runs as UID `0:0`
(mapped to the host operator), read-only root, tmpfs `/tmp`.

## 3. Quickstart

Prereqs: rootless Docker daemon, `npx`, `python3` on the host.

```sh
sh init.sh
```

`init.sh` (never `chmod +x`, no sudo inside) checks the daemon,
prepares `~/.config/localbridge/`, and validates the two secret files:

- `~/.config/localbridge/openai_tunnel_id` (mode `0400`)
- `~/.config/localbridge/openai_api_key` (mode `0400`)

Secrets contract (validate-only): create and fill the files yourself;
localbridge NEVER creates or writes secrets — missing / non-regular /
wrong-owner / wrong-mode files fail closed with a message. Then:

```sh
docker compose up --build
```

The compose service runs
`/opt/mcp/bin/localbridge --config /opt/mcp/config/gateway.yaml --profile live`
with the repo mounted at `/workspace:ro` and `WORKSPACE_ROOT=/workspace`.

## 4. Usage

Connect ChatGPT to the gateway with its developer tunnel connector,
using the tunnel ID stored in
`~/.config/localbridge/openai_tunnel_id` (the connector handles
authentication; localbridge never receives your ChatGPT credentials).

Call tools by name: the 17 natives directly (`read_file`, `git_log`,
`jj_status`, `echo`, ...), downstream tools as `server__tool`, where
`server` matches a `servers:` key in `gateway.yaml`.

Switch profiles by editing the compose service `command:` line
(`--profile live` → another profile name declared in `gateway.yaml`),
then re-run `docker compose up --build`. Downstreams with an empty
`profiles:` array are selected by default (unless `enabled: false`
vetoes); the rest start only when one of their listed profiles
intersects the requested set.

Proxied downstreams may expose mutating tools
(same-container servers are operator-trusted — see §6). To prevent
publishing a tool, add `deny:` rows to its `gateway.yaml` entry:

```yaml
deny:
  - type: exact
    params: {value: write_file}
```

Denied tools never enter the registry, so calling one surfaces the
plain unknown-tool error (`proxy: unknown tool ...`) — never a
`DownstreamError`, since no server exists to blame — before any
transport I/O.

## 5. Configuration

`gateway.yaml` declares downstream servers with `type: local`
(`command:` + optional `environment:`) or `type: remote` (`url:` +
optional `headers:`), each with `profiles:` (empty means selected
by default unless `enabled: false`) and optional `deny:` rows.

Tunnel credentials resolve in Go only (single owner), no aliases:

- Tunnel ID: `OPENAI_TUNNEL_ID_FILE` > `OPENAI_TUNNEL_ID` (required)
- API key: `OPENAI_API_KEY_FILE` > `OPENAI_API_KEY` (required, general
  OpenAI key, also authorizes the tunnel)
- Optional: `OPENAI_TUNNEL_BASE_URL`,
  `OPENAI_TUNNEL_ORGANIZATION_ID`, `OPENAI_TUNNEL_POLL_TIMEOUT`,
  `OPENAI_TUNNEL_EXTRA_HEADERS`

`_FILE` values are read once (trailing CR/LF trimmed, empty /
whitespace-only rejected), then unset so children never inherit paths.
Compose delivers them via file-backed secrets at
`/run/secrets/tunnel_id` and `/run/secrets/api_key`.

## 6. Trust model (summary)

- Same-container downstreams are operator-trusted: masking and env
  sanitation prevent accidental propagation only, never malicious
  same-container credential access.
- Verify-not-chown: ownership mismatches fail closed; no repair path
  exists by design (no chown repair in docs, entrypoint, or init.sh).
- Workspace is mounted read-only; `.secrets` never lives in-repo
  (operator keys stay in `~/.config/localbridge/`).
- `safe.directory` covers exactly `/workspace` — git matches exact
  paths only, so subdirectories need their own entries.
