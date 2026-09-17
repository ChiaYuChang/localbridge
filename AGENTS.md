# AGENTS.md

## Architectural Topology & Jurisdictions

- **Repository Tier**: Product MCP server (`github.com/ChiaYuChang/local-mcp`, Go 1.27.0, devenv). Orchestration: AgentPlaybook roles `planner`, `reviewer`, `builder`, `scout`, `verifier` (`core`), `navigator`, `cartographer` (`companion`). Flows: `init`, `plan`, `blueprint`, `build`, `review`, `commit`, `cartography`, `session-handoff`, `e2e`, `navigator-cartography`. Memory: living `AGENTS.md`.
- **External Interfaces**: MCP over OpenAI Tunnel (`tunnelclient.New`, `client.Start`, `WaitUntilReady`); in-memory transports (`mcp.NewInMemoryTransports`, `server.Run`); tools `echo`, `read_file`; env `CONTROL_PLANE_TUNNEL_ID`, `CONTROL_PLANE_API_KEY`/`OPENAI_API_KEY`, `CONTROL_PLANE_BASE_URL`, `CONTROL_PLANE_ORGANIZATION_ID`, `CONTROL_PLANE_POLL_TIMEOUT`, `CONTROL_PLANE_EXTRA_HEADERS`, `TUNNEL_CLIENT_SDK_READY_FILE`.
- **Module Map**: Entry `cmd/main.go` (single entrypoint: inline echo + tool wiring + tests in `cmd/main_test.go`); contract `internal/tools/tools.go` (`Tool{Name,Register}`); impl `internal/tools/test/echo.go` (`ToolEcho`); sandbox `internal/tools/filesystem/filesystem.go` (`os.OpenRoot`, `FileSystem{root *os.Root}`); readers `internal/tools/filesystem/file.go`; metadata `directory.go`/`allowed.go`; search `search.go`; hider wiring `hider.go`; git `internal/tools/git/` (`status/diff/log/show` via `os/exec`); jj `internal/tools/jj/` (`status/diff/log/show` via `os/exec` + fake fixture); secrets `internal/tools/secrets/secrets.go` (unified `Secret`/`SecretHider`, `Enable` Extra-only, `Lead`, `Compile`); zero empty stubs post path.go deletion; empty `tests/`. Retired: root `main.go` (broken `internal/tools/echo` import, superseded).
- **Artifact Governance**: Vault `~/.agentplaybook/<project>/plan`, `~/.agentplaybook/<project>/e2e`; docs `blueprint-plan`, `sub-build-plan`, `sub-review-plan`, `sub-review-resolution`, `review-resolution`, `diagram-brief`, `diagram-completion`, `diagram-clarification-request`, `e2e-brief`, `e2e-report`, `e2e-test-spec`, `e2e-clarification-request`.
- **Blind Barrier, Scout Isolation & Companion Allowlist**: `review-findings` restricted to `["planner","reviewer"]`; Builder receives Planner-sanitized remediation only. Scout excluded from in-flight plans/findings. Navigator/Cartographer visibility limited to Settled-Artifact Allowlist (`agents-md`, `review-resolution`, `sub-review-resolution`) plus authorized diagram messages; in-flight drafts exclude companions. Navigator acts only in `navigator-cartography`; Cartographer owns only `diagram-completion`, `diagram-clarification-request`, acts only in `cartography`, `navigator-cartography`.
- **Navigator Governance**: Star topology (`user`,`planner`,`cartographer` only); fixed handoff `Please send this requirement directly to Planner` on change intent; queries zero side-effect, zero Planner response obligation; source-restricted responses with `[Source: <path> | Observed: <rev> @ <timestamp>]`; gated to `idle`/`done`, max 1 in-flight, <500 chars, discard on non-eligible, fallback static artifacts.
- **Cartographer Governance**: Renders `docs/diagrams/<safe-name>.html` only, rejects `..`/leading-slash/external writes; Taste Gate <=12 nodes/transitions else `ADVISORY_ISSUED`; zero context pollution via `diagram-completion` (<100 tokens, <=250 chars, <=60 words, single-sentence digest, zero markup); fire-and-forget, no polling; brief self-sufficiency, anti-exploration; clarification protocol, anti-guessing.
- **Jurisdictional Boundaries**:
  - `AgentPlaybook`: Conceptual evidence-based governance. VCS-neutral; zero raw shell/command syntax in catalog data.
  - VCS Mechanism: Headless guards, workspace management delegated to active VCS skill (Jujutsu/`agentjj` or Git).
  - Policy Overlay: Candidate stabilization, TOCTOU defense, secret scan delegated to active commit policy (`agentcommit`).
  - Product: Builder delivers verified working-copy diffs only, never seals commits; Planner owns VCS history/progression; Verifier runs E2E only in out-of-tree sandbox.

