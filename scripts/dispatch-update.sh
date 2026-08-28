#!/usr/bin/env bash
# dispatch-update — pull the latest origin/<branch>, rebuild, install, restart the service.
#
# Run by dispatch-update.timer (see deploy/), or by hand:
#   sudo /usr/local/lib/dispatch/dispatch-update.sh
#   sudo DISPATCH_UPDATE_FORCE=1 /usr/local/lib/dispatch/dispatch-update.sh   # rebuild even if unchanged
#
# Contract: never leave the service down. The new binary is built and smoke-tested
# in a scratch directory first; the live binary is only replaced once that passes,
# and the replacement is an atomic rename. A failure at any step exits non-zero
# with the old binary still running.
#
# It also deploys the *glue* — this script and the unit files — because a release
# that changes deploy/ is as much "the new version" as one that changes the Go
# code, and installing only the binary silently leaves the box on the old units.
set -Eeuo pipefail

log() { printf 'dispatch-update: %s\n' "$*"; }
fail() { printf 'dispatch-update: ERROR: %s\n' "$*" >&2; exit 1; }

# ---- configuration ---------------------------------------------------------
# Values already in the environment win, so `sudo env DISPATCH_BIN=... this-script`
# still overrides the installed record.
ENVFILE=${DISPATCH_DEPLOY_ENV:-/etc/dispatch/deploy.env}
if [ -r "$ENVFILE" ]; then
	while IFS='=' read -r key val; do
		case "$key" in ''|'#'*) continue ;; esac
		eval "existing=\${$key-}"
		[ -n "$existing" ] || eval "export $key=\$val"
	done < "$ENVFILE"
fi

