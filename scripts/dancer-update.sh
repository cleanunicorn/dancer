#!/usr/bin/env bash
# dancer-update — pull the latest origin/<branch>, rebuild, install, restart the service.
#
# Run by dancer-update.timer (see deploy/), or by hand:
#   sudo /usr/local/lib/dancer/dancer-update.sh
#   sudo DANCER_UPDATE_FORCE=1 /usr/local/lib/dancer/dancer-update.sh   # rebuild even if unchanged
#
# Contract: never leave the service down. The new binary is built and smoke-tested
# in a scratch directory first; the live binary is only replaced once that passes,
# and the replacement is an atomic rename. A failure at any step exits non-zero
# with the old binary still running.
set -Eeuo pipefail

REPO=${DANCER_REPO:-https://github.com/cleanunicorn/dancer}
BRANCH=${DANCER_BRANCH:-main}
SRC=${DANCER_SRC:-/opt/dancer/src}
BIN=${DANCER_BIN:-/usr/local/bin/dancer}
SERVICE=${DANCER_SERVICE:-dancer.service}
GO=${GO:-go}
LOCK=${DANCER_UPDATE_LOCK:-/var/lock/dancer-update.lock}
# The sha the running binary was built from. Not the checkout's HEAD: the checkout
# is reset before the build, so a failed build would otherwise look "up to date"
# forever while an older binary keeps running.
STATE=${DANCER_UPDATE_STATE:-/var/lib/dancer/deployed.sha}
FORCE=${DANCER_UPDATE_FORCE:-}

log() { printf 'dancer-update: %s\n' "$*"; }
fail() { printf 'dancer-update: ERROR: %s\n' "$*" >&2; exit 1; }

# One updater at a time: a slow build must not overlap the next timer tick.
if [ -z "${DANCER_UPDATE_LOCKED:-}" ]; then
	export DANCER_UPDATE_LOCKED=1
	# -E 75: distinguish "lock is held" from the child's own exit code.
	flock -n -E 75 "$LOCK" "$0" "$@" && exit 0
	rc=$?
	[ "$rc" = 75 ] && { log "another update is already running; skipping this tick"; exit 0; }
	exit "$rc"
fi

command -v git >/dev/null || fail "git not found"
command -v "$GO" >/dev/null || fail "go not found (set GO=/path/to/go)"

if [ ! -d "$SRC/.git" ]; then
	log "cloning $REPO into $SRC"
	mkdir -p "$(dirname "$SRC")"
	git clone --branch "$BRANCH" "$REPO" "$SRC"
fi

git -C "$SRC" remote set-url origin "$REPO"
git -C "$SRC" fetch --prune --quiet origin "$BRANCH"

remote_sha=$(git -C "$SRC" rev-parse "origin/$BRANCH")
deployed_sha=$(cat "$STATE" 2>/dev/null || echo none)

if [ "$deployed_sha" = "$remote_sha" ] && [ -x "$BIN" ] && [ -z "$FORCE" ]; then
	log "up to date at ${remote_sha:0:12}"
	exit 0
fi

log "updating ${deployed_sha:0:12} -> ${remote_sha:0:12} on $BRANCH"

# Discard anything local: this checkout is the deploy's, not a place to edit.
git -C "$SRC" reset --hard --quiet "origin/$BRANCH"
git -C "$SRC" clean -ffdq

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT

log "building"
(cd "$SRC" && "$GO" build -o "$stage/dancer" ./cmd/dancer) || fail "build failed, keeping ${deployed_sha:0:12}"

# Smoke test: a binary that cannot even print its usage must not replace a working one.
"$stage/dancer" -h >/dev/null 2>&1 || fail "new binary failed its smoke test, keeping ${deployed_sha:0:12}"

install -d "$(dirname "$BIN")"
install -m 0755 "$stage/dancer" "$BIN.new"
mv -f "$BIN.new" "$BIN"    # atomic: a concurrent exec sees old or new, never a partial file
install -d "$(dirname "$STATE")"
printf '%s\n' "$remote_sha" > "$STATE"
log "installed $BIN at ${remote_sha:0:12}"

if systemctl is-enabled --quiet "$SERVICE" 2>/dev/null || systemctl is-active --quiet "$SERVICE" 2>/dev/null; then
	log "restarting $SERVICE"
	# SIGTERM: dancer notifies live threads and drains in-flight tool calls first.
	systemctl restart "$SERVICE"
	systemctl is-active --quiet "$SERVICE" || fail "$SERVICE did not come back up"
	log "$SERVICE is up on ${remote_sha:0:12}"
else
	log "$SERVICE is not enabled or active; binary updated, nothing restarted"
fi
