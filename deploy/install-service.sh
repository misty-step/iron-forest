#!/usr/bin/env bash
# Install the Iron Forest template unit and enable one instance per checkout.
#
#   deploy/install-service.sh              # enable this checkout
#   deploy/install-service.sh landmark     # also enable the sibling checkout
#
# One organization runs one installation. Instance names are sibling directories
# of the factory source, so the organization's checkout directory is the root and
# no repository has to move.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
factory="$(cd "$here/.." && pwd)"
root="$(dirname "$factory")"
name="${1:-$(basename "$factory")}"
target="$root/$name"
unit="$HOME/.config/systemd/user/forest@.service"

die() { echo "$(basename "$0"): $*" >&2; exit 1; }

[ -d "$target/.git" ] || die "no git checkout at $target"
[ -f "$target/forest.yaml" ] || die "no forest.yaml at $target; a managed repository declares its own factory"

# Retire the single-instance unit this template replaces, so one machine never
# runs two daemons against the same checkout.
if [ -f "$HOME/.config/systemd/user/forest.service" ]; then
	systemctl --user disable --now forest.service >/dev/null 2>&1 || true
	rm -f "$HOME/.config/systemd/user/forest.service"
fi

mkdir -p "$(dirname "$unit")"
sed -e "s|@FOREST_ROOT@|$root|g" -e "s|@FACTORY_DIR@|$factory|g" \
	"$here/forest@.service" > "$unit"

# Seed the binary from the factory source. A managed checkout is never built: it
# may be in any language, and self-update rebuilds it from the factory source.
stamp="$(git -C "$factory" rev-parse --short HEAD)"
(cd "$factory" && mise exec -- go build -ldflags "-X main.version=$stamp" -o "$target/forest" .)

systemctl --user daemon-reload
systemctl --user enable "forest@$name" >/dev/null
systemctl --user restart "forest@$name"
echo "$(basename "$0"): installed $unit"
echo "  instance: forest@$name -> $target (source: $factory at $stamp)"
echo "  status:   systemctl --user status 'forest@*'"
echo "  logs:     journalctl --user -u 'forest@*' -f"