REPO=${DISPATCH_REPO:-https://github.com/cleanunicorn/dispatch}
BRANCH=${DISPATCH_BRANCH:-main}
SRC=${DISPATCH_SRC:-/opt/dispatch/src}
BIN=${DISPATCH_BIN:-/usr/local/bin/dispatch}
SERVICE=${DISPATCH_SERVICE:-dispatch.service}
GO=${GO:-go}
LOCK=${DISPATCH_UPDATE_LOCK:-/var/lock/dispatch-update.lock}
# The sha the running binary was built from. Not the checkout's HEAD: the checkout
# is reset before the build, so a failed build would otherwise look "up to date"
# forever while an older binary keeps running.
STATE=${DISPATCH_UPDATE_STATE:-/var/lib/dispatch/deployed.sha}
# A sha that built fine but whose binary would not stay up. Retrying it every tick
# would restart dispatch twice per tick forever, so it is skipped until the branch moves.
POISON="$STATE.failed"
# How long the restarted service must stay up before the deploy counts as good.
GRACE=${DISPATCH_UPDATE_GRACE:-10}
FORCE=${DISPATCH_UPDATE_FORCE:-}
# Also act as a watchdog: start the service if it is enabled but not running.
# 0 disables that, e.g. while you keep it stopped for maintenance.
WATCHDOG=${DISPATCH_UPDATE_WATCHDOG:-1}
# 0 leaves the units and this script alone (binary-only deploys).
SYNC_GLUE=${DISPATCH_UPDATE_SYNC_GLUE:-1}

UPDATER=${DISPATCH_UPDATER:-/usr/local/lib/dispatch/dispatch-update.sh}
UNIT=${DISPATCH_UNIT:-/etc/systemd/system/dispatch.service}
UPDATE_UNIT=${DISPATCH_UPDATE_UNIT:-/etc/systemd/system/dispatch-update.service}
UPDATE_TIMER=${DISPATCH_UPDATE_TIMER:-/etc/systemd/system/dispatch-update.timer}

# The sha check alone cannot notice that dispatch is down: with no new commit every
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

# ---- one updater at a time -------------------------------------------------
# A slow build must not overlap the next timer tick.
if [ -z "${DISPATCH_UPDATE_LOCKED:-}" ]; then
	export DISPATCH_UPDATE_LOCKED=1
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

# Reset every tick, not only when deploying: the glue below is rendered from this
# checkout, so it has to match the branch even on a tick that builds nothing.
# Discard anything local — this checkout is the deploy's, not a place to edit.
git -C "$SRC" reset --hard --quiet "origin/$BRANCH"
git -C "$SRC" clean -ffdq

# ---- glue: this script, then the units -------------------------------------

# Replace this script and hand over to the new copy, so a release that changes the
# updater takes effect on the tick that brings it in rather than the one after.
sync_self() {
	[ "$SYNC_GLUE" = 1 ] || return 0
	local new="$SRC/scripts/dispatch-update.sh"
	[ -f "$new" ] || return 0
	cmp -s "$new" "$UPDATER" && return 0
	[ -z "${DISPATCH_UPDATE_REEXEC:-}" ] || {
		log "WARNING: updater still differs from the checkout after re-exec; continuing with the old one"
		return 0
	}
	log "updater changed on $BRANCH — installing and handing over"
	install -d "$(dirname "$UPDATER")"
	install -m 0755 "$new" "$UPDATER.new"
	mv -f "$UPDATER.new" "$UPDATER"
	export DISPATCH_UPDATE_REEXEC=1
	# The flock fd is inherited across exec, so the lock is still held.
	exec "$UPDATER" "$@"
}

render_unit() {
	sed -e "s|__USER__|${DISPATCH_USER:-}|g" \
		-e "s|__GROUP__|${DISPATCH_GROUP:-}|g" \
		-e "s|__HOME__|${DISPATCH_HOME:-}|g" \
		-e "s|__BIN__|$BIN|g" \
		-e "s|__REPO__|$REPO|g" \
		-e "s|__BRANCH__|$BRANCH|g" \
		-e "s|__SRC__|$SRC|g" \
		-e "s|__GO__|$GO|g" \
		-e "s|__UPDATER__|$UPDATER|g" \
		-e "s|__INTERVAL__|${DISPATCH_INTERVAL:-5min}|g" \
		"$1"
}

units_changed=""
dispatch_unit_changed=""
staged=""
trap 'rm -rf "${staged:-}"' EXIT

sync_units() {
	[ "$SYNC_GLUE" = 1 ] || return 0
	if [ ! -r "$ENVFILE" ]; then
		log "no $ENVFILE — cannot re-render units; run 'make service-install' and 'make update-install' once"
		return 0
	fi
	local src dst name pair
	staged=$(mktemp -d)
	for pair in "dispatch.service:$UNIT" "dispatch-update.service:$UPDATE_UNIT" "dispatch-update.timer:$UPDATE_TIMER"; do
		name=${pair%%:*}; dst=${pair#*:}
		src="$SRC/deploy/$name"
		[ -f "$src" ] || continue
		if [ "$name" = dispatch.service ] && { [ -z "${DISPATCH_USER:-}" ] || [ -z "${DISPATCH_GROUP:-}" ] || [ -z "${DISPATCH_HOME:-}" ]; }; then
			log "WARNING: $ENVFILE has no DISPATCH_USER/GROUP/HOME — leaving $name alone"
			continue
		fi
		render_unit "$src" > "$staged/$name"
		cmp -s "$staged/$name" "$dst" && continue
		log "$name changed on $BRANCH — installing"
		if [ -f "$dst" ]; then cp -f "$dst" "$dst.prev"; fi
		install -m 0644 "$staged/$name" "$dst"
		units_changed="$units_changed $name"
		if [ "$name" = dispatch.service ]; then dispatch_unit_changed=1; fi
	done
	[ -n "$units_changed" ] || return 0

	systemctl daemon-reload
	# A unit systemd cannot parse is worse than a stale one. LoadState is the check
	# that matters — it is what systemd made of the file we just wrote.
	# (`systemd-analyze verify` is not usable here: it exits non-zero when ExecStart
	# points at a binary that is not installed yet, which is every first install, and
	# exits zero for an unknown directive.)
	for name in $units_changed; do
		dst=$(unit_path "$name")
		# Ask about the unit as *installed*: the destination may be named differently
		# from the template it was rendered from, and querying the template name would
		# silently inspect some other unit on the box.
		[ "$(systemctl show "$(basename "$dst")" -p LoadState --value 2>/dev/null)" = loaded ] && continue
		log "WARNING: systemd could not load $name from $BRANCH — restoring the previous one"
		if [ -f "$dst.prev" ]; then install -m 0644 "$dst.prev" "$dst"; fi
		units_changed=$(echo "$units_changed" | sed "s/\b$name\b//")
		if [ "$name" = dispatch.service ]; then dispatch_unit_changed=""; fi
		systemctl daemon-reload
	done
	case "$units_changed" in
		*dispatch-update.timer*)
			systemctl reenable dispatch-update.timer >/dev/null 2>&1 || true
			systemctl restart dispatch-update.timer || log "WARNING: could not restart dispatch-update.timer"
			;;
	esac
	log "units updated:$units_changed"
}

unit_path() {
	case "$1" in
		dispatch.service) echo "$UNIT" ;;
		dispatch-update.service) echo "$UPDATE_UNIT" ;;
		dispatch-update.timer) echo "$UPDATE_TIMER" ;;
	esac
}

