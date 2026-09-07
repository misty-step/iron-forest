#!/usr/bin/env bash
# Install the Iron Forest template unit in one explicit checkout mode.
#
# Self-host mode builds and enables the factory source checkout:
#   deploy/install-service.sh
#
# Sibling mode builds the same factory source into one sibling managed checkout:
#   deploy/install-service.sh <sibling-checkout-name>
#
# Update an installed instance through the fenced adoption procedure:
#   deploy/install-service.sh update <instance>
#
# For a sibling managed checkout (target != factory), the exact factory
# Revision must be passed and must already be adopted in this checkout:
#   deploy/install-service.sh update <instance> <factory-sha>
#
# The sibling form verifies that Revision before stopping the consumer unit;
# it never mutates the factory checkout.
#
# Each instance runs the selected checkout's forest.yaml.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
factory="$(cd "$here/.." && pwd)"
root="$(dirname "$factory")"
unit="$HOME/.config/systemd/user/forest@.service"
flywheel_service="$HOME/.config/systemd/user/forest-eval-flywheel@.service"
flywheel_timer="$HOME/.config/systemd/user/forest-eval-flywheel@.timer"
service_path="$HOME/.local/bin:$HOME/bin:$HOME/.local/share/mise/shims:/usr/local/bin:/usr/bin:/bin"

# Bounded waits used only by `update`. The drain itself has no wall-clock
# deadline (ADR 0020); the active wait is a post-restart liveness bound.
active_timeout_seconds=60

# Rollback state used only by `update`; the install path leaves them unset.
service_stopped=false
update_success=false
cancelled=false
tree_was_clean=false
prev_sha=""
prev_binary=""
factory_revision=""
factory_sha=""
status_json=""
audit_since_ns=0

die() { echo "$(basename "$0"): $*" >&2; exit 1; }

on_signal() {
	cancelled=true
	exit 130
}
trap on_signal INT TERM

rollback_on_exit() {
	if [ "$update_success" = true ] || [ "$service_stopped" != true ]; then
		return 0
	fi
	if [ "$cancelled" = true ]; then
		echo "$(basename "$0"): update cancelled; restoring prior instance forest@$name" >&2
	else
		echo "$(basename "$0"): rolling back to prior instance forest@$name" >&2
	fi
	if [ -n "$prev_binary" ] && [ -f "$prev_binary" ]; then
		cp -p "$prev_binary" "$target/forest" >/dev/null 2>&1 ||
			echo "$(basename "$0"): rollback: could not restore $target/forest" >&2
	fi
	if [ "$tree_was_clean" = true ] && [ -n "$prev_sha" ]; then
		git -C "$target" reset --hard "$prev_sha" >/dev/null 2>&1 ||
			echo "$(basename "$0"): rollback: could not reset $target to $prev_sha" >&2
	fi
	# The restored pre-change .gitignore does not ignore forest.prev, so the
	# update's untracked rollback artifact would fail the next clean-tree check.
	if [ -n "$prev_binary" ]; then
		rm -f -- "$prev_binary" >/dev/null 2>&1 ||
			echo "$(basename "$0"): rollback: could not remove $prev_binary" >&2
	fi
	systemctl --user restart "forest@$name" >/dev/null 2>&1 ||
		echo "$(basename "$0"): rollback: could not restart forest@$name" >&2
}
trap rollback_on_exit EXIT

refresh_status() {
	if status_json="$( (cd "$target" && env -u FOREST_DEFAULTS PATH="$service_path" ./forest status --json) 2>&1 )"; then
		return 0
	else
		status_exit=$?
		die "forest status failed (exit $status_exit): $status_json"
	fi
}

audit_ready() {
	if ! printf '%s\n' "$status_json" | jq -e '
		.exit == 0 and
		.data.audit.last_result == "pass" and
		(.data.audit.last_at | type == "string") and
		(.data.audit.last_at != "")
	' >/dev/null 2>&1; then
		return 1
	fi
	last_at="$(printf '%s\n' "$status_json" | jq -r '.data.audit.last_at')"
	last_ns="$(date -u -d "$last_at" +%s%N 2>/dev/null || echo 0)"
	[ "${last_ns:-0}" -ge "${audit_since_ns:-0}" ]
}

