#!/bin/sh
# init.sh — operator bootstrap for container-path proxy (host-side only).
# Run as: sh init.sh
# Committed WITHOUT the exec bit (asserted in proof); never chmod +x.
# MUST NOT chown/chmod anything inside the repo; MUST NOT require sudo
# (no sudo appears in this file — bootstrap touches only $HOME paths
# owned by the invoking user).
set -eu

fail() { echo "FAIL: $1" >&2; exit 1; }

# ---- 0. instance gate (HOST-SIDE FIRST: before ANY docker invocation;
# direct compose-with-custom-instance is UNSUPPORTED — this gate is the
# mount boundary; the gateway re-validates as second layer only) ----
LOCALBRIDGE_INSTANCE="${LOCALBRIDGE_INSTANCE:-default}"
case "$LOCALBRIDGE_INSTANCE" in
	"") fail "LOCALBRIDGE_INSTANCE must not be empty" ;;
	.|..) fail "LOCALBRIDGE_INSTANCE must not be '$LOCALBRIDGE_INSTANCE'" ;;
	*[!A-Za-z0-9._-]*) fail "LOCALBRIDGE_INSTANCE '$LOCALBRIDGE_INSTANCE' must match [A-Za-z0-9._-]+" ;;
esac
echo "instance: $LOCALBRIDGE_INSTANCE"

# ---- preflight: docker present, rootless daemon ----
# (Host needs nothing else: downstream installs run in-container via
# the gateway installer; the old npx/python3 host prefetch is gone.)
command -v docker >/dev/null 2>&1 || fail "docker CLI missing"
docker info >/dev/null 2>&1 || fail "docker daemon unreachable"
if docker info -f '{{.SecurityOptions}}' 2>/dev/null | grep -q "name=rootless"; then
	echo "daemon: rootless"
else
	fail "daemon is not rootless (primary model requires rootless; container UID 0 must map to the operator, not host root)"
fi

# ---- instance seed (create-time only: dir 0700 + minimal gateway.yaml
# 0600 iff absent; NEVER overwrites; NEVER manufactures at runtime —
# the entrypoint/gateway fail clear on a missing gateway.yaml) ----
# Runs BEFORE the secrets check: provisioning must not be blocked by
# missing credentials.
INSTDIR="$HOME/.config/localbridge/$LOCALBRIDGE_INSTANCE"
mkdir -p "$INSTDIR"
chmod 0700 "$INSTDIR"
[ "$(stat -c %a "$INSTDIR")" = "700" ] || fail "mode of $INSTDIR must be 0700"
if [ ! -e "$INSTDIR/gateway.yaml" ]; then
	printf 'servers: {}\n' > "$INSTDIR/gateway.yaml"
	chmod 0600 "$INSTDIR/gateway.yaml"
	echo "seeded: $INSTDIR/gateway.yaml (servers: {}, 0600)"
else
	[ -f "$INSTDIR/gateway.yaml" ] || fail "not a regular file: $INSTDIR/gateway.yaml"
	echo "instance config kept: $INSTDIR/gateway.yaml (never overwritten)"
fi

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

# ---- next steps ----
cat <<'EOF'
next steps:
  1. Create + fill ~/.config/localbridge/{openai_tunnel_id,openai_api_key} if missing, keep 0400 (see above).
  2. Rebuild + prove: docker build -t localbridge:plan -f Dockerfile . && sh scripts/container-proof.sh localbridge:plan
  3. Live sweep: docker compose up -d, connect the tunnel, call each forwarded tool via the online agent.
EOF
