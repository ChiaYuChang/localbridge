#!/bin/sh
# C1 container-proof: ENVELOPE ONLY harness (build correctness, versions,
# layout/perms, history, ro-fs, caches, safe.directory, PID1 reaping,
# external-diff canary). Live serving rows DEFERRED post-G4 (see plan).
#
# Frozen harness: POSIX sh + docker CLI + python3-stdlib only (no jq).
# Every docker assertion step runs under `timeout 120`; the build step
# uses parameterized BUILD_TIMEOUT (default 1800, cold-cache safe).
# Fixtures are MOUNTED from host tmp dirs (never COPYd, never shipped).
# Daemon absent -> exit 3 with reason (fail-closed). Any failure -> exit 1.
# Whole proof needs NO network assertions and NO tunnel creds.
#
# NOTE: docker invocations go through external timeout(1), which cannot
# execute shell functions — so the shared run flags live in $DFLAGS
# (intentionally word-split; all paths are space-free mktemp outputs).
# Compose rows use up -d + wait + logs (never `run --rm`: cleanup spin).
set -eu

IMAGE="${1:?usage: container-proof.sh <image>}"
BUILD_TIMEOUT="${BUILD_TIMEOUT:-1800}"
# Compose rows pin the proof image (default tag localbridge); --no-build
# on every compose run row (never build implicitly).
LOCAL_MCP_IMAGE="${LOCAL_MCP_IMAGE:-$IMAGE}"
export LOCAL_MCP_IMAGE

fail() { echo "FAIL: $1"; exit 1; }

# Daemon gate (exit 3, fail-closed).
docker info >/dev/null 2>&1 || { echo "REASON: docker daemon absent"; exit 3; }
command -v python3 >/dev/null 2>&1 || fail "python3 required"
command -v git >/dev/null 2>&1 || fail "host git required for fixtures"
command -v jj >/dev/null 2>&1 || fail "host jj required for fixtures"
[ "$(id -u)" != "10001" ] || fail "host UID 10001 makes ownership proof vacuous"

