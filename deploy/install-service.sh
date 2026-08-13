#!/usr/bin/env bash
# Install the Iron Forest template unit in one explicit checkout mode.
#
# Self-host mode builds and enables the factory source checkout:
#   deploy/install-service.sh
#
# Sibling mode builds the same factory source into one sibling managed checkout:
#   deploy/install-service.sh <sibling-checkout-name>
#
# Each instance runs the selected checkout's forest.yaml.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
factory="$(cd "$here/.." && pwd)"
root="$(dirname "$factory")"
unit="$HOME/.config/systemd/user/forest@.service"
service_path="$HOME/.local/bin:$HOME/bin:$HOME/.local/share/mise/shims:/usr/local/bin:/usr/bin:/bin"

die() { echo "$(basename "$0"): $*" >&2; exit 1; }

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

# Build the Kernel from the factory source into the selected target.
stamp="$(git -C "$factory" rev-parse --short HEAD)"
(cd "$factory" && mise exec -- go build -o "$target/forest" .)

# Validate with the same trusted PATH that the service receives. Credentials
# remain outside the installer and arrive through the service environment file.
(cd "$target" && env -u FOREST_DEFAULTS PATH="$service_path" ./forest selfcheck)

systemctl --user daemon-reload
systemctl --user enable "forest@$name" >/dev/null
systemctl --user restart "forest@$name"
echo "$(basename "$0"): installed $unit"
echo "  instance: forest@$name -> $target (mode: $mode; source: $factory at $stamp)"
echo "  status:   systemctl --user status 'forest@*'"
echo "  logs:     journalctl --user -u 'forest@*' -f"
