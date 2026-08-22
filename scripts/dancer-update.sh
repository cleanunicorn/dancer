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
# A sha that built fine but whose binary would not stay up. Retrying it every tick
# would restart dancer twice per tick forever, so it is skipped until the branch moves.
POISON="$STATE.failed"
# How long the restarted service must stay up before the deploy counts as good.
GRACE=${DANCER_UPDATE_GRACE:-10}
FORCE=${DANCER_UPDATE_FORCE:-}
# Also act as a watchdog: start the service if it is enabled but not running.
# 0 disables that, e.g. while you keep it stopped for maintenance.
WATCHDOG=${DANCER_UPDATE_WATCHDOG:-1}

log() { printf 'dancer-update: %s\n' "$*"; }

# The sha check alone cannot notice that dancer is down: with no new commit every
# tick reports "up to date" and moves on. That is how a stray `pkill` kept the box
# down for 23 minutes once. Restart=always covers the common case; this covers a
# unit sitting in `failed` after tripping systemd's start rate limit, which
# Restart= cannot get out of on its own.
ensure_running() {
	[ "$WATCHDOG" = 1 ] || return 0
	systemctl is-enabled --quiet "$SERVICE" 2>/dev/null || return 0
	systemctl is-active --quiet "$SERVICE" && return 0
	log "$SERVICE is enabled but not running — starting it"
	systemctl reset-failed "$SERVICE" 2>/dev/null || true
	if systemctl start "$SERVICE"; then
		log "$SERVICE started"
	else
		log "WARNING: could not start $SERVICE"
	fi
}
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
	ensure_running
	exit 0
fi

if [ "$(cat "$POISON" 2>/dev/null || true)" = "$remote_sha" ] && [ -z "$FORCE" ]; then
	log "${remote_sha:0:12} already failed to stay up; waiting for a new commit on $BRANCH"
	log "(DANCER_UPDATE_FORCE=1 retries it anyway)"
	ensure_running
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

install -d "$(dirname "$BIN")" "$(dirname "$STATE")"
# Keep the binary we are replacing: it is the only thing known to actually run.
if [ -x "$BIN" ]; then cp -f "$BIN" "$BIN.prev"; fi
install -m 0755 "$stage/dancer" "$BIN.new"
mv -f "$BIN.new" "$BIN"    # atomic: a concurrent exec sees old or new, never a partial file
log "installed $BIN at ${remote_sha:0:12}"

commit_state() { printf '%s\n' "$remote_sha" > "$STATE"; rm -f "$POISON"; }

rollback() {
	printf '%s\n' "$remote_sha" > "$POISON"
	if [ ! -x "$BIN.prev" ]; then
		fail "$SERVICE did not stay up on ${remote_sha:0:12} and there is no previous binary to restore"
	fi
	log "rolling back to the previous binary"
	cp -f "$BIN.prev" "$BIN.rollback"
	mv -f "$BIN.rollback" "$BIN"
	# A unit that just crash-looped has tripped systemd's start rate limit, and every
	# further `restart` is refused with "start request repeated too quickly". Without
	# this the rollback puts the good binary back and still leaves the service down.
	systemctl reset-failed "$SERVICE" 2>/dev/null || true
	systemctl restart "$SERVICE" || true
	sleep 2
	if systemctl is-active --quiet "$SERVICE"; then
		log "rolled back to ${deployed_sha:0:12}; $SERVICE is up again"
	else
		log "WARNING: $SERVICE is still down after the rollback — needs a human"
	fi
	fail "$SERVICE did not stay up on ${remote_sha:0:12}; rolled back, and this sha is skipped until $BRANCH moves"
}

if ! systemctl is-enabled --quiet "$SERVICE" 2>/dev/null && ! systemctl is-active --quiet "$SERVICE" 2>/dev/null; then
	commit_state
	log "$SERVICE is not enabled or active; binary updated, nothing restarted"
	exit 0
fi

log "restarting $SERVICE"
# SIGTERM: dancer notifies live threads and drains in-flight tool calls first.
restarts_before=$(systemctl show -p NRestarts --value "$SERVICE" 2>/dev/null || echo 0)
systemctl restart "$SERVICE" || rollback

# `systemctl restart` returns as soon as the process is up, which a binary that
# dies on startup also satisfies. Give it long enough to fall over first.
sleep "$GRACE"
restarts_after=$(systemctl show -p NRestarts --value "$SERVICE" 2>/dev/null || echo 0)
systemctl is-active --quiet "$SERVICE" || rollback
if [ "${restarts_after:-0}" -gt "${restarts_before:-0}" ]; then
	log "$SERVICE restarted itself $((restarts_after - restarts_before))x within ${GRACE}s — treating as a crash loop"
	rollback
fi

commit_state
log "$SERVICE is up on ${remote_sha:0:12}"