# Fixture tree (host tmp, mounted ro / rw as noted).
T=$(mktemp -d)
WS="$T/ws"
mkdir -p "$WS"
V_CACHE="c1proof-cache-$$"
REAP_C="c1proof-reap-$$"
CANARY_C="c1proof-canary-$$"
CPROJ="c2proof$$"
C2OVERRIDE=""
cleanup() {
	code=$?
	# Diagnostics survive failure (kept dir echoed); success cleans tmp.
	if [ "$code" -ne 0 ]; then
		echo "diagnostics kept in $T"
	else
		rm -rf "$T"
	fi
	docker rm -f "$REAP_C" "$CANARY_C" >/dev/null 2>&1 || true
	if [ -n "$C2OVERRIDE" ]; then
		docker compose -p "$CPROJ" -f docker-compose.yml -f "$C2OVERRIDE" rm -sf localbridge >/dev/null 2>&1 || true
	fi
	docker volume rm -f "$V_CACHE" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Shared run flags (runtime contract): read-only root, tmpfs /tmp,
# ro workspace, installer cache volume. Unquoted on purpose (see NOTE).
# shellcheck disable=SC2086: DFLAGS word-split intentional, space-free paths
DFLAGS="--rm --read-only --tmpfs /tmp -v $WS:/workspace:ro -v $V_CACHE:/var/lib/mcp/cache"

# ---- fixture repos (host UID ownership, != 10001 by gate above) ----
# NOTE: the git repo lives AT /workspace itself: safe.directory matches
# exact paths only (verified: a bare /workspace entry does NOT cover
# /workspace/subdir), and the trust line is frozen to exactly that value.
# Fixtures build under HOME=<tmpdir> so operator dotfiles (git/jj
# identity, templates) can never leak into fixture repos.
mkdir -p "$WS"
OLDHOME=$HOME
export HOME="$T/home"
mkdir -p "$HOME"
git -C "$WS" init -q
git -C "$WS" -c user.email=t@t.t -c user.name=t -c commit.gpgsign=false commit -q --allow-empty -m init
printf 'v1\n' > "$WS/file.txt"
git -C "$WS" -c user.email=t@t.t -c user.name=t -c commit.gpgsign=false add file.txt
git -C "$WS" -c user.email=t@t.t -c user.name=t -c commit.gpgsign=false commit -qm one
printf 'v2-dirty\n' > "$WS/file.txt"

jj git init "$WS/jjrepo" >/dev/null 2>&1
jj -R "$WS/jjrepo" config set --repo user.name t >/dev/null 2>&1 || true
jj -R "$WS/jjrepo" config set --repo user.email t@t.t >/dev/null 2>&1 || true
printf 'alpha\n' > "$WS/jjrepo/a.txt"
jj -R "$WS/jjrepo" describe -m fixture >/dev/null 2>&1
printf 'alpha\nmore\n' > "$WS/jjrepo/a.txt"
jj -R "$WS/jjrepo" st >/dev/null 2>&1
export HOME="$OLDHOME"

# World-readable fixtures (host UID ownership retained for the
# ownership proof; container UID 10001 only needs read traversal).
chmod -R a+rX "$WS"

# External-diff canary: writes a marker to /tmp (tmpfs) if ever executed
# as a driver. /workspace stays ro — the marker must never land there.
CANARY="$WS/canary.sh"
printf '#!/bin/sh\necho RAN >> /tmp/canary.ran\nexit 0\n' > "$CANARY"
chmod +x "$CANARY"

# ---- 1. build ----
echo "== build"
timeout "$BUILD_TIMEOUT" docker build -t "$IMAGE" -f Dockerfile . || fail "docker build"

# ---- 1b. build-test stage (deferred live rows live here, not in a
# serve mode): gateway composition, natives, git/jj integration,
# sanitized child env, proxy child start/stop — proven at build time.
# Never credited with mount/PID1/stop semantics (runtime rows below)
# and vice versa.
echo "== build-test-stage"
timeout "$BUILD_TIMEOUT" docker build --target test -t localbridge:test -f Dockerfile . || fail "test stage"

# ---- 2. Volumes null (image stays plain) ----
echo "== volumes-null"
VOLUMES=$(docker inspect "$IMAGE" -f '{{json .Config.Volumes}}')
[ "$VOLUMES" = "null" ] || fail "Volumes not null: $VOLUMES"

# ---- 3. history sentinel scan (no secrets/ARG-ENV leaks, no secrets-COPY) ----
echo "== history-scan"
docker history --no-trunc "$IMAGE" > "$T/history.txt"
python3 - "$T/history.txt" <<'EOF' || fail "history-scan"
import sys
text = open(sys.argv[1]).read()
for marker in ["CONTROL_PLANE_API_KEY", "OPENAI_API_KEY", "OPENAI_TUNNEL_ID", "/root/.ssh", "id_rsa", "id_ed25519"]:
    if marker in text:
        print("leaked marker in history: " + marker)
        sys.exit(1)
print("history clean")
EOF

# ---- 4. ro-workspace (OS-level unwritable) ----
echo "== ro-workspace"
if timeout 120 docker run $DFLAGS "$IMAGE" touch /workspace/nope 2>/dev/null; then
	fail "workspace writable"
fi

# ---- 5. binary smoke (executes; serving needs creds — out of scope) ----
echo "== binary-smoke"
SMOKE_OUT="$T/smoke.txt"
set +e
timeout 120 docker run $DFLAGS -e WORKSPACE_ROOT=/workspace "$IMAGE" /opt/mcp/bin/localbridge > "$SMOKE_OUT" 2>&1
SMOKE_CODE=$?
set -e
cat "$SMOKE_OUT"
# No-hang proof: timeout kills at 124 — reject explicitly. Expected
# contract: config-error exit 1 complaining about the missing tunnel ID
# (creds never supplied; serving out of scope).
[ "$SMOKE_CODE" -ne 124 ] || fail "smoke hung (timeout kill)"
[ "$SMOKE_CODE" -eq 1 ] || fail "smoke exit $SMOKE_CODE, want config-error 1"
grep -q "OPENAI_TUNNEL_ID" "$SMOKE_OUT" || fail "smoke output missing config complaint"

# ---- 6. parsed versions (git >= 2.41, jj == 0.41.0-prefix exact) ----
echo "== versions"
timeout 120 docker run $DFLAGS "$IMAGE" git --version > "$T/gitv.txt" || fail "git version"
timeout 120 docker run $DFLAGS "$IMAGE" jj --version > "$T/jjv.txt" || fail "jj version"
cat "$T/gitv.txt" "$T/jjv.txt"
python3 - "$T/gitv.txt" "$T/jjv.txt" <<'EOF' || fail "versions"
import sys, re
gitv = open(sys.argv[1]).read().strip()
jjv = open(sys.argv[2]).read().strip()
m = re.search(r"(\d+)\.(\d+)", gitv)
assert m, "unparseable git version: " + gitv
assert (int(m.group(1)), int(m.group(2))) >= (2, 41), "git too old: " + gitv
assert jjv == "jj 0.41.0" or jjv.startswith("jj 0.41.0-"), "jj version mismatch: " + jjv
print("versions ok")
EOF

# ---- 7. exact jj argv + state identity (reads never mutate) ----
echo "== jj-argv-identity"
# Worktree digest (sorted find+sha256sum manifest, .jj excluded):
# byte-identical before/after proves zero mutation.
# NOTE: jj 0.41 requires a writable .jj (secure-config check creates a
# temp file even for pure reads), so the frozen argv runs on a /tmp copy
# of the ro-mounted fixture — the workspace mount itself stays ro. The
# pre-copy host manifest proves the copy faithful; pre/post copy
# manifests prove reads never mutate.
treemanifest() {
	(cd "$1" && find . -type f -not -path './.jj/*' | LC_ALL=C sort | xargs sha256sum) > "$2"
}
treemanifest "$WS/jjrepo" "$T/tree-host.txt"
timeout 120 docker run $DFLAGS "$IMAGE" sh -c '
cp -r /workspace/jjrepo /tmp/jjrw &&
cd /tmp/jjrw &&
echo "@@MANIFEST1" &&
(find . -type f -not -path "./.jj/*" | LC_ALL=C sort | xargs sha256sum) &&
echo "@@STATUS1" &&
jj --no-pager --color=never --ignore-working-copy -R /tmp/jjrw status &&
echo "@@DIFF" &&
jj --no-pager --color=never --ignore-working-copy -R /tmp/jjrw diff --git -- a.txt &&
echo "@@STATUS2" &&
jj --no-pager --color=never --ignore-working-copy -R /tmp/jjrw status &&
echo "@@MANIFEST2" &&
(find . -type f -not -path "./.jj/*" | LC_ALL=C sort | xargs sha256sum)' > "$T/jjout.txt" || fail "jj argv run"
python3 - "$T/jjout.txt" "$T" <<'EOF' || fail "jj-argv-identity"
import sys
out, d = open(sys.argv[1]).read(), sys.argv[2]
parts, cur = {}, None
for line in out.splitlines():
    if line.startswith("@@"):
        cur = line[2:]
        parts[cur] = []
    elif cur:
        parts[cur].append(line)
for k in ["MANIFEST1", "STATUS1", "DIFF", "STATUS2", "MANIFEST2"]:
    assert k in parts, "missing section " + k
    open(d + "/" + k.lower() + ".txt", "w").write("\n".join(parts[k]) + "\n")
print("sections split")
EOF
grep -q "a.txt" "$T/diff.txt" || fail "frozen diff argv shows no change"
cmp "$T/status1.txt" "$T/status2.txt" || fail "jj reads mutated repo state"
cmp "$T/tree-host.txt" "$T/manifest1.txt" || fail "fixture copy unfaithful"
cmp "$T/manifest1.txt" "$T/manifest2.txt" || fail "worktree digest changed"

# ---- 8. ownership + safe.directory trust (spot-check, both directions) ----
echo "== ownership-trust"
[ "$(timeout 120 docker run $DFLAGS "$IMAGE" id -u)" = "10001" ] || fail "container UID must be 10001"
[ "$(stat -c %u "$WS/file.txt")" != "10001" ] || fail "fixture UID vacuous"
timeout 120 docker run $DFLAGS "$IMAGE" cat /tmp/.gitconfig > "$T/gitconfig.txt" || fail "gitconfig read"
grep -q "directory = /workspace" "$T/gitconfig.txt" || fail "trust line missing"
[ "$(grep -cv '^\[safe\]\|directory = /workspace\|^$' "$T/gitconfig.txt" || true)" = "0" ] || fail ".gitconfig carries more than the trust line"
# Positive: alien-owned repo works WITH the trust line.
timeout 120 docker run $DFLAGS "$IMAGE" git -C /workspace status >/dev/null || fail "trusted git status failed"
# Negative control: without the entrypoint no trust exists -> must fail
# (proves the trust line load-bearing, non-vacuous).
if timeout 120 docker run --rm --read-only --tmpfs /tmp --entrypoint sh -v "$WS:/workspace:ro" "$IMAGE" -c 'git -C /workspace status' >/dev/null 2>&1; then
	fail "untrusted git status unexpectedly succeeded"
fi

# ---- 8b. workspace + env hardening rows ----
echo "== workspace-secrets-absent"
# No in-repo secrets: the operator dir lives outside the repo, so the
# mounted workspace must not contain a .secrets tree at all.
timeout 120 docker run $DFLAGS "$IMAGE" sh -c 'test ! -e /workspace/.secrets' || fail "/workspace/.secrets present"
echo "== baseline-git"
# Baseline env only (no GIT_CONFIG_* anywhere — empty is not unset for
# git, so assert true absence): git discovers the HOME-default trust
# file implicitly — no overrides needed.
timeout 120 docker run $DFLAGS "$IMAGE" sh -c 'git -C /workspace status >/dev/null && ! env | grep -q "^GIT_CONFIG"' || fail "baseline git"
echo "== cache-uid"
# Cache dirs writable as the fixed UID (explicit, not just implied).
timeout 120 docker run $DFLAGS "$IMAGE" sh -c '[ "$(id -u)" = "10001" ] && touch /var/lib/mcp/cache/npm/u /var/lib/mcp/cache/uv/u /var/lib/mcp/cache/cargo/u' || fail "cache writable as 10001"

# ---- 9. frozen diff-argv canary (external driver must NOT run) ----
# Rationale: external diff drivers execute without a TTY, so the canary
# is non-vacuous (removing --no-ext-diff flips it). core.pager /
# core.editor are TTY-gated instead and explicitly untested.
# The marker lives in /tmp (tmpfs), read back via docker exec; the live
# container keeps /workspace ro throughout.
echo "== diff-canary"
docker run -d --name "$CANARY_C" --read-only --tmpfs /tmp \
	-v "$WS:/workspace:ro" \
	-v "$V_CACHE:/var/lib/mcp/cache" \
	"$IMAGE" sleep 300 >/dev/null
# NOTE: `-C` is the repo pin (git analogue of jj `-R`); the frozen
# portion is the flag sequence `--no-pager diff --no-color --no-ext-diff
# -- <path>` (no-rev layout).
timeout 120 docker exec -e GIT_EXTERNAL_DIFF=/workspace/canary.sh "$CANARY_C" \
	git -C /workspace --no-pager diff --no-color --no-ext-diff -- file.txt > "$T/diffout.txt" || fail "canary run"
[ -s "$T/diffout.txt" ] || fail "frozen diff argv produced no output"
if timeout 120 docker exec "$CANARY_C" test -e /tmp/canary.ran; then
	fail "external driver ran despite --no-ext-diff"
fi
[ ! -e "$WS/canary.ran" ] || fail "marker leaked into ro workspace"
# Negative control: without the flag the driver DOES run (non-vacuous).
timeout 120 docker exec -e GIT_EXTERNAL_DIFF=/workspace/canary.sh "$CANARY_C" \
	git -C /workspace --no-pager diff --no-color -- file.txt >/dev/null || fail "negative control run"
timeout 120 docker exec "$CANARY_C" cat /tmp/canary.ran | grep -q RAN || fail "canary negative control did not run"
docker rm -f "$CANARY_C" >/dev/null

# ---- 10. /tmp + cache persistence across --rm runs (volumes) ----
echo "== tmp-caches"
timeout 120 docker run $DFLAGS "$IMAGE" sh -c 'touch /tmp/ok && touch /var/lib/mcp/cache/npm/m1 /var/lib/mcp/cache/uv/m2' || fail "writable dirs"
timeout 120 docker run $DFLAGS "$IMAGE" sh -c 'test -f /var/lib/mcp/cache/npm/m1 && test -f /var/lib/mcp/cache/uv/m2' || fail "cache persistence"

# ---- 11. reaping on standalone stub tree (no gateway involved) ----
echo "== reaping"
docker run -d --name "$REAP_C" --read-only --tmpfs /tmp -v "$WS:/workspace:ro" "$IMAGE" sh -c 'sh -c "sleep 120" & wait' >/dev/null
sleep 2
ZOMBIES=$(timeout 120 docker exec "$REAP_C" sh -c 'n=0; for d in /proc/[0-9]*; do s=$(cat "$d/stat" 2>/dev/null) || continue; rest=${s##*) }; set -- $rest; [ "$1" = Z ] && n=$((n+1)); done; echo "$n"')
[ "$ZOMBIES" = "0" ] || fail "zombies while running: $ZOMBIES"
timeout 120 docker stop -t 5 "$REAP_C" >/dev/null || fail "stop hung"
[ "$(docker inspect "$REAP_C" -f '{{.State.Running}}')" = "false" ] || fail "residual running container"

# ---- 12. runtime sentinels (never shipped / never present) ----
echo "== sentinels"
timeout 120 docker run $DFLAGS "$IMAGE" sh -c 'test ! -e scripts && test ! -e /root/.ssh && test ! -e /tmp/lr-proof' || fail "shipped paths present"
if timeout 120 docker run $DFLAGS "$IMAGE" env | grep -E "OPENAI_TUNNEL_ID|OPENAI_API_KEY" >/dev/null 2>&1; then
	fail "secret in runtime env"
fi

# ---- 12b. No stale preinstalls (joint defense): the runtime image
# carries NO globally installed downstream servers — the installer is
# the sole install path (receipts/reconciliation). Negative rows:
# presence checks must FAIL.
echo "== no-stale-preinstalls"
if timeout 120 docker run $DFLAGS "$IMAGE" sh -c 'command -v mcp-server-filesystem' >/dev/null 2>&1; then
	fail "stale global filesystem server present"
fi
if timeout 120 docker run $DFLAGS "$IMAGE" python3 -c 'import mcp_server_time' >/dev/null 2>&1; then
	fail "stale global time server present"
fi
echo "== toolchain-presence"
# Phase-1 row 7 runtime half: all four installer managers present
# in-image (real installs proven at build time in the contract RUN).
timeout 120 docker run $DFLAGS "$IMAGE" sh -c 'command -v npm && command -v uv && command -v cargo && command -v go' || fail "installer manager missing in-image"
# Round 3 (1): uv is image-resident (/opt/mcp/pipx), never the
# persistent volume; image PATH orders image dirs before /var/lib/mcp/bin.
[ "$(timeout 120 docker run $DFLAGS "$IMAGE" sh -c 'command -v uv')" = "/opt/mcp/pipx/bin/uv" ] || fail "uv not image-resident"
timeout 120 docker run $DFLAGS "$IMAGE" sh -c 'case "$PATH" in /opt/mcp/bin:/opt/mcp/pipx/bin:/var/lib/mcp/bin:*) ;; *) exit 1;; esac' || fail "image PATH order"
echo "== missing-config"
# Phase-1 row 6: missing gateway.yaml fails clear (names the path),
# never manufactured — config load precedes tunnel bootstrap, so no
# creds are needed for this row.
set +e
timeout 120 docker run $DFLAGS "$IMAGE" /opt/mcp/bin/localbridge --config /nonexistent/gateway.yaml > "$T/missingcfg.txt" 2>&1
MISSCFG_CODE=$?
set -e
[ "$MISSCFG_CODE" -ne 0 ] || fail "missing config must refuse"
grep -q "config file /nonexistent/gateway.yaml" "$T/missingcfg.txt" || fail "missing config must name the path"
echo "== init-sh"
[ ! -x init.sh ] || fail "init.sh must not carry the exec bit"
grep -q "sh init.sh" init.sh || fail "init.sh invocation undocumented"
sh -n init.sh || fail "init.sh syntax"
if sed 's/#.*//' init.sh | grep -qw "sudo"; then
	fail "init.sh must not sudo"
fi
# Regular-file gate: static presence (the -f check in the per-file
# loop) plus behavioral refusal of a dir-shaped secret under a fake
# HOME. Note: preflight (rootless check) precedes the gate, so on
# non-rootless hosts the non-zero exit may come from preflight — the
# static assert above pins the gate itself.
grep -q '\[ ! -f "\$SECDIR/\$f" \]' init.sh || fail "regular-file gate missing"
mkdir -p "$T/fakehome/.config/localbridge/openai_tunnel_id"
if HOME="$T/fakehome" sh init.sh >/dev/null 2>&1; then
	fail "dir-shaped secret must refuse"
fi
# Instance gate (Phase-1 row 9, DELTA: host-side first): charset check
# precedes ANY docker invocation. Static presence (case gate text
# before the docker preflight line) + mount-boundary behavior (fake
# docker shadow logging every call; illegal names exit non-zero with
# ZERO docker calls).
aft_gate=$(grep -n "command -v docker" init.sh | cut -d: -f1)
gate_line=$(grep -nF '*[!A-Za-z0-9._-]*' init.sh | cut -d: -f1)
[ -n "$gate_line" ] && [ "$gate_line" -lt "$aft_gate" ] || fail "instance gate must precede docker preflight"
mkdir -p "$T/fakebin"
printf '#!/bin/sh\necho "$@" >> "$FAKE_DOCKER_LOG"\nif [ "$1" = "info" ]; then echo "name=rootless"; fi\nexit 0\n' > "$T/fakebin/docker"
chmod +x "$T/fakebin/docker"
for evil in '../evil' 'a/b'; do
	: > "$T/docker.log"
	if FAKE_DOCKER_LOG="$T/docker.log" PATH="$T/fakebin:$PATH" LOCALBRIDGE_INSTANCE="$evil" sh init.sh >/dev/null 2>&1; then
		fail "instance '$evil' must refuse"
	fi
	[ ! -s "$T/docker.log" ] || fail "instance '$evil' invoked docker (mount boundary violated)"
done
# Instance seed: temp HOME + shadowed docker (hermetic regardless of
# host daemon); secrets absent so init.sh exits 1 AFTER seeding —
# assert dir 0700 + gateway.yaml 0600 with `servers: {}`.
mkdir -p "$T/seedhome"
: > "$T/docker.log"
if FAKE_DOCKER_LOG="$T/docker.log" PATH="$T/fakebin:$PATH" HOME="$T/seedhome" sh init.sh >/dev/null 2>&1; then
	fail "seed run must stop at missing secrets"
fi
[ "$(stat -c %a "$T/seedhome/.config/localbridge/default")" = "700" ] || fail "instance dir mode"
[ "$(stat -c %a "$T/seedhome/.config/localbridge/default/gateway.yaml")" = "600" ] || fail "seeded gateway.yaml mode"
grep -qxF "servers: {}" "$T/seedhome/.config/localbridge/default/gateway.yaml" || fail "seeded content"
# Never-overwrite: custom content survives a re-run.
printf 'servers:\n  custom: {}\n' > "$T/seedhome/.config/localbridge/default/gateway.yaml"
if FAKE_DOCKER_LOG="$T/docker.log" PATH="$T/fakebin:$PATH" HOME="$T/seedhome" sh init.sh >/dev/null 2>&1; then
	fail "re-run must stop at missing secrets"
fi
grep -q "custom" "$T/seedhome/.config/localbridge/default/gateway.yaml" || fail "seed overwrote existing config"

# ---- 13. compose rows (C2: Docker-secrets delivery, no real creds) ----
# Mapping lives in Go config parsing now (cmd matrix proves it); these
# rows prove DELIVERY: files mounted readable, _FILE passthrough
# untouched by the entrypoint, direct env exact, absence clean.
# Proof HOME confinement: the base file interpolates ${HOME} for the
# instance mount — point HOME at tmp so docker never auto-creates
# instance dirs in the operator HOME (trap-cleaned).
mkdir -p "$T/chome"
export HOME="$T/chome"
echo "== compose-config"
# Fresh compose project per run (fresh network/volume names). Image
# pinned via LOCAL_MCP_IMAGE + presence-gated, so no row ever builds
# implicitly.
CPROJ="c2proof$$"
# Temp fixtures need NO privilege: created by the invoking user they are
# already correctly owned — VERIFY, never chown. NEVER repo .secrets
# (no repo residue; $T cleanup via trap).
C2S="$T/c2secrets"
mkdir -p "$C2S"
printf 'tid-123\n' > "$C2S/openai_tunnel_id"
printf '  spaced-key  \n' > "$C2S/openai_api_key"
chmod 0400 "$C2S/openai_tunnel_id" "$C2S/openai_api_key"
[ "$(stat -c %u:%a "$C2S/openai_tunnel_id")" = "$(id -u):400" ] || fail "fixture owner must equal invoking UID"
mkoverride() {
	# $1 = secrets dir; $2 = extra environment YAML lines (optional).
	# Command is ALWAYS ["env"]: rows assert env output via up/logs
	# (no `run` anywhere).
	cat > "$T/override.yml" <<EOF
services:
  localbridge:
    image: ${LOCAL_MCP_IMAGE:-localbridge}
    command: ["env"]
${2:-}
secrets:
  tunnel_id:
    file: $1/openai_tunnel_id
  api_key:
    file: $1/openai_api_key
EOF
	C2OVERRIDE="$T/override.yml"
}
mkovempty() {
	# $1 = secrets dir; $2 = extra environment YAML lines. _FILE emptied
	# (absent path) while secrets stay mounted-but-unused.
	cat > "$T/override-empty.yml" <<EOF
services:
  localbridge:
    image: ${LOCAL_MCP_IMAGE:-localbridge}
    command: ["env"]
    environment:
      OPENAI_TUNNEL_ID_FILE: ""
      OPENAI_API_KEY_FILE: ""
${2:-}
secrets:
  tunnel_id:
    file: $1/openai_tunnel_id
  api_key:
    file: $1/openai_api_key
EOF
	C2OVERRIDE="$T/override-empty.yml"
}
c2up() {
	# $1 = output file. Pattern: up -d + docker wait + docker logs +
	# compose rm -sf (no `run --rm` anywhere — its cleanup spins under
	# load). Container name is deterministic (project-service-1); rm
	# after every row keeps the index at -1. Returns container exit code.
	_out=$1
	# up -d output lands in the file too (mount-time refusals surface
	# here, before any container exists to log).
	if ! timeout 120 docker compose -p "$CPROJ" -f docker-compose.yml -f "$C2OVERRIDE" up -d localbridge >"$_out" 2>&1; then
		docker compose -p "$CPROJ" -f docker-compose.yml -f "$C2OVERRIDE" rm -sf localbridge >/dev/null 2>&1 || true
		return 1
	fi
	_code=$(timeout 120 docker wait "$CPROJ-localbridge-1" 2>/dev/null) || _code=124
	timeout 120 docker logs "$CPROJ-localbridge-1" >"$_out" 2>&1 || true
	docker compose -p "$CPROJ" -f docker-compose.yml -f "$C2OVERRIDE" rm -sf localbridge >/dev/null 2>&1 || true
	case "$_code" in
	'' | *[!0-9]*) return 1 ;;
	*) return "$_code" ;;
	esac
}
mkoverride "$C2S"
timeout 120 docker compose -p "$CPROJ" -f docker-compose.yml -f "$T/override.yml" config > "$T/cfg.txt" 2>&1 || fail "compose config"
grep -q "image: $IMAGE" "$T/cfg.txt" || fail "image pin"
grep -q "0:0" "$T/cfg.txt" || fail "user 0:0 pin"
docker image inspect "$IMAGE" >/dev/null || fail "pinned image missing"
# Rendered config leaks no secret values (paths only; values unknowable
# outside the mounts — the proof's own fixture values must not appear).
if grep -q "tid-123" "$T/cfg.txt" || grep -q "spaced-key" "$T/cfg.txt"; then
	fail "config leaks secret values"
