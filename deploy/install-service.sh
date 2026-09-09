#!/usr/bin/env bash
# Install: deploy/install-service.sh [sibling-checkout-name]
# Update self-host: deploy/install-service.sh update <instance>
# Update sibling: deploy/install-service.sh update <instance> <factory-sha>
# Only this transaction may adopt a legacy profile/runtime. Kernel has one layout.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
factory="$(cd "$here/.." && pwd)"
root="$(dirname "$factory")"
unit="$HOME/.config/systemd/user/forest@.service"
flywheel_service="$HOME/.config/systemd/user/forest-eval-flywheel@.service"
flywheel_timer="$HOME/.config/systemd/user/forest-eval-flywheel@.timer"
service_path="$HOME/.local/bin:$HOME/bin:$HOME/.local/share/mise/shims:/usr/local/bin:/usr/bin:/bin"
active_timeout_seconds=60
transaction=""
service_stopped=false
service_restarted=false
source_changed=false
profile_changed=false
runtime_adopted=false
success=false
prior_binary=""
prior_sha=""
prior_paused=true
factory_sha=""

 die() { echo "$(basename "$0"): $*" >&2; exit 1; }

forest_command() { (cd "$target" && PATH="$service_path" "$target/.iron-forest/bin/forest" "$@"); }

stop_instance() {
	echo "$(basename "$0"): stopping forest@$name (stops new dispatches and drains live Runs)"
	service_stopped=true
	local stop_status=0 state
	systemctl --user stop "forest@$name" || stop_status=$?
	state="$(systemctl --user is-active "forest@$name" 2>/dev/null || true)"
	case "$state" in
		inactive|unknown|not-found) ;;
		*) die "forest@$name did not stop (exit $stop_status, state $state)" ;;
	esac
}

restore_profile() {
	# Runtime evidence is never restored from a snapshot: failed adoption may
	# have appended useful evidence, and all of it must survive rollback.
	if [ "$runtime_adopted" = true ]; then
		if [ -d "$target/.iron-forest/runtime" ] && [ ! -e "$target/.forest" ]; then
			mv "$target/.iron-forest/runtime" "$target/.forest" || return 1
		fi
	fi
	if [ "$profile_changed" = true ]; then
		shopt -s nullglob dotglob
		for entry in "$target/.iron-forest"/*; do
			[ "${entry##*/}" = runtime ] && continue
			rm -rf -- "$entry" || return 1
		done
		shopt -u nullglob dotglob
		tar -xf "$transaction/profile.tar" -C "$target" || return 1
	fi
	if [ "$source_changed" = true ]; then
		git -C "$target" reset --hard "$prior_sha" || return 1
	fi
	if [ -n "$prior_binary" ]; then
		mkdir -p "$(dirname "$prior_binary")"
		cp -p "$transaction/binary" "$prior_binary" || return 1
	fi
	if [ -f "$transaction/unit" ]; then
		cp -p "$transaction/unit" "$unit" || return 1
	elif [ -f "$transaction/no-unit" ]; then
		rm -f "$unit" || return 1
	fi
	local admission="$target/.iron-forest/runtime/admission.json"
	if [ -f "$transaction/admission" ]; then
		mkdir -p "$(dirname "$admission")"
		cp -p "$transaction/admission" "$admission" || return 1
	elif [ "$runtime_adopted" != true ]; then
		rm -f "$admission" || return 1
	fi
}

finish_transaction() {
	local status=$?
	trap - EXIT INT TERM
	if [ "$success" != true ] && [ "$service_stopped" = true ]; then
		echo "$(basename "$0"): rolling back this transaction for forest@$name" >&2
		# A newly restarted Kernel is still paused. Stop it before restoring its
		# source, profile and binary. Never restart an incoherent partial rollback.
		if [ "$service_restarted" = true ]; then
			systemctl --user stop "forest@$name" || { echo "rollback could not stop Kernel; backup: $transaction" >&2; exit 1; }
		fi
		exec 8>&-
		local rollback_lock="$target/.iron-forest/runtime/lock"
		[ ! -d "$target/.forest" ] || rollback_lock="$target/.forest/lock"
		mkdir -p "$(dirname "$rollback_lock")"
		exec 8>"$rollback_lock"
		flock -n 8 || { echo "rollback Kernel lock unavailable; backup: $transaction" >&2; exit 1; }
		if ! restore_profile; then
			echo "rollback incomplete; instance remains stopped; backup: $transaction" >&2
			exit 1
		fi
		exec 8>&-
		systemctl --user daemon-reload
		if [ -n "$prior_binary" ]; then
			systemctl --user restart "forest@$name" || { echo "rollback restart failed; backup: $transaction" >&2; exit 1; }
		fi
	fi
	[ -z "$transaction" ] || rm -rf -- "$transaction"
	exit "$status"
}
trap finish_transaction EXIT
trap 'exit 130' INT TERM

