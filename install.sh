#!/bin/sh
# swoop installer
#   curl -fsSL https://raw.githubusercontent.com/prashant-zo/swoop/main/install.sh | sh
# or, inside a clone of the repo:
#   ./install.sh
set -eu

REPO="prashant-zo/swoop"
BIN="swoop"
INSTALL_DIR="${SWOOP_INSTALL_DIR:-$HOME/.local/bin}"

say() { printf '  %s\n' "$*"; }
die() { printf '  error: %s\n' "$*" >&2; exit 1; }

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  arm64 | aarch64) arch=arm64 ;;
  x86_64 | amd64) arch=amd64 ;;
  *) die "unsupported CPU: $arch" ;;
esac
case "$os" in
  darwin | linux) ;;
  mingw* | msys* | cygwin*)
    die "on Windows run this in PowerShell instead: irm https://raw.githubusercontent.com/$REPO/main/install.ps1 | iex" ;;
  *) die "unsupported OS: $os" ;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

sha256() {
  if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
  else sha256sum "$1" | awk '{print $1}'; fi
}

if [ -f go.mod ] && grep -q "module github.com/$REPO" go.mod && command -v go >/dev/null 2>&1; then
  say "building from local source"
  CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$tmp/$BIN" .
else
  asset="${BIN}_${os}_${arch}"
  base="https://github.com/$REPO/releases/latest/download"
  if curl -fsSL "$base/$asset" -o "$tmp/$BIN" 2>/dev/null; then
    curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt" || die "could not fetch checksums.txt"
    expected=$(awk -v a="$asset" '$2 == a {print $1}' "$tmp/checksums.txt")
    [ -n "$expected" ] || die "no checksum listed for $asset"
    [ "$expected" = "$(sha256 "$tmp/$BIN")" ] || die "checksum mismatch, refusing to install"
    say "downloaded $asset (checksum verified)"
  elif command -v go >/dev/null 2>&1; then
    say "no release binary yet, building with Go"
    GOBIN="$tmp" CGO_ENABLED=0 go install "github.com/$REPO@latest"
  else
    die "no release binary found and Go is not installed (brew install go, then retry)"
  fi
fi

mkdir -p "$INSTALL_DIR"
cp "$tmp/$BIN" "$INSTALL_DIR/$BIN"
chmod 0755 "$INSTALL_DIR/$BIN"
if [ "$os" = darwin ]; then
  xattr -d com.apple.quarantine "$INSTALL_DIR/$BIN" 2>/dev/null || true
fi

say "installed: $INSTALL_DIR/$BIN ($("$INSTALL_DIR/$BIN" -v))"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) say "run: swoop" ;;
  *)
    say "add it to your PATH, then run swoop:"
    say "  echo 'export PATH=\"$INSTALL_DIR:\$PATH\"' >> ~/.zshrc && source ~/.zshrc"
    ;;
esac
