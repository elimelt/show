#!/usr/bin/env bash
# show installer (Linux). Author: Elijah Melton
set -euo pipefail

APP=show
REPO=elimelt/show
OS=linux

need() { command -v "$1" >/dev/null 2>&1 || { echo "error: $1 is required" >&2; exit 1; }; }
need curl; need tar; need install

arch() {
  case "$(uname -m)" in
    x86_64) echo amd64 ;;
    arm64|aarch64) echo arm64 ;;
    *) echo "unsupported" ;;
  esac
}

ARCH=$(arch)
if [ "$ARCH" = unsupported ]; then
  echo "error: unsupported CPU: $(uname -m)" >&2; exit 1
fi

PREFIX=${PREFIX:-/usr/local}
DESTBIN="$PREFIX/bin"
DESTMAN="$PREFIX/share/man/man1"
VERSION=${VERSION:-}

if [ -z "${VERSION}" ]; then
  VERSION=$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" | sed -n 's#.*/tag/\(.*\)$#\1#p')
  [ -n "$VERSION" ] || VERSION=latest
fi

tmp=$(mktemp -d 2>/dev/null || mktemp -d -t show)
trap 'rm -rf "$tmp"' EXIT

fetch_and_install() {
  local ver="$1" os="$2" arch="$3"; shift || true
  local name_with_v="${APP}_${ver}_${os}_${arch}.tar.gz"
  local name_no_v="${APP}_${ver#v}_${os}_${arch}.tar.gz"
  local url1="https://github.com/$REPO/releases/download/${ver}/${name_with_v}"
  local url2="https://github.com/$REPO/releases/download/${ver}/${name_no_v}"

  echo "Fetching ${ver} for ${os}/${arch}..."
  if ! curl -fL "$url1" -o "$tmp/$name_with_v"; then
    curl -fL "$url2" -o "$tmp/$name_no_v" || return 1
    tar -xzf "$tmp/$name_no_v" -C "$tmp"
  else
    tar -xzf "$tmp/$name_with_v" -C "$tmp"
  fi

  local bin
  bin=$(find "$tmp" -type f -name "$APP" -perm -u+x | head -n1 || true)
  [ -n "$bin" ] || return 1

  local man
  man=$(find "$tmp" -type f -name 'show.1' | head -n1 || true)

  mkdir -p "$DESTBIN"
  install -m 0755 "$bin" "$DESTBIN/$APP"
  if [ -n "$man" ]; then
    mkdir -p "$DESTMAN" && install -m 0644 "$man" "$DESTMAN/show.1"
  fi
  echo "Installed: $DESTBIN/$APP"
}

if ! fetch_and_install "$VERSION" "$OS" "$ARCH"; then
  echo "Prebuilt archive not found; attempting go install..."
  if command -v go >/dev/null 2>&1; then
    if [ "$VERSION" = latest ]; then
      GO_VER=latest
      RAW_REF=main
    else
      GO_VER="$VERSION"
      RAW_REF="$VERSION"
    fi
    GOBIN="$DESTBIN" go install "github.com/$REPO/cmd/$APP@${GO_VER}"
    mkdir -p "$DESTMAN"
    curl -fsSL "https://raw.githubusercontent.com/$REPO/${RAW_REF}/man/show.1" -o "$DESTMAN/show.1" || true
    echo "Installed via go install: $DESTBIN/$APP"
  else
    echo "error: neither prebuilt archive nor Go toolchain available" >&2
    exit 1
  fi
fi