verify_factory_revision() {
	revision="$1"
	[ -n "$revision" ] || die "missing factory revision for sibling update"
	[ -d "$factory/.git" ] || [ -f "$factory/.git" ] || die "no git checkout at $factory"

	if [ -n "$(git -C "$factory" status --porcelain)" ]; then
		die "factory working tree is not clean at $factory; refusing sibling update"
	fi
	if ! factory_sha="$(git -C "$factory" rev-parse "$revision^{commit}" 2>/dev/null)" || [ -z "$factory_sha" ]; then
		die "factory revision is absent from $factory: $revision"
	fi
	factory_head="$(git -C "$factory" rev-parse HEAD)"
	if [ "$factory_head" != "$factory_sha" ]; then
		if git -C "$factory" merge-base --is-ancestor "$factory_head" "$factory_sha" >/dev/null 2>&1; then
			die "factory checkout is behind the requested revision (HEAD=$factory_head, requested=$factory_sha); the factory owner must adopt it first"
		fi
		die "factory checkout is not at the requested revision (HEAD=$factory_head, requested=$factory_sha)"
	fi
}

update_instance() {
	# The update subcommand adopts a merged revision into an installed instance.
	# It preserves the protected environment and refreshes only the self-host
	# evaluation timer because that timer ships with this factory checkout.
	command -v jq >/dev/null 2>&1 || die "jq is required for update"
	[ -d "$target/.git" ] || [ -f "$target/.git" ] || die "no git checkout at $target"
	[ -f "$target/forest.yaml" ] || die "no forest.yaml at $target; a managed repository declares its own factory"

	environment_file="$HOME/.config/iron-forest/$name.env"
	echo "$(basename "$0"): required service environment file: $environment_file (protect as mode 0600)"
	[ -f "$environment_file" ] || die "required service environment file is missing or not a regular file: $environment_file"
	[ -O "$environment_file" ] || die "service environment file is not owned by the current user: $environment_file"
	environment_mode="$(stat -c '%a' "$environment_file")"
	[ "$environment_mode" = 600 ] || die "service environment file must have mode 0600, found $environment_mode: $environment_file"

	# Sibling updates never mutate the factory checkout. The factory revision
	# is a separate source input and must already be adopted there before the
	# consumer fence begins.
	if [ "$target" != "$factory" ]; then
		verify_factory_revision "$factory_revision"
	fi

	# Clean-tree precondition (recorded). A dirty tree aborts before the service
	# is touched, before any fetch, and before any tracked file moves.
	if [ -n "$(git -C "$target" status --porcelain)" ]; then
		die "working tree is not clean at $target; refusing to update"
	fi
	tree_was_clean=true
	prev_sha="$(git -C "$target" rev-parse HEAD)"
	prev_binary="$target/forest.prev"
	[ -f "$target/forest" ] || die "no forest binary at $target/forest; run install-service.sh first"

	echo "$(basename "$0"): stopping forest@$name (stops new dispatches and drains live Runs)"
	service_stopped=true
	stop_status=0
	systemctl --user stop "forest@$name" >/dev/null 2>&1 || stop_status=$?
	instance_status=0
	instance_state="$(systemctl --user is-active "forest@$name" 2>/dev/null)" || instance_status=$?
	case "$instance_state" in
		inactive|unknown|not-found) ;;
		*) die "forest@$name is not inactive or not-found (stop exit $stop_status, state $instance_state, query exit $instance_status)" ;;
	esac

	primary_branch="$(git -C "$target" ls-remote --symref origin HEAD 2>/dev/null | awk '
		$1 == "ref:" && $3 == "HEAD" {
			sub(/refs\/heads\//, "", $2)
			print $2
			exit
		}
	')"
	[ -n "$primary_branch" ] || die "remote HEAD symref is missing or malformed for $target"
	echo "$(basename "$0"): fast-forwarding $target to origin/$primary_branch"
	if ! git -C "$target" fetch origin; then
		die "git fetch origin failed in $target"
	fi
	if ! git -C "$target" merge --ff-only "origin/$primary_branch"; then
		die "git merge --ff-only origin/$primary_branch failed in $target"
	fi

	echo "$(basename "$0"): preserving prior binary as forest.prev (previous $prev_sha)"
	if ! cp -p "$target/forest" "$target/forest.prev"; then
		die "could not preserve $target/forest as $target/forest.prev"
	fi

	echo "$(basename "$0"): building Kernel from $factory into $target"
	if [ "$target" = "$factory" ]; then
		build_sha="$(git -C "$factory" rev-parse HEAD)"
	else
		build_sha="$factory_sha"
	fi
	stamp="$(git -C "$factory" rev-parse --short "$build_sha")"
	sha="$build_sha"
	commit_time="$(git -C "$factory" show -s --format=%cI "$build_sha")"
	dirty="false"
	if [ -n "$(git -C "$factory" status --porcelain)" ]; then
		dirty="true"
	fi
	ldflags="-X main.buildSHA=$sha -X main.buildTime=$commit_time -X main.buildDirty=$dirty"
	if ! (cd "$factory" && mise exec -- go build -ldflags "$ldflags" -o "$target/forest" .); then
		die "build failed; rolling back to the prior instance"
	fi

	echo "$(basename "$0"): verifying installed forest reports build_sha $sha"
	if ! version_json="$( (cd "$target" && env -u FOREST_DEFAULTS PATH="$service_path" ./forest version --json) 2>&1 )"; then
		die "forest version --json failed: $version_json"
	fi
	installed_sha="$(printf '%s\n' "$version_json" | jq -r '.data.build_sha // empty')"
	if [ "$installed_sha" != "$sha" ]; then
		die "installed forest build_sha mismatch (expected $sha, got $installed_sha)"
	fi

	echo "$(basename "$0"): validating $target with forest selfcheck"
	if ! (cd "$target" && env -u FOREST_DEFAULTS PATH="$service_path" ./forest selfcheck); then
		die "selfcheck failed; rolling back to the prior instance"
	fi

	echo "$(basename "$0"): forcing a fresh audit pass before restart"
	if ! forced_audit_json="$( (cd "$target" && env -u FOREST_DEFAULTS PATH="$service_path" ./forest audit show --rescan --json) 2>&1 )"; then
		die "forest audit show --rescan failed: $forced_audit_json"
	fi
	forced_audit_result="$(printf '%s\n' "$forced_audit_json" | jq -r '.data.last_result // empty')"
	forced_audit_at="$(printf '%s\n' "$forced_audit_json" | jq -r '.data.last_at // empty')"
	[ "$forced_audit_result" = pass ] || die "forced audit did not pass: $forced_audit_json"
	[ -n "$forced_audit_at" ] || die "forced audit did not record last_at"
	audit_since_ns="$(date -u -d "$forced_audit_at" +%s%N 2>/dev/null || echo 0)"
	[ "${audit_since_ns:-0}" -gt 0 ] || die "could not parse audit last_at: $forced_audit_at"

	echo "$(basename "$0"): starting forest@$name"
	if ! systemctl --user restart "forest@$name" >/dev/null 2>&1; then
		die "systemctl restart forest@$name failed; rolling back to the prior instance"
	fi

	echo "$(basename "$0"): waiting for forest@$name to become active"
	deadline=$(( $(date +%s) + active_timeout_seconds ))
	while true; do
		state="$(systemctl --user is-active "forest@$name" 2>/dev/null || true)"
		if [ "$state" = active ]; then
			break
		fi
		if [ "$(date +%s)" -ge "$deadline" ]; then
			die "forest@$name did not become active within ${active_timeout_seconds}s (state=$state)"
		fi
		sleep 1
	done

	echo "$(basename "$0"): verifying the fresh audit pass after restart"
	refresh_status
	if ! audit_ready; then
		die "fresh audit pass not observed for forest@$name after restart"
	fi

	update_success=true
	if [ "$target" = "$factory" ] && [ "$name" = "$(basename "$factory")" ]; then
		echo "$(basename "$0"): refreshing the self-host evaluation flywheel timer"
		mkdir -p "$(dirname "$flywheel_service")"
		sed -e "s|@FOREST_ROOT@|$root|g" \
			"$target/deploy/forest-eval-flywheel@.service" > "$flywheel_service"
		cp "$target/deploy/forest-eval-flywheel@.timer" "$flywheel_timer"
		systemctl --user daemon-reload
		systemctl --user enable "forest-eval-flywheel@$name.timer" >/dev/null
		systemctl --user start "forest-eval-flywheel@$name.timer"
	fi
	echo "$(basename "$0"): updated forest@$name"
	echo "  instance: forest@$name -> $target (mode: update; source: $factory at $stamp)"
	echo "  status:   systemctl --user status 'forest@*'"
	echo "  logs:     journalctl --user -u 'forest@*' -f"
}

