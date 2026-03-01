#!/usr/bin/env sh
# Mantis installer
# Usage:  curl -fsSL https://raw.githubusercontent.com/seedhire/mantis/main/install.sh | sh
# Or:     curl -fsSL https://raw.githubusercontent.com/seedhire/mantis/main/install.sh | sh -s -- --install-dir /usr/local/bin

set -e

REPO="seedhire/mantis"
INSTALL_DIR="${MANTIS_INSTALL_DIR:-/usr/local/bin}"
BINARY="mantis"

# ── Parse flags ───────────────────────────────────────────────────────────────
while [ "$#" -gt 0 ]; do
  case "$1" in
    --install-dir) INSTALL_DIR="$2"; shift 2 ;;
    --version)     VERSION="$2";     shift 2 ;;
    *) echo "Unknown option: $1"; exit 1 ;;
  esac
done

# ── Detect OS ─────────────────────────────────────────────────────────────────
OS="$(uname -s)"
case "$OS" in
  Linux)   OS="linux"   ;;
  Darwin)  OS="darwin"  ;;
  MINGW*|MSYS*|CYGWIN*) OS="windows" ;;
  *) echo "Unsupported OS: $OS"; exit 1 ;;
esac

# ── Detect arch ───────────────────────────────────────────────────────────────
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64)        ARCH="amd64" ;;
  aarch64|arm64)       ARCH="arm64" ;;
  *) echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

# ── Resolve version ───────────────────────────────────────────────────────────
if [ -z "$VERSION" ]; then
  printf "Fetching latest release... "
  VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep '"tag_name"' \
    | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')"
  echo "$VERSION"
fi

# ── Build download URL ────────────────────────────────────────────────────────
if [ "$OS" = "windows" ]; then
  EXT="zip"
else
  EXT="tar.gz"
fi

FILENAME="mantis_${OS}_${ARCH}.${EXT}"
URL="https://github.com/${REPO}/releases/download/${VERSION}/${FILENAME}"

# ── Download ──────────────────────────────────────────────────────────────────
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

printf "Downloading %s ... " "$FILENAME"
curl -fsSL "$URL" -o "${TMP}/${FILENAME}"
echo "done"

# ── Extract ───────────────────────────────────────────────────────────────────
printf "Extracting... "
cd "$TMP"
if [ "$EXT" = "zip" ]; then
  unzip -q "$FILENAME"
else
  tar -xzf "$FILENAME"
fi
echo "done"

# ── Install ───────────────────────────────────────────────────────────────────
if [ ! -d "$INSTALL_DIR" ]; then
  mkdir -p "$INSTALL_DIR"
fi

# Check write permission; fall back to sudo
if [ -w "$INSTALL_DIR" ]; then
  mv "${TMP}/mantis" "${INSTALL_DIR}/${BINARY}"
else
  echo "Installing to ${INSTALL_DIR} (requires sudo)..."
  sudo mv "${TMP}/mantis" "${INSTALL_DIR}/${BINARY}"
fi

chmod +x "${INSTALL_DIR}/${BINARY}"

# ── Verify ────────────────────────────────────────────────────────────────────
echo ""
echo "✓ Mantis ${VERSION} installed to ${INSTALL_DIR}/${BINARY}"
echo ""
"${INSTALL_DIR}/${BINARY}" --help | head -6
echo ""
echo "Get started:"
echo "  cd your-project"
echo "  mantis init --lang ts"
echo "  mantis tui"