verify_factory_revision() {
	local revision="$1" head
	[ -z "$(git -C "$factory" status --porcelain)" ] || die "factory working tree is not clean at $factory; refusing sibling update"
	factory_sha="$(git -C "$factory" rev-parse "$revision^{commit}" 2>/dev/null)" || die "factory revision is absent from $factory: $revision"
	head="$(git -C "$factory" rev-parse HEAD)"
	if [ "$head" != "$factory_sha" ]; then
		if git -C "$factory" merge-base --is-ancestor "$head" "$factory_sha"; then
			die "factory checkout is behind the requested revision (HEAD=$head, requested=$factory_sha); the factory owner must adopt it first"
		fi
		die "factory checkout is not at requested revision (HEAD=$head, requested=$factory_sha)"
	fi
}

snapshot_transaction() {
	transaction="$(mktemp -d -t iron-forest-update.XXXXXXXX)"
	prior_sha="$(git -C "$target" rev-parse HEAD)"
	# An unrelated forest.prev is never an input. Backup completes before any
	# service/source mutation, and only this fresh snapshot can be restored.
	if [ -f "$target/.iron-forest/bin/forest" ]; then
		prior_binary="$target/.iron-forest/bin/forest"
	elif [ -f "$target/forest" ]; then
		prior_binary="$target/forest"
	fi
	if [ -n "$prior_binary" ]; then
		cp -p "$prior_binary" "$transaction/binary"
	fi
	if [ -f "$unit" ]; then cp -p "$unit" "$transaction/unit"; else touch "$transaction/no-unit"; fi
	if [ -f "$target/.iron-forest/runtime/admission.json" ]; then
		cp -p "$target/.iron-forest/runtime/admission.json" "$transaction/admission"
	fi
	local entries=() entry
	for entry in .iron-forest forest.yaml forest.defaults.yaml forest.secrets.yaml agents; do
		[ ! -e "$target/$entry" ] || entries+=("$entry")
	done
	tar -C "$target" --exclude=.iron-forest/runtime --exclude=.iron-forest/bin -cf "$transaction/profile.tar" "${entries[@]}"
}

adopt_profile() {
	profile_changed=true
	mkdir -p "$target/.iron-forest"
	# Versioned config, policy and agent paths must migrate together in source;
	# the installer does not rewrite arbitrary shell commands or agent prompts.
	[ ! -e "$target/forest.yaml" ] && [ ! -e "$target/forest.secrets.yaml" ] && [ ! -e "$target/agents" ] ||
		die "target source must adopt the versioned .iron-forest profile before installation"
	if [ -f "$target/forest.defaults.yaml" ]; then
		[ ! -e "$target/.iron-forest/defaults.yaml" ] || die "both legacy and profile defaults exist"
		mv "$target/forest.defaults.yaml" "$target/.iron-forest/defaults.yaml"
	fi
	if [ -d "$target/.forest" ]; then
		[ ! -e "$target/.iron-forest/runtime" ] || die "both legacy and current runtime directories exist; refusing to combine evidence"
		runtime_adopted=true
		mv "$target/.forest" "$target/.iron-forest/runtime"
	fi
	[ -f "$target/.iron-forest/config.yaml" ] || die "no .iron-forest/config.yaml at $target"
	mkdir -p "$target/.iron-forest/runtime" "$target/.iron-forest/bin"
}

mode=install
factory_revision=""
if [ "${1:-}" = update ]; then
	mode=update
	shift
	[ "$#" -ge 1 ] || die "usage: $(basename "$0") update <instance> [<factory-sha>]"
	name="$1"
	shift
else
	[ "$#" -le 1 ] || die "usage: $(basename "$0") [sibling-checkout-name]"
	name="${1:-$(basename "$factory")}"
	[ "$#" -eq 0 ] || shift
