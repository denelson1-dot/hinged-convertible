#!/usr/bin/env bash
#
# Install hinged for the current user.
#
# Deliberately not the script it replaces: no hardcoded home directory, no
# refusal to run as anyone but one person, and it tells you what it found
# before it changes anything.

set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="${XDG_BIN_HOME:-$HOME/.local/bin}"
UNIT="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
RULES=/etc/udev/rules.d

DO_UDEV=1
DRY=0
for arg in "$@"; do
    case "$arg" in
        --no-udev)  DO_UDEV=0 ;;
        --dry-run)  DRY=1 ;;
        -h|--help)
            sed -n '2,10p' "$0"
            echo
            echo "Usage: ./install.sh [--no-udev] [--dry-run]"
            exit 0 ;;
        *) echo "unknown option: $arg" >&2; exit 2 ;;
    esac
done

say()  { printf '==> %s\n' "$*"; }
step() { if ((DRY)); then printf '    would run: %s\n' "$*"; else eval "$*"; fi; }

command -v go >/dev/null || { echo "Go is required to build hinged" >&2; exit 1; }

say "Building"
step "CGO_ENABLED=0 go build -trimpath -o '$REPO/hinged' ./cmd/hinged"

say "What this machine looks like"
"$REPO/hinged" doctor | sed 's/^/    /'
echo

say "Installing the binary into $BIN"
step "mkdir -p '$BIN'"
step "install -m755 '$REPO/hinged' '$BIN/hinged'"

# The panel is a separate binary so the daemon stays dependency-free: the
# only third-party package in this repo is the pure-Go X11 client, and it is
# reachable from the panel alone.
if CGO_ENABLED=0 go build -trimpath -o "$REPO/hinged-panel" ./cmd/hinged-panel 2>/dev/null; then
    step "install -m755 '$REPO/hinged-panel' '$BIN/hinged-panel'"
else
    echo "    (panel did not build; the daemon works without it)"
fi

if ((DO_UDEV)); then
    say "Installing udev rules (needs sudo)"
    echo "    /dev/uinput  - to create the virtual switch"
    echo "    switch nodes - to read the lid, which decides what a near-zero angle means"
    step "sudo install -m644 '$REPO/packaging/udev/70-hinged-uinput.rules' '$RULES/'"
    step "sudo install -m644 '$REPO/packaging/udev/70-hinged-switch.rules' '$RULES/'"
    step "sudo udevadm control --reload"
    step "sudo udevadm trigger --subsystem-match=input --subsystem-match=misc"
    step "sudo modprobe uinput || true"
else
    say "Skipping udev rules (--no-udev)"
    echo "    Without them the daemon cannot create its virtual switch."
fi

say "Installing the systemd user service"
step "mkdir -p '$UNIT'"
step "install -m644 '$REPO/packaging/systemd/hinged.service' '$UNIT/'"
step "install -m644 '$REPO/packaging/systemd/hinged-restore.service' '$UNIT/'"
step "systemctl --user daemon-reload"

cat <<MSG

Installed. Next:

  hinged doctor              confirm /dev/uinput is now writable
  hinged daemon --dry-run    decide everything, change nothing
  systemctl --user enable --now hinged

If udev rules were just installed, the new permissions may need a re-login
before they apply to your session.
MSG