fi
echo "== secret-contract"
# CROSS-SURFACE assert (FF-3): the compose DEFAULT file refs (no
# override — overrides mask the defaults that caused the original
# contradiction) must equal the ~/.config/localbridge contract shared
# by init.sh and the bootstrap docs. Render with a fixture HOME so the
# assert is host-independent.
mkdir -p "$T/fakehome"
HOME="$T/fakehome" timeout 120 docker compose -p "$CPROJ" -f docker-compose.yml config > "$T/cfg-base.txt" 2>&1 || fail "base config render"
grep -qxF "    file: $T/fakehome/.config/localbridge/openai_tunnel_id" "$T/cfg-base.txt" || fail "default tunnel_id ref off-contract"
grep -qxF "    file: $T/fakehome/.config/localbridge/openai_api_key" "$T/cfg-base.txt" || fail "default api_key ref off-contract"
echo "== workflow-static"
# Cheap static asserts on the CI workflow (Reviewer verifies by read
# where cheap checks stop): strict glob, scoped cache, max provenance,
# semver gate presence.
grep -q '"v\[0-9\]\*"' .github/workflows/build.yml || fail "workflow glob"
grep -q "scope=localbridge" .github/workflows/build.yml || fail "workflow cache scope"
grep -q "provenance: mode=max" .github/workflows/build.yml || fail "workflow provenance"
# Full strict semver gate (not just the refs/tags/v prefix): the
# workflow must carry the ^v[0-9]+\.[0-9]+\.[0-9]+$ verdict itself.
grep -qF '^v[0-9]+\.[0-9]+\.[0-9]+$' .github/workflows/build.yml || fail "workflow semver gate"
echo "== root-model"
# Rootless primary model: container UID 0 (maps to the host operator,
# not host root). Repo + operator-owned 0400 secrets readable as root;
# cache volume writable as root.
[ "$(timeout 120 docker run --rm --user 0:0 "$IMAGE" id -u)" = "0" ] || fail "euid 0 in-container"
timeout 120 docker run --rm --read-only --tmpfs /tmp --user 0:0 -v "$WS:/workspace:ro" "$IMAGE" sh -c 'test -r /workspace/file.txt' || fail "repo readable as root"
timeout 120 docker run --rm --user 0:0 -v "$C2S:/run/secrets:ro" "$IMAGE" sh -c 'test -r /run/secrets/openai_tunnel_id && test -r /run/secrets/openai_api_key' || fail "secrets readable as root"
timeout 120 docker run --rm --user 0:0 -v "$V_CACHE:/var/lib/mcp/cache" "$IMAGE" sh -c 'touch /var/lib/mcp/cache/rootw' || fail "cache writable as root"
echo "== compose-file-presence"
# Entrypoint performs NO mapping now (single-owner Go): _FILE lines pass
# through verbatim; direct secret vars stay absent.
c2up "$T/cenv.txt" || fail "compose up"
grep -qxF "OPENAI_TUNNEL_ID_FILE=/run/secrets/tunnel_id" "$T/cenv.txt" || fail "tunnel _FILE passthrough"
grep -qxF "OPENAI_API_KEY_FILE=/run/secrets/api_key" "$T/cenv.txt" || fail "api _FILE passthrough"
if grep -q "^OPENAI_TUNNEL_ID=" "$T/cenv.txt" || grep -q "^OPENAI_API_KEY=" "$T/cenv.txt"; then
	fail "entrypoint mapped credentials (single-owner violation)"
