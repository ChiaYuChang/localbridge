#!/bin/sh
# C1 entrypoint (runs UNDER tini as PID 1 child). Mechanism frozen:
# write the workspace trust line, then exec CMD. jj needs no exemption
# (no ownership check).
# safe.directory goes to the HOME default path ($HOME/.gitconfig, i.e.
# /tmp/.gitconfig under the image HOME=/tmp contract): git discovers it
# implicitly, so no GIT_CONFIG_GLOBAL/NOSYSTEM exports are needed — the
# entrypoint manipulates no credential or config-search env at all.
# /etc/gitconfig discovery now relies on the base image shipping none
# (NOSYSTEM dropped accordingly).
# Credential _FILE resolution lives ONLY in Go config parsing
# (single-owner rule); this script performs no credential parsing.
set -eu
printf '[safe]\n\tdirectory = /workspace\n' > "$HOME/.gitconfig"
exec "$@"