## Global Operational Invariants

- **Peer-Session Primacy**: Reviewer/Builder/Scout/Verifier/Cartographer operate as dedicated peer sessions via harness transport (e.g. herdr). Planner MUST NEVER spawn nested subagents to simulate gates. Route dispatches to peer panes.
- **Dual Gates & E2E Lifecycle**: `PLAN_PASS` before implementation; `IMPLEMENTATION_REVIEW_PASS` pre-E2E; frozen `candidate_ref` for `E2E_REQUIRED`; only `E2E_EVIDENCE_ADMITTED` permits final `REVIEW_PASS`/`FEATURE_REVIEW_PASS`. No bypass.
- **Non-Interactive Execution**: Headless-safe only. No TUIs, unshielded pagers, prompts in unattended sessions.
- **Living Memory Single-Writer**: Planner sole author/curator of `AGENTS.md`. Others never edit directly.
- **Language Standard & Telegraphic Style**: Concise en-US ASCII. Drop articles/filler/prose. Non-ASCII requires adjacent inline rationale. Exact symbols/paths mandatory.
- **Inter-Agent Messaging**: Efficiency-first. Compact structured payloads, exact symbols/paths, no pleasantries.
- **Commit & Publication Separation**: Commit auth = local seal only. Remote push requires separate explicit user auth.
- **Fail-Closed Intent Recovery**: On `AUTHORIZATION_DENIED`, return to Step 2 await renewed intent. No autonomous re-draft.
- **Conventional Commits**: `feat`, `fix`, `refactor`, `test`, `docs`, `chore`. Concise header, no plan slug.
- **Seal Cadence**: Seal per completed feature right after its gate; fix forward via new changes (jj makes post-seal fixes cheap). One feature per working copy keeps seals atomic without interactive split.
- **No Background Auto-Update**: User-initiated only. No polling/downloading/mutation without explicit invocation.
- **Coherent Plan Units & Anti-Rubber-Stamp**: One plan one root intent; decompose by coupling/scope/concerns/verification heterogeneity; JIT sub-plans from blueprints; Reviewer executes counterfactual split challenge.
- **Finding Severity**: Exactly `Blocker`, `Major`, `Minor`, `Other`. Blocker blocks `REVIEW_PASS`; Major needs fix or Planner waiver; Minor/Other advisory.
- **Verification Tracks**: Track A local RED/GREEN for behavioral; static/spec evidence for non-behavioral; optional Track B action differentials with pinned `(repository_identity,baseline_identity)`, fail-closed `BASELINE_STALE`.
- **Zero Context Pollution**: Cartographer raw markup isolated; Verifier raw logs confined to sandbox disk; handoffs lightweight only.

## Builder Precautions & Gotchas