fi
echo "== compose-caches"
for kv in "npm_config_cache=/var/lib/mcp/cache/npm" "UV_CACHE_DIR=/var/lib/mcp/cache/uv" "UV_TOOL_DIR=/var/lib/mcp/cache/uvtools" "UV_TOOL_BIN_DIR=/var/lib/mcp/bin" "CARGO_HOME=/var/lib/mcp/cache/cargo" "GOBIN=/var/lib/mcp/bin" "GOCACHE=/var/lib/mcp/cache/go/build" "GOMODCACHE=/var/lib/mcp/cache/go/mod" "GOPATH=/var/lib/mcp/cache/go/path" "PIPX_HOME=/opt/mcp/pipx" "PIPX_BIN_DIR=/opt/mcp/pipx/bin" "HOME=/tmp"; do
	grep -qxF "$kv" "$T/cenv.txt" || fail "cache env $kv"
done
echo "== instance-mount"
# Phase-1 row 5: default compose file mounts the instance dir
# (fixture HOME keeps the assert host-independent); the legacy
# repo-file mount must be gone.
mkdir -p "$T/insthome"
HOME="$T/insthome" timeout 120 docker compose -p "$CPROJ" -f docker-compose.yml config > "$T/cfg-inst.txt" 2>&1 || fail "instance config render"
grep -qxF "        source: $T/insthome/.config/localbridge/default" "$T/cfg-inst.txt" || fail "instance mount source missing"
grep -qxF "        target: /opt/mcp/config" "$T/cfg-inst.txt" || fail "instance mount target missing"
# rw lives in the source file (rendered long syntax marks ro only).
grep -q 'localbridge/${LOCALBRIDGE_INSTANCE:-default}:/opt/mcp/config:rw' docker-compose.yml || fail "instance mount not :rw"
if grep -q "gateway.yaml:/opt/mcp/config" "$T/cfg-inst.txt"; then
	fail "legacy repo-file mount still present"