command="${1:-}"
case "$command" in
	update)
		shift
		[ "$#" -ge 1 ] || die "usage: $(basename "$0") update <instance>"
		name="$1"
		target="$root/$name"
		shift
		if [ "$target" = "$factory" ]; then
			[ "$#" -eq 0 ] || die "usage: $(basename "$0") update <instance>"
			factory_revision=""
		else
			[ "$#" -eq 1 ] || die "usage: $(basename "$0") update <instance> <factory-sha>"
			factory_revision="$1"
		fi
		update_instance
		exit 0
		;;
esac

case "$#" in
	0)
		mode="self-host"
		name="$(basename "$factory")"
		target="$factory"
		;;
	1)
		mode="sibling"
		name="$1"
		target="$root/$name"
		[ "$target" != "$factory" ] || die "use no argument for self-host mode"
		;;
	*)
		die "usage: $(basename "$0") [sibling-checkout-name]"
		;;
esac
environment_file="$HOME/.config/iron-forest/$name.env"

[ -d "$target/.git" ] || [ -f "$target/.git" ] || die "no git checkout at $target"
[ -f "$target/forest.yaml" ] || die "no forest.yaml at $target; a managed repository declares its own factory"
echo "$(basename "$0"): required service environment file: $environment_file (protect as mode 0600)"
[ -f "$environment_file" ] || die "required service environment file is missing or not a regular file: $environment_file"
[ -O "$environment_file" ] || die "service environment file is not owned by the current user: $environment_file"
environment_mode="$(stat -c '%a' "$environment_file")"
[ "$environment_mode" = 600 ] || die "service environment file must have mode 0600, found $environment_mode: $environment_file"

