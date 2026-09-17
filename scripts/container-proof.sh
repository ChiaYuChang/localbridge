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
set -eu

IMAGE="${1:?usage: container-proof.sh <image>}"
BUILD_TIMEOUT="${BUILD_TIMEOUT:-1800}"

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
V_NPM="c1proof-npm-$$"
V_UV="c1proof-uv-$$"
V_PIPX="c1proof-pipx-$$"
REAP_C="c1proof-reap-$$"
CANARY_C="c1proof-canary-$$"
cleanup() {
	rm -rf "$T"
	docker rm -f "$REAP_C" "$CANARY_C" >/dev/null 2>&1 || true
	docker volume rm -f "$V_NPM" "$V_UV" "$V_PIPX" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Shared run flags (runtime contract): read-only root, tmpfs /tmp,
# ro workspace, named cache volumes. Unquoted on purpose (see NOTE).
# shellcheck disable=SC2086: DFLAGS word-split intentional, space-free paths
DFLAGS="--rm --read-only --tmpfs /tmp -v $WS:/workspace:ro -v $V_NPM:/var/lib/mcp/npm -v $V_UV:/var/lib/mcp/uv -v $V_PIPX:/var/lib/mcp/pipx"

# ---- fixture repos (host UID ownership, != 10001 by gate above) ----
# NOTE: the git repo lives AT /workspace itself: safe.directory matches
# exact paths only (verified: a bare /workspace entry does NOT cover
# /workspace/subdir), and the trust line is frozen to exactly that value.
mkdir -p "$WS"
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
timeout "$BUILD_TIMEOUT" docker build --target test -t local-mcp:test -f Dockerfile . || fail "test stage"

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
for marker in ["CONTROL_PLANE_API_KEY", "OPENAI_API_KEY", "/root/.ssh", ".secrets", "id_rsa", "id_ed25519"]:
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
timeout 120 docker run $DFLAGS -e WORKSPACE_ROOT=/workspace "$IMAGE" /opt/mcp/bin/gateway > "$SMOKE_OUT" 2>&1
SMOKE_CODE=$?
set -e
cat "$SMOKE_OUT"
# No-hang proof: timeout kills at 124 — reject explicitly. Expected
# contract: config-error exit 1 complaining about the missing tunnel ID
# (creds never supplied; serving out of scope).
[ "$SMOKE_CODE" -ne 124 ] || fail "smoke hung (timeout kill)"
[ "$SMOKE_CODE" -eq 1 ] || fail "smoke exit $SMOKE_CODE, want config-error 1"
grep -q "CONTROL_PLANE_TUNNEL_ID" "$SMOKE_OUT" || fail "smoke output missing config complaint"

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

# ---- 9. frozen diff-argv canary (external driver must NOT run) ----
# Rationale: external diff drivers execute without a TTY, so the canary
# is non-vacuous (removing --no-ext-diff flips it). core.pager /
# core.editor are TTY-gated instead and explicitly untested.
# The marker lives in /tmp (tmpfs), read back via docker exec; the live
# container keeps /workspace ro throughout.
echo "== diff-canary"
docker run -d --name "$CANARY_C" --read-only --tmpfs /tmp \
	-v "$WS:/workspace:ro" \
	-v "$V_NPM:/var/lib/mcp/npm" \
	-v "$V_UV:/var/lib/mcp/uv" \
	-v "$V_PIPX:/var/lib/mcp/pipx" \
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
timeout 120 docker run $DFLAGS "$IMAGE" sh -c 'touch /tmp/ok && touch /var/lib/mcp/npm/m1 /var/lib/mcp/uv/m2 /var/lib/mcp/pipx/m3' || fail "writable dirs"
timeout 120 docker run $DFLAGS "$IMAGE" sh -c 'test -f /var/lib/mcp/npm/m1 && test -f /var/lib/mcp/uv/m2 && test -f /var/lib/mcp/pipx/m3' || fail "cache persistence"

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
if timeout 120 docker run $DFLAGS "$IMAGE" env | grep -i "CONTROL_PLANE" >/dev/null 2>&1; then
	fail "secret in runtime env"
fi

echo "container-proof PASS: $IMAGE"