fi
# Custom instance resolves identically in bind source AND container
# env (the gateway second-layer gate reads the env var — a missing
# passthrough would always see default).
LOCALBRIDGE_INSTANCE="my.inst-1" HOME="$T/insthome" timeout 120 docker compose -p "$CPROJ" -f docker-compose.yml config > "$T/cfg-custom.txt" 2>&1 || fail "custom instance render"
grep -qxF "        source: $T/insthome/.config/localbridge/my.inst-1" "$T/cfg-custom.txt" || fail "custom bind source"
grep -qxF "      LOCALBRIDGE_INSTANCE: my.inst-1" "$T/cfg-custom.txt" || fail "custom env render"
# Runtime container env (up + logs via the env-command override).
mkoverride "$C2S"
LOCALBRIDGE_INSTANCE="my.inst-1" c2up "$T/customenv.txt" || fail "custom instance run"
grep -qxF "LOCALBRIDGE_INSTANCE=my.inst-1" "$T/customenv.txt" || fail "custom container env"
echo "== workspace-root"
# Without WORKSPACE_ROOT the gateway serves the process cwd (container
# /): list_directory(.) would list the container root and git/jj roots
# follow to /. The env assert fails on the old behavior (var absent);
# the mount assert proves the workspace the gateway WILL serve shows
# repo content (AGENTS.md present, container-root entries absent).
# (Serving itself needs tunnel creds — out of proof scope; the default
# chain filesystem.New+cwd and the ResolveRoot workspace fallbacks are
# unit-tested in cmd/git/jj suites.)
grep -qxF "WORKSPACE_ROOT=/workspace" "$T/cenv.txt" || fail "WORKSPACE_ROOT unset"
timeout 120 docker run --rm --read-only --tmpfs /tmp -v "$(pwd):/workspace:ro" "$IMAGE" sh -c 'test -f /workspace/AGENTS.md && test ! -e /workspace/bin && test ! -e /workspace/etc' || fail "workspace mount content"
echo "== compose-readability"
# Mounted fakes readable as container root (compose user 0:0 model).
timeout 120 docker run --rm --user 0:0 -v "$C2S:/run/secrets:ro" \
	-e OPENAI_TUNNEL_ID_FILE=/run/secrets/openai_tunnel_id -e OPENAI_API_KEY_FILE=/run/secrets/openai_api_key \
	"$IMAGE" sh -c 'test -r "$OPENAI_TUNNEL_ID_FILE" && test -r "$OPENAI_API_KEY_FILE"' || fail "secrets unreadable as root"
