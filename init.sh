#!/bin/sh
# init.sh — operator bootstrap for container-path proxy (host-side only).
# Run as: sh init.sh
# Committed WITHOUT the exec bit (asserted in proof); never chmod +x.
# MUST NOT chown/chmod anything inside the repo; MUST NOT require sudo
# (no sudo appears in this file — bootstrap touches only $HOME paths
# owned by the invoking user).
set -eu

fail() { echo "FAIL: $1" >&2; exit 1; }
warn() { echo "WARN: $1" >&2; }

# ---- preflight: docker present, rootless daemon, versions ----
command -v docker >/dev/null 2>&1 || fail "docker CLI missing"
docker info >/dev/null 2>&1 || fail "docker daemon unreachable"
if docker info -f '{{.SecurityOptions}}' 2>/dev/null | grep -q "name=rootless"; then
	echo "daemon: rootless"
else
	fail "daemon is not rootless (primary model requires rootless; container UID 0 must map to the operator, not host root)"
fi
command -v npx >/dev/null 2>&1 || fail "npx missing (host-side prefetch needs it)"
command -v python3 >/dev/null 2>&1 || fail "python3 missing (host-side prefetch needs it)"
echo "host: $(npx --version 2>/dev/null || echo 'npx-unavailable') / $(python3 --version 2>&1)"

# ---- secrets bootstrap (HOME only, invoking user owns everything) ----
# Secret home: ~/.config/localbridge/ (single location shared by
# compose refs, docs, and proof contract). Dir 0700, files 0400,
# owner == invoking UID (verify-not-chown kept).
SECDIR="$HOME/.config/localbridge"
mkdir -p "$SECDIR"
chmod 0700 "$SECDIR"
[ "$(stat -c %a "$SECDIR")" = "700" ] || fail "mode of $SECDIR must be 0700"
for f in openai_tunnel_id openai_api_key; do
	if [ ! -e "$SECDIR/$f" ]; then
		cat >&2 <<EOF
missing $SECDIR/$f — create it yourself and set mode 0400.
EOF
		exit 1
	fi
	# Regular-file gate (a directory named like a secret must fail
	# closed, not validate green).
	if [ ! -f "$SECDIR/$f" ]; then
		echo "not a regular file: $SECDIR/$f" >&2
		exit 1
	fi
	[ "$(stat -c %u "$SECDIR/$f")" = "$(id -u)" ] || fail "owner of $SECDIR/$f must equal invoking UID"
	[ "$(stat -c %a "$SECDIR/$f")" = "400" ] || fail "mode of $SECDIR/$f must be 0400"
done
echo "secrets dir OK: $SECDIR (dir 0700, owner + 0400 verified, never chowned)"

# ---- prefetch for host-side runs (Phase 2a cache; warn-only offline) ----
# NOTE: --help is consumed as a directory arg (server starts, rejects
# the bogus dir): "accessible" in output proves cache hit + execution.
if npx -y @modelcontextprotocol/server-filesystem@2026.8.31 --help 2>&1 | grep -q "accessible"; then
	echo "prefetch: server-filesystem cached"
else
	warn "npx prefetch failed (offline?) — Phase 2a driver will report SERVER_MISSING"
fi
if python3 -c "import mcp_server_time" 2>/dev/null; then
	echo "prefetch: mcp-server-time importable"
else
	warn "mcp_server_time not importable — install per your python policy, then re-run"
fi

# ---- next steps ----
cat <<'EOF'
next steps:
  1. Create + fill ~/.config/localbridge/{openai_tunnel_id,openai_api_key} if missing, keep 0400 (see above).
  2. Rebuild + prove: docker build -t localbridge:plan -f Dockerfile . && sh scripts/container-proof.sh localbridge:plan
  3. Live sweep: docker compose up -d, connect the tunnel, call each forwarded tool via the online agent.
EOF
