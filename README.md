# localbridge

Local MCP gateway that exposes native tools and proxied
downstream tools to ChatGPT through an OpenAI tunnel.

## 1. Overview

A single MCP endpoint combining three capabilities:

- **23 natives** — 17 read-only (8 filesystem (`read_file`,
  `read_multiple_files`, `search_files`, `search_within_files`,
  `list_directory`, `tree`, `get_file_info`,
  `list_allowed_directories`), 4 git (`git_status`, `git_diff`,
  `git_log`, `git_show`), 4 jj (`jj_status`, `jj_diff`, `jj_log`,
  `jj_show`), plus `echo`) + 6 admin (`admin_list_servers`,
  `admin_get_server`, `admin_get_server_details`,
  `admin_check_installer`, `admin_set_server_enabled`,
  `admin_upsert_server`; restart-loaded config mutation, see §5).
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
| Natives | `internal/tools/*` | 23 tools (17 read-only + 6 admin), shared Hider, no network |
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
`/opt/mcp/bin/localbridge --config /opt/mcp/config/gateway.yaml --profile live --state-dir /var/lib/mcp`
with the repo mounted at `/workspace:ro` and `WORKSPACE_ROOT=/workspace`.

## 4. Usage

Connect ChatGPT to the gateway with its developer tunnel connector,
using the tunnel ID stored in
`~/.config/localbridge/openai_tunnel_id` (the connector handles
authentication; localbridge never receives your ChatGPT credentials).

Call tools by name: the 23 natives directly (`read_file`, `git_log`,
`jj_status`, `echo`, `admin_list_servers`, ...), downstream tools as `server__tool`, where
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

### Admin tools (restart-loaded)

Five natives inspect or mutate `gateway.yaml` without hand-editing:
`admin_list_servers`, `admin_get_server`, `admin_get_server_details`,
`admin_set_server_enabled`, `admin_upsert_server` (no delete path —
server removal is operator volume reset only). A sixth,
`admin_check_installer`, only probes host toolchain availability and
never touches `gateway.yaml` (see below).

- Restart-required: config-mutating changes take effect on restart
  only (install + enable/disable uniformly restart-loaded). No
  runtime session/proxy mutation. After editing via admin tools,
  restart the deployment: container restart or recreation
  (`docker compose restart localbridge` or
  `docker compose up -d --force-recreate`)
  or a host process restart for non-container runs; the next startup
  composes the new set. Rebuilding the image without
  recreating/restarting does not apply edits.
- Atomic + validated: write path loads the current file, mutates the
  struct, runs `config.Validate` on the whole config, then atomic
  tmp+rename in the same dir, mode `0600`, fsync before rename.
  Validation failure leaves original bytes intact.
- Redaction: `admin_list_servers` never emits `environment`/`headers`;
  `admin_get_server` returns exactly `name`, `enabled`, `profiles`;
  `admin_get_server_details` returns keys + value lengths only (values
  never emitted). Instance config must not hold secrets.
- Secret guard (fail closed): `environment`/`headers` keys matching
  case-insensitive `OPENAI_`, `KEY`, `SECRET`, `TOKEN`, `AUTH`,
  `BEARER`, `COOKIE`, `CREDENTIAL`, `PASSWORD` as substrings are
  refused with zero bytes written.
- Read-only bootstrap: with `--config -` (stdin) or empty
  (natives-only), mutating admin tools hard-error naming
  unavailability; list/get serve the startup snapshot.
- Installer check: `admin_check_installer` reports per-manager
  `{found, path, version|error}` for `npm`/`uv`/`cargo`/`go`
  (frozen `go version`, `cargo --version`, `npm --version`,
  `uv --version`; raw first line, never installs). Absent managers
  report `found:false` with reason instead of failing the call.

### Host runs (no container)

- Workspace: `--path /srv/repo` serves that tree (must exist);
  default is `--path` > `WORKSPACE_ROOT` env > current directory.
  Unknown positional arguments fail closed. `GIT_ROOT`/`JJ_ROOT`
  still fall back to the workspace root automatically.
- State dir: `--state-dir` flag > `LOCALBRIDGE_STATE_DIR` env >
  auto (`/var/lib/mcp` when writable, else
  `$HOME/.local/share/localbridge`; both unwritable is a hard
  error). Auto prefers `/var/lib/mcp` whenever it is writable —
  even for non-root users with access there — so a bare host run
  lands under `$HOME` only when the system dir is out of reach.
- Toolchain: missing package managers skip with reason (optional
  servers) or abort loudly (required) — never raw exec errors.
  `RUSTUP_HOME` pins `/opt/mcp/rustup` in-container and stays out
  of the way on hosts (your `rustup` toolchain resolves via `HOME`).
  Probe yours with `admin_check_installer`.
- Config + credentials: same contract as §5 — `--config` file,
  `-` for stdin, or empty for natives-only; tunnel credentials via
  `OPENAI_TUNNEL_ID(_FILE)` / `OPENAI_API_KEY(_FILE)` as above.

### Local stdio test mode

`localbridge --test` serves the composed gateway over stdio as a
plain MCP server — no tunnel, no credentials:

```sh
localbridge --test --path /srv/repo --config /opt/mcp/config/gateway.yaml
```

`--config`, `--state-dir`, `--path`, and `--profile` compose exactly
as in tunnel mode; `--test` with `--config -` is rejected (stdin
cannot carry both config bytes and MCP frames). Connect any local
MCP client over the process pipes (`initialize` → `tools/list` →
`tools/call`), then close stdin for a clean exit.

Stdout-purity contract: in `--test` mode stdout carries MCP frames
only — logs and diagnostics stay on stderr. The tunnel remains the
production path; `--test` only downgrades local verification
difficulty.

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