sync_self "$@"
sync_units

# ---- binary ----------------------------------------------------------------

deployed_sha=$(cat "$STATE" 2>/dev/null || echo none)

# A changed dispatch.service only takes effect on the next start. If a deploy is
# about to restart dispatch anyway, let it; otherwise restart here.
restart_for_unit() {
	[ -n "$dispatch_unit_changed" ] || return 0
	systemctl is-active --quiet "$SERVICE" || return 0
	log "restarting $SERVICE to pick up the new unit"
	if systemctl restart "$SERVICE" && sleep 2 && systemctl is-active --quiet "$SERVICE"; then
		return 0
	fi
	# It loaded but will not run — a bad value for a good directive only shows up here.
	if [ -f "$UNIT.prev" ]; then
		log "WARNING: $SERVICE will not start on the new unit — restoring the previous one"
		install -m 0644 "$UNIT.prev" "$UNIT"
		systemctl daemon-reload
		systemctl reset-failed "$SERVICE" 2>/dev/null || true
		systemctl start "$SERVICE" || log "WARNING: $SERVICE is still down — needs a human"
	else
		log "WARNING: $SERVICE will not start and there is no previous unit to restore"
	fi
}

if [ "$deployed_sha" = "$remote_sha" ] && [ -x "$BIN" ] && [ -z "$FORCE" ]; then
	log "up to date at ${remote_sha:0:12}"
	restart_for_unit
	ensure_running
	exit 0
fi

if [ "$(cat "$POISON" 2>/dev/null || true)" = "$remote_sha" ] && [ -z "$FORCE" ]; then
	log "${remote_sha:0:12} already failed to stay up; waiting for a new commit on $BRANCH"
	log "(DISPATCH_UPDATE_FORCE=1 retries it anyway)"
	restart_for_unit
	ensure_running
	exit 0
fi

log "updating ${deployed_sha:0:12} -> ${remote_sha:0:12} on $BRANCH"

stage=$(mktemp -d)
trap 'rm -rf "$stage" "${staged:-}"' EXIT

log "building"
(cd "$SRC" && "$GO" build -o "$stage/dispatch" ./cmd/dispatch) || fail "build failed, keeping ${deployed_sha:0:12}"

# Smoke test: a binary that cannot even print its usage must not replace a working one.
"$stage/dispatch" -h >/dev/null 2>&1 || fail "new binary failed its smoke test, keeping ${deployed_sha:0:12}"

install -d "$(dirname "$BIN")" "$(dirname "$STATE")"
# Keep the binary we are replacing: it is the only thing known to actually run.
if [ -x "$BIN" ]; then cp -f "$BIN" "$BIN.prev"; fi
install -m 0755 "$stage/dispatch" "$BIN.new"
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
	# If this tick also changed the unit, the unit is just as likely to be why the
	# service will not come up — put both back, not only the half we suspect.
	if [ -n "$dispatch_unit_changed" ] && [ -f "$UNIT.prev" ]; then
		log "also restoring the previous $UNIT"
		install -m 0644 "$UNIT.prev" "$UNIT"
		systemctl daemon-reload
	fi
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
# SIGTERM: dispatch notifies live threads and drains in-flight tool calls first.
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
