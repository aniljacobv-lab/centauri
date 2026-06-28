#!/bin/sh
# Centauri installer for Linux and macOS.
#   curl -fsSL https://centauridb.ai/install.sh | sh
# Installs the right binary to /usr/local/bin and tells you how to start.
set -e

REPO="aniljacobv-lab/centauri"

OS="$(uname -s)"
ARCH="$(uname -m)"
case "$OS" in
  Linux)  os=linux ;;
  Darwin) os=darwin ;;
  *) echo "Unsupported OS: $OS (download manually from https://github.com/$REPO/releases)"; exit 1 ;;
esac
case "$ARCH" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

# Stable asset names mean this URL always points at the newest release.
# Pin a version with: CENTAURI_VERSION=v0.3.0 curl ... | sh
if [ -n "${CENTAURI_VERSION:-}" ]; then
  URL="https://github.com/$REPO/releases/download/$CENTAURI_VERSION/centauri-$os-$arch"
else
  URL="https://github.com/$REPO/releases/latest/download/centauri-$os-$arch"
fi
echo "Downloading Centauri for $os/$arch ..."
TMP="$(mktemp)"
curl -fSL -o "$TMP" "$URL"
chmod +x "$TMP"

DEST=/usr/local/bin/centauri
if [ -w "$(dirname $DEST)" ]; then
  mv "$TMP" "$DEST"
else
  echo "(sudo needed to write $DEST)"
  sudo mv "$TMP" "$DEST"
fi

echo ""
echo "✓ Installed: $DEST"
echo ""
echo "Start it (your browser will open automatically):"
echo "    centauri desktop"
echo ""
echo "Your data lives in ~/.config/Centauri/ — one file, yours."