echo "== compose-direct-env"
mkovempty "$C2S" "      OPENAI_TUNNEL_ID: direct1
      OPENAI_API_KEY: direct2"
c2up "$T/denv.txt" || fail "direct run"
grep -qxF "OPENAI_TUNNEL_ID=direct1" "$T/denv.txt" || fail "direct passthrough id"
grep -qxF "OPENAI_API_KEY=direct2" "$T/denv.txt" || fail "direct passthrough key"
echo "== compose-negative"
mkovempty "$C2S"
c2up "$T/nenv.txt" || fail "negative run"
if grep -q "^OPENAI_TUNNEL_ID=" "$T/nenv.txt"; then
	fail "mapping absence leaked"
fi
echo "== compose-missing-secret"
# RED validation path with ZERO file creation: override points at a
# path nothing ever creates — mount refusal must fail the row (never
# provisioned, never touched).
mkoverride "$T/does-not-exist"
if c2up "$T/missing.txt"; then
	fail "missing secret file must refuse"
fi
echo "== compose-hygiene"
[ -z "$(find . -path ./.devenv -prune -o -name '*.example' -print)" ] || fail "*.example files present"
# Insurance form only: `.secrets/` present in both ignores, bare
# `secrets/` absent from both (stale).
grep -qxF ".secrets/" .gitignore || fail ".gitignore lacks .secrets/ insurance"
grep -qxF ".secrets/" .dockerignore || fail ".dockerignore lacks .secrets/ insurance"
if grep -qxF "secrets/" .gitignore || grep -qxF "secrets/" .dockerignore; then
	fail "stale secrets/ ignore present"
fi
# Tracked subset: operator-local secret files are untracked by design;
# the only trackable member is .gitkeep (placeholder).
tracked=$(jj --no-pager file list 2>/dev/null | grep "\.secrets/" || true)
bad=$(printf '%s\n' "$tracked" | grep -v "\.secrets/\.gitkeep$" || true)
[ -z "$bad" ] || fail "tracked secrets beyond .gitkeep: $bad"
echo "== compose-teardown"
timeout 120 docker compose -p "$CPROJ" -f docker-compose.yml -f "$T/override.yml" down -v >/dev/null || fail "compose down"

echo "container-proof PASS: $IMAGE"