fi
[[ "$name" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || die "invalid instance name"
target="$root/$name"
if [ "$mode" = update ] && [ "$target" != "$factory" ]; then
	[ "$#" -eq 1 ] || die "usage: $(basename "$0") update <instance> <factory-sha>"
	factory_revision="$1"
	verify_factory_revision "$factory_revision"
else
	[ "$#" -eq 0 ] || die "unexpected factory revision"
fi
[ -e "$target/.git" ] || die "no git checkout at $target"
[ -f "$target/.iron-forest/config.yaml" ] || [ -f "$target/forest.yaml" ] || die "no instance profile at $target"
for command in jq flock tar mise; do command -v "$command" >/dev/null || die "$command is required"; done
environment_file="$HOME/.config/iron-forest/$name.env"
[ -f "$environment_file" ] || die "required service environment file is missing: $environment_file"
[ -O "$environment_file" ] || die "service environment file is not owned by the current user: $environment_file"
[ "$(stat -c '%a' "$environment_file")" = 600 ] || die "service environment file must have mode 0600: $environment_file"
if [ "$mode" = update ]; then
	[ -z "$(git -C "$target" status --porcelain)" ] || die "working tree is not clean at $target; refusing to update"
fi
# A deployment lock prevents concurrent installers even before a profile exists.
# It is a Git transaction lock, not a second Kernel runtime or profile loader.
git_dir="$(git -C "$target" rev-parse --absolute-git-dir)"
exec 9>"$git_dir/forest-update.lock"
flock -n 9 || die "another deployment transaction owns $target"
# Reject competing runtime owners before pausing, stopping, snapshotting or
# arming rollback. A legacy lock cannot fence a current-layout Kernel.
if [ -e "$target/.forest" ] && [ -e "$target/.iron-forest/runtime" ]; then
	die "both legacy and current runtime paths exist; reconcile ownership before deployment"
fi
snapshot_transaction
if [ "$mode" = update ] && [ -z "$prior_binary" ]; then die "no installed binary; install this instance first"; fi
if [ "$prior_binary" = "$target/.iron-forest/bin/forest" ]; then
	admission="$(forest_command admission show --json)"
	prior_paused="$(printf '%s\n' "$admission" | jq -er '.data.paused | tostring')"
	# This pause is durable, and drain never cancels existing Runs.
	forest_command admission drain
elif [ -n "$prior_binary" ]; then
	prior_paused=false
fi
stop_instance
lock_path="$target/.iron-forest/runtime/lock"
[ ! -d "$target/.forest" ] || lock_path="$target/.forest/lock"
mkdir -p "$(dirname "$lock_path")"
exec 8>"$lock_path"
flock -n 8 || die "another Kernel still owns $target"
if [ "$mode" = update ]; then
	primary_branch="$(git -C "$target" ls-remote --symref origin HEAD | sed -n 's|^ref: refs/heads/\([^[:space:]]*\)[[:space:]]HEAD$|\1|p')"
	[ -n "$primary_branch" ] || die "remote HEAD symref is missing or malformed for $target"
	echo "$(basename "$0"): fast-forwarding $target to origin/$primary_branch"
	git -C "$target" fetch origin
	source_changed=true
	git -C "$target" merge --ff-only "origin/$primary_branch"
fi
adopt_profile
# New code starts paused. Only a verified adoption restores open admission.
printf '{"paused":true,"updated_at":"%s"}\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$target/.iron-forest/runtime/admission.json"
sha="${factory_sha:-$(git -C "$factory" rev-parse HEAD)}"
commit_time="$(git -C "$factory" show -s --format=%cI "$sha")"
dirty=false
[ -z "$(git -C "$factory" status --porcelain)" ] || dirty=true
ldflags="-X main.buildSHA=$sha -X main.buildTime=$commit_time -X main.buildDirty=$dirty"
(cd "$factory" && mise exec -- go build -ldflags "$ldflags" -o "$target/.iron-forest/bin/forest.next" .)
mv "$target/.iron-forest/bin/forest.next" "$target/.iron-forest/bin/forest"
version_json="$(forest_command version --json)"
[ "$(printf '%s\n' "$version_json" | jq -r '.data.build_sha // empty')" = "$sha" ] || die "installed forest build_sha mismatch"
forest_command selfcheck
# The installer owns the lock; release it for the audited command, which acquires
# the same lock itself. Admission remains durably paused throughout this window.
exec 8>&-
forced_audit="$(forest_command audit show --rescan --json)"
printf '%s\n' "$forced_audit" | jq -e '.exit == 0 and (.data.last_result == "pass" or .data.last_result == "not_applicable")' >/dev/null || die "forced audit did not pass or report external delivery"
mkdir -p "$(dirname "$unit")"
sed -e "s|@FOREST_ROOT@|$root|g" "$here/forest@.service" > "$unit"
systemctl --user daemon-reload
systemctl --user enable "forest@$name"
service_restarted=true
systemctl --user restart "forest@$name"
deadline=$(( $(date +%s) + active_timeout_seconds ))
until [ "$(systemctl --user is-active "forest@$name" 2>/dev/null || true)" = active ]; do
	[ "$(date +%s)" -lt "$deadline" ] || die "forest@$name did not become active"
	sleep 1
done
status="$(forest_command status --json)"
expected_audit="$(printf '%s\n' "$forced_audit" | jq -c '.data | {last_result, last_at}')"
printf '%s\n' "$status" | jq -e --argjson expected "$expected_audit" '.exit == 0 and (.data.audit | {last_result, last_at}) == $expected' >/dev/null || die "installed audit receipt changed after restart"
if [ "$prior_paused" = false ]; then forest_command admission resume; fi
# From here the adopted Kernel/profile are authoritative; stale old executable
# artifacts are not a rollback mechanism and are never read on a later update.
[ "$prior_binary" != "$target/forest" ] || rm -f "$target/forest"
success=true
if [ "$target" = "$factory" ]; then
	sed -e "s|@FOREST_ROOT@|$root|g" "$here/forest-eval-flywheel@.service" > "$flywheel_service"
	cp "$here/forest-eval-flywheel@.timer" "$flywheel_timer"
	systemctl --user daemon-reload
	systemctl --user enable --now "forest-eval-flywheel@$name.timer"
fi
if [ "$mode" = update ]; then echo "$(basename "$0"): updated forest@$name"; else echo "$(basename "$0"): installed forest@$name (paused)"; fi
echo "  profile: $target/.iron-forest (source $sha; prior paused=$prior_paused)"