# Retire the single-instance unit this template replaces, and prove it is gone.
if [ -f "$HOME/.config/systemd/user/forest.service" ]; then
	disable_status=0
	systemctl --user disable --now forest.service >/dev/null 2>&1 || disable_status=$?
	legacy_status=0
	legacy_state="$(systemctl --user is-active forest.service 2>/dev/null)" || legacy_status=$?
	case "$legacy_state" in
		inactive|unknown|not-found) ;;
		*) die "legacy forest.service is not inactive or not-found (disable exit $disable_status, state $legacy_state, query exit $legacy_status)" ;;
	esac
	rm "$HOME/.config/systemd/user/forest.service"
fi

# Stop this instance before removing legacy credential-bearing Run profiles.
# Only timestamped reserved names created by the old Kernel are eligible.
stop_status=0
systemctl --user stop "forest@$name" >/dev/null 2>&1 || stop_status=$?
instance_status=0
instance_state="$(systemctl --user is-active "forest@$name" 2>/dev/null)" || instance_status=$?
case "$instance_state" in
	inactive|unknown|not-found) ;;
	*) die "forest@$name is not inactive or not-found (stop exit $stop_status, state $instance_state, query exit $instance_status)" ;;
esac
legacy_profiles="$target/.forest/profiles"
if [ -d "$legacy_profiles" ]; then
	shopt -s nullglob
	for profile in "$legacy_profiles"/*; do
		profile_name="${profile##*/}"
		case "$profile_name" in
			[0-9]*-[A-Za-z0-9_-]*)
				[[ "$profile_name" =~ ^[0-9]+-[A-Za-z0-9_-]+$ ]] || continue
				rm -rf -- "$profile"
				;;
		esac
	done
	rmdir "$legacy_profiles" 2>/dev/null || true
fi

mkdir -p "$(dirname "$unit")"
sed -e "s|@FOREST_ROOT@|$root|g" \
	"$here/forest@.service" > "$unit"
if [ "$mode" = self-host ]; then
	sed -e "s|@FOREST_ROOT@|$root|g" \
		"$here/forest-eval-flywheel@.service" > "$flywheel_service"
	cp "$here/forest-eval-flywheel@.timer" "$flywheel_timer"
fi

# Build the Kernel from the factory source into the selected target. Build
# metadata is stamped at link time so `forest version` reports the exact
# revision, commit time, and whether the source tree was modified.
stamp="$(git -C "$factory" rev-parse --short HEAD)"
sha="$(git -C "$factory" rev-parse HEAD)"
commit_time="$(git -C "$factory" show -s --format=%cI HEAD)"
dirty="false"
if [ -n "$(git -C "$factory" status --porcelain)" ]; then
	dirty="true"
fi
ldflags="-X main.buildSHA=$sha -X main.buildTime=$commit_time -X main.buildDirty=$dirty"
(cd "$factory" && mise exec -- go build -ldflags "$ldflags" -o "$target/forest" .)

# Validate with the same trusted PATH that the service receives. Credentials
# remain outside the installer and arrive through the service environment file.
(cd "$target" && env -u FOREST_DEFAULTS PATH="$service_path" ./forest selfcheck)

systemctl --user daemon-reload
systemctl --user enable "forest@$name" >/dev/null
if [ "$mode" = self-host ]; then
	systemctl --user enable "forest-eval-flywheel@$name.timer" >/dev/null
	systemctl --user start "forest-eval-flywheel@$name.timer"
fi
systemctl --user restart "forest@$name"
echo "$(basename "$0"): installed $unit"
echo "  instance: forest@$name -> $target (mode: $mode; source: $factory at $stamp)"
if [ "$mode" = self-host ]; then
	echo "  evals:    systemctl --user status 'forest-eval-flywheel@*'"
fi
echo "  status:   systemctl --user status 'forest@*'"
echo "  logs:     journalctl --user -u 'forest@*' -f"