- **`os.Root` Confinement**: `FileSystem.New(root)` via `os.OpenRoot`; all opens through `fs.root.Open`; `Close` required; rejects escape; no symlink/mount bypass; no absolute host paths outside root.
- **Package Read Policy**: Every file-content read in `internal/tools/filesystem` uses stdlib `io` only (`io.LimitReader` + `utf8.Valid`); stat size early-gate + TOCTOU recheck, over-limit/UTF-8 hard errors with zero content. Retired 2026-09-14: in-house `LimitedReader`, `read_partial_file`, partial returns (line numbers unknowable upfront; over-limit files treated as non-text).
- **Tool Shape Convention**: Per-tool methods split `check` (open/stat/gate) then `do` (`read`/actions); `handle()` orchestrates only. Shared formatters stay single-call-site.
- **Error Taxonomy & Echo-Input**: Inner code returns bare sentinels (`ErrFileOpen`, `ErrNotAFile`, `ErrFileTooLarge`, `ErrInvalidUTF8`, `ErrFileRead`); only `handle()` formats (`"%q: %w"` via membership-checked `ErrTaxonomy`, unknown normalizes to `ErrFileRead`); messages echo caller-supplied paths verbatim, never absolute paths; zero content on errors.
- **Read Limits**: `read_file` max 4MB (`MaxFileSize`); stat early-gate (`tooLargeError`: cap + non-text verdict) + streaming recheck via check/read methods; UTF-8 enforced; dir rejected; single tool, no ranges, no partials.
- **Empty Stubs**: `filesystem/directory.go`, `path.go`, `search.go` 0 bytes. Do not import unimplemented symbols. Implement or remove only per approved plan. (Retired with path.go deletion: all three implemented or removed; line kept as convention.)
- **Tool Contract**: Input/output structs with `json` tags (`ToolEchoI/O`, `ToolReadFileI/O`); `Name()` unique (`echo`, `read_file`); `Register(*mcp.Server) error` via `mcp.AddTool`; handler returns `(CallToolResult, Output, error)`.
- **Env Parsing**: `CONTROL_PLANE_TUNNEL_ID` required; `APIKey` falls back `CONTROL_PLANE_API_KEY` -> `OPENAI_API_KEY`; `durationFromEnvironment` uses `time.ParseDuration`, must be >0; `headersFromEnvironment` splits `,`/`;`, each `Key: Value`, trims spaces, rejects malformed; `writeReadyFile` mode `0600`, no-op on empty path.
- **Tunnel Lifecycle**: `tunnelclient.New(cfg, clientTransport)` -> `Start(ctx)` -> `WaitUntilReady(ctx)` -> serve until `ctx.Done`/`client.Done`/`serverDone`; `Stop` with 5s timeout deferred. No real control-plane in unit tests; keep tests hermetic with `t.Setenv`.
- **Toolchain**: `devenv.yaml` rolling nixpkgs, `allow_unfree:true`; Go 1.27.0; `.secrets/` gitignored, never copy/mount operator secrets into sandbox; generated creds local-only throwaway.
- **Stateless Replaceability**: Builder disposable; replace bloated sessions from approved plan, no compaction overhead.
- **Handoff**: Deliver minimal diff + green logs; never run VCS seal commands; never edit `AGENTS.md`.
- **MCP Error-Path Drop**: go-sdk v1.7 typed `AddTool` discards handler result/output on non-nil error (`StructuredContent=null`); surface agent guidance via error text, prove structured partial at handle level.

## Reviewer Precautions & Checklist

- **Public-Only Guidance**: `AGENTS.md` holds public operational facts only. Exclude private criteria, hidden fixtures, inspection methods.
- **Contract Falsifiability**: Boundary tests assert observable I/O/errors at `Tool` boundary, must fail on plausible violation; distinct from TDD reproductions.
- **Plan Coverage**: Audit every Build invariant against independent Review verification path; check Track B fields, baseline identity, severity disposition.
- **Product Checks**: `os.Root` escape attempts blocked; size/UTF-8/dir guards hit; exact-cap vs cap+1 boundaries; zero empty stubs post path.go deletion; `0600` ready-file, no secret leak, `.secrets/` untracked.
- **Untested Server Boot**: `run()` tunnel lifecycle (Start/WaitUntilReady vs real control plane) has zero coverage -- no live creds/network path exists, and sandbox hygiene forbids real creds; transport tests cover only the in-memory half. E2E trigger: staging-capable credentials or fake control-plane endpoint.
- **Resolution Hygiene**: Shared resolutions sanitized actionable-only; exclude review-plan criteria/fixtures/methods; keep task findings out of `AGENTS.md`.
- **Scout Confidentiality**: Verify `scout-survey` provenance/evidence/uncertainties; keep Scout read-only, no access to private review artifacts.
- **Language Purity & Barrier Audit**: Audit `AGENTS.md` for secret leaks, unauthorized non-ASCII without inline rationale, fluff; verify no `review-findings` leak to Builder/companions.
- **Post-Commit Compaction**: Self-evaluate context >50% or review clutter, then compact.

## Active State & In-Flight Context

- **Observed-At**: `2026-09-17T22:51:03Z - G2+G3 proxy sealed (feat(proxy) @ ozxrktkx)`
- **Dirty Status**: `dirty (C1 container + path-deletion + AGENTS.md unsealed; first-green repo-wide)`
- **Milestone**: `C1 + PATH-STUB REVIEW_PASS - docker build green, 12/12 proof PASS, 180 tests zero-overlay`
- **Next Pickup Item**: `Commit C1+path-deletion per seal cadence, or draft G4 native-startup sub-plan pair`
- **Commit Ordering Rule**: All `AGENTS.md` content updates land PRE-seal. `Observed-At` carries timestamp + sealed message only, never commit hashes (hashes mutate on squash and live authoritatively in `jj log`); no post-seal squash cycle exists.
- **Ground Truth Revalidation Invariant**: Cold-start Planners MUST run fresh VCS status/log inspection; never trust cached Active State.
