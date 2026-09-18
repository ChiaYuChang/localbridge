#!/bin/sh
# C1 entrypoint (runs UNDER tini as PID 1 child). Mechanism frozen:
# write /tmp/.gitconfig containing ONLY the workspace trust line, then
# exec CMD. jj needs no exemption (no ownership check).
#
# C2 narrow two-var _FILE mapping (EXACTLY TWO variables — no generic
# *_FILE expansion): file-wins when both set; direct env preserved
# byte-identical when _FILE absent (default path untouched).
set -eu
printf '[safe]\n\tdirectory = /workspace\n' > /tmp/.gitconfig
export GIT_CONFIG_GLOBAL=/tmp/.gitconfig
export GIT_CONFIG_NOSYSTEM=1
# map_file VAR: if VAR_FILE is non-empty, read the credential from that
# path (fail-closed: unreadable path / empty / whitespace-only are hard
# errors naming the VAR, never the value), trim trailing CR/LF ONLY
# (all other bytes preserved incl. intentional spaces), export VAR,
# then UNSET VAR_FILE (no file-path residue). No eval anywhere.
map_file() {
	_var=$1
	case $_var in
	CONTROL_PLANE_TUNNEL_ID) _path=${CONTROL_PLANE_TUNNEL_ID_FILE:-} ;;
	CONTROL_PLANE_API_KEY) _path=${CONTROL_PLANE_API_KEY_FILE:-} ;;
	*) echo "entrypoint: unknown credential ${_var}" >&2; exit 1 ;;
	esac
	_filevar=${_var}_FILE
	[ -n "$_path" ] || return 0
	[ -r "$_path" ] || { echo "entrypoint: ${_filevar} unreadable: ${_path}" >&2; exit 1; }
	# Command substitution strips trailing newlines; strip trailing
	# carriage-returns explicitly (CR/LF-only trim, nothing else).
	_val=$(cat "$_path")
	_cr=$(printf '\r')
	_nl=$(printf '\nX')
	_nl=${_nl%X}
	while :; do
		case "$_val" in
		*"$_cr" | *"$_nl") _val=${_val%?} ;;
		*) break ;;
		esac
	done
	[ -n "$_val" ] || { echo "entrypoint: ${_var} from ${_filevar} empty" >&2; exit 1; }
	case "$_val" in
	*[![:space:]]*) ;;
	*) echo "entrypoint: ${_var} from ${_filevar} whitespace-only" >&2; exit 1 ;;
	esac
	export "$_var=$_val"
	unset "$_filevar"
}
map_file CONTROL_PLANE_TUNNEL_ID
map_file CONTROL_PLANE_API_KEY
exec "$@"
