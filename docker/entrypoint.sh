#!/bin/sh
# C1 entrypoint (runs UNDER tini as PID 1 child). Mechanism frozen:
# write /tmp/.gitconfig containing ONLY the workspace trust line, then
# exec CMD. jj needs no exemption (no ownership check).
set -eu
printf '[safe]\n\tdirectory = /workspace\n' > /tmp/.gitconfig
export GIT_CONFIG_GLOBAL=/tmp/.gitconfig
export GIT_CONFIG_NOSYSTEM=1
exec "$@"
