#!/bin/sh
# Installs the latest meterm release after checking it against the published
# checksums. Needs no root. METERM_INSTALL_DIR changes where it goes.
set -eu

repo=https://github.com/bcd91/meterm
dest=${METERM_INSTALL_DIR:-$HOME/.local/bin}

case $(uname -s) in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) echo "meterm: no installer for $(uname -s); see $repo/releases" >&2; exit 1 ;;
esac
case $(uname -m) in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) echo "meterm: no release for $(uname -m); see $repo/releases" >&2; exit 1 ;;
esac

url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$repo/releases/latest")
tag=${url##*/}
case $tag in
    v[0-9]*) ;;
    *) echo "meterm: couldn't find the latest release ($url)" >&2; exit 1 ;;
esac
archive=meterm_${tag#v}_${os}_${arch}.tar.gz

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
curl -fsSL -o "$work/$archive" "$repo/releases/download/$tag/$archive"
curl -fsSL -o "$work/checksums.txt" "$repo/releases/download/$tag/checksums.txt"

if command -v sha256sum >/dev/null 2>&1; then
    sum=sha256sum
else
    sum="shasum -a 256"
fi
want=$(awk -v f="$archive" '$2 == f { print $1 }' "$work/checksums.txt")
got=$($sum "$work/$archive" | awk '{ print $1 }')
if [ -z "$want" ] || [ "$want" != "$got" ]; then
    echo "meterm: checksum mismatch for $archive; nothing installed" >&2
    exit 1
fi

tar -xzf "$work/$archive" -C "$work" meterm
mkdir -p "$dest"
# Through a temporary name, so a running meterm keeps working.
install -m 755 "$work/meterm" "$dest/.meterm.new"
mv -f "$dest/.meterm.new" "$dest/meterm"

echo "Installed: $("$dest/meterm" --version) at $dest/meterm"
found=$(command -v meterm 2>/dev/null || true)
if [ "$found" != "$dest/meterm" ]; then
    echo "Note: this shell runs ${found:-nothing} as meterm; put $dest first on PATH." >&2
fi
