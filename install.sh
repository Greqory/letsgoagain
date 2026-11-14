#!/bin/sh
# Installs or updates LetsGoAgain from its GitHub releases:
#   curl -fsSL https://raw.githubusercontent.com/Greqory/letsgoagain/main/install.sh | sh
# Downloads the binary for this machine, checks it against the release's checksums.txt, puts it in
# ~/.local/bin and installs the systemd user service. Running it again updates.
#
# Environment:
#   LETSGOAGAIN_VERSION=v1.2.3     a specific release instead of the latest
#   LETSGOAGAIN_INSTALL_DIR=DIR    where the binary goes (default ~/.local/bin)
#   LETSGOAGAIN_DRY_RUN=1          install the service in dry-run mode (records stops, sends nothing)
#   LETSGOAGAIN_NO_SERVICE=1       only the binary, no systemd service
set -eu

repo="Greqory/letsgoagain"
base="${LETSGOAGAIN_BASE_URL:-https://github.com}"
dir="${LETSGOAGAIN_INSTALL_DIR:-$HOME/.local/bin}"

say() { printf 'letsgoagain: %s\n' "$*"; }
die() { printf 'letsgoagain: %s\n' "$*" >&2; exit 1; }

[ "$(uname -s)" = Linux ] || die "only Linux is supported"
case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "unsupported CPU: $(uname -m) (supported: x86_64, aarch64)" ;;
esac
command -v sha256sum >/dev/null 2>&1 || die "sha256sum is needed"
if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -q -O "$2" "$1"; }
else
	die "curl or wget is needed"
fi

asset="letsgoagain_linux_$arch"
if [ -n "${LETSGOAGAIN_VERSION:-}" ]; then
	url="$base/$repo/releases/download/$LETSGOAGAIN_VERSION"
else
	url="$base/$repo/releases/latest/download"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

say "downloading $asset"
fetch "$url/$asset" "$tmp/$asset" || die "download failed: $url/$asset"
fetch "$url/checksums.txt" "$tmp/checksums.txt" || die "download failed: $url/checksums.txt"
(cd "$tmp" && grep " $asset\$" checksums.txt | sha256sum -c --quiet -) || die "checksum mismatch, nothing installed"

mkdir -p "$dir"
chmod 755 "$tmp/$asset"
mv -f "$tmp/$asset" "$dir/letsgoagain.new"
mv -f "$dir/letsgoagain.new" "$dir/letsgoagain"
say "installed $("$dir/letsgoagain" version) to $dir/letsgoagain"

case ":$PATH:" in
	*":$dir:"*) ;;
	*) say "note: $dir is not in your PATH; add it to run letsgoagain by name" ;;
esac

if [ -n "${LETSGOAGAIN_NO_SERVICE:-}" ]; then
	say "skipping the service (LETSGOAGAIN_NO_SERVICE); start it with: letsgoagain install"
	exit 0
fi
command -v systemctl >/dev/null 2>&1 || die "systemctl not found; run the daemon yourself: letsgoagain daemon"
if [ -n "${LETSGOAGAIN_DRY_RUN:-}" ]; then
	"$dir/letsgoagain" install --dry-run
else
	"$dir/letsgoagain" install
fi
say "done. Check it with: letsgoagain status"
