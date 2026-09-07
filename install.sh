#!/usr/bin/env sh
# Install the ops-repo MCP server binary.
#
#   curl -fsSL https://raw.githubusercontent.com/nice-pink/ops-repo-mcp/main/install.sh | sh
#
# Environment:
#   VERSION       release tag to install (default: latest, e.g. v0.1.0)
#   INSTALL_DIR   target directory (default: /usr/local/bin if writable, else $HOME/.local/bin)
#   GITHUB_TOKEN  optional, sent only to the GitHub API to lift the rate limit
#   SKIP_CHECKSUM set to 1 to install without verifying the SHA-256 (not recommended)

set -eu

REPO="nice-pink/ops-repo-mcp"
BIN="mcp-server"
TAG_PREFIX="v"
MAX_PAGES=5

die() { printf 'error: %s\n' "$*" >&2; exit 1; }
info() { printf '%s\n' "$*" >&2; }

need() { command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"; }

need uname
need tar
need mktemp
need awk
need grep

if command -v curl >/dev/null 2>&1; then
  DL="curl"
elif command -v wget >/dev/null 2>&1; then
  DL="wget"
else
  die "need curl or wget"
fi

# fetch <url> <dest> — no credentials; asset downloads redirect to a
# different host, and the token has no business travelling there.
fetch() {
  if [ "$DL" = "curl" ]; then
    curl -fsSL -o "$2" "$1"
  else
    wget -qO "$2" "$1"
  fi
}

# fetch_api <url> <dest> — GitHub API only. Sends GITHUB_TOKEN if set, purely
# to lift the unauthenticated rate limit.
fetch_api() {
  if [ -z "${GITHUB_TOKEN:-}" ]; then
    fetch "$1" "$2"
  elif [ "$DL" = "curl" ]; then
    curl -fsSL -H "Authorization: Bearer ${GITHUB_TOKEN}" -o "$2" "$1"
  else
    wget -qO "$2" --header="Authorization: Bearer ${GITHUB_TOKEN}" "$1"
  fi
}

# ---- platform detection ----------------------------------------------------

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  darwin) ;;
  linux)  ;;
  *) die "unsupported OS: $os (supported: darwin, linux)" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64)  arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) die "unsupported architecture: $arch (supported: amd64, arm64)" ;;
esac

asset="${BIN}_${os}_${arch}.tar.gz"

# ---- version resolution ----------------------------------------------------

tmp=$(mktemp -d)
staged=""

# A single 'trap ... EXIT INT TERM' handler does not exit: POSIX sh runs it and
# resumes at the interruption point, leaving the script running with $tmp
# already deleted. INT/TERM therefore get their own handlers that exit.
cleanup() { rm -rf "$tmp"; [ -n "$staged" ] && rm -f "$staged"; return 0; }
trap 'cleanup' EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

version="${VERSION:-}"
if [ -z "$version" ]; then
  info "resolving latest ${TAG_PREFIX}* release..."
  # List releases (newest first), drop drafts and prereleases, and take the
  # first ${TAG_PREFIX}* tag. /releases/latest is not used: it ignores the
  # draft/prerelease distinction we care about here, and paging keeps this
  # correct if the listing ever carries tags from another channel.
  page=1
  scanned=0
  while [ "$page" -le "$MAX_PAGES" ]; do
    fetch_api "https://api.github.com/repos/${REPO}/releases?per_page=100&page=${page}" \
      "$tmp/releases.json" || die "could not query GitHub releases API"

    # No entries left: we have walked past the oldest release.
    grep -q '"tag_name"' "$tmp/releases.json" || break
    scanned=$((scanned + 1))

    # The API pretty-prints, so a release object spans many lines and a
    # line-wise grep cannot tell which record a "draft": true belongs to.
    # Track record boundaries instead: top-level objects open at two spaces
    # of indent and their own fields sit at four, so nested author/assets
    # objects can't be mistaken for a release.
    version=$(awk -v prefix="^${TAG_PREFIX}" '
      /^  \{/ { tag = ""; bad = 0 }
      /^    "tag_name":/ {
        tag = $0
        sub(/^[^:]*:[[:space:]]*"/, "", tag)
        sub(/".*$/, "", tag)
      }
      /^    "draft":[[:space:]]*true/      { bad = 1 }
      /^    "prerelease":[[:space:]]*true/ { bad = 1 }
      /^  \}/ { if (!bad && tag ~ prefix) { print tag; exit } }
    ' "$tmp/releases.json") || true
    [ -z "$version" ] || break

    page=$((page + 1))
  done
  [ -n "$version" ] || die "no published ${TAG_PREFIX}* release found (searched ${scanned} page(s) of releases); set VERSION=<tag> explicitly"
fi

base="https://github.com/${REPO}/releases/download/${version}"

# ---- download and verify ---------------------------------------------------

info "downloading ${asset} (${version})..."
fetch "${base}/${asset}" "$tmp/$asset" || die "download failed: ${base}/${asset}"

if [ "${SKIP_CHECKSUM:-0}" = "1" ]; then
  info "warning: SKIP_CHECKSUM=1, installing without verification"
else
  fetch "${base}/checksums.txt" "$tmp/checksums.txt" \
    || die "checksums.txt missing from release ${version}; re-run with SKIP_CHECKSUM=1 to install anyway"

  if command -v sha256sum >/dev/null 2>&1; then
    sum=$(sha256sum "$tmp/$asset" | cut -d' ' -f1)
  elif command -v shasum >/dev/null 2>&1; then
    sum=$(shasum -a 256 "$tmp/$asset" | cut -d' ' -f1)
  else
    die "need sha256sum or shasum to verify the download; re-run with SKIP_CHECKSUM=1 to install anyway"
  fi

  want=$(grep -F " ${asset}" "$tmp/checksums.txt" | cut -d' ' -f1 | head -n1) || true
  [ -n "$want" ] || die "checksums.txt has no entry for ${asset}"
  [ "$sum" = "$want" ] || die "checksum mismatch for ${asset}: got ${sum}, expected ${want}"
  info "checksum ok"
fi

tar -xzf "$tmp/$asset" -C "$tmp"
[ -f "$tmp/$BIN" ] || die "archive did not contain '${BIN}'"
chmod +x "$tmp/$BIN"

# ---- install ---------------------------------------------------------------

dir="${INSTALL_DIR:-}"
if [ -z "$dir" ]; then
  if [ -w /usr/local/bin ]; then
    dir="/usr/local/bin"
  else
    dir="${HOME}/.local/bin"
  fi
fi

mkdir -p "$dir" || die "cannot create ${dir}; set INSTALL_DIR to a writable path"
[ -w "$dir" ] || die "${dir} is not writable; set INSTALL_DIR to a writable path or re-run with sudo"

# Stage inside the target directory, then rename: a plain mv across
# filesystems copies in place, which fails with ETXTBSY if an MCP client is
# currently running the old binary. A same-directory rename replaces it.
# mktemp creates an O_EXCL regular file, so a pre-planted symlink at the
# staging path cannot redirect the write. A predictable ".${BIN}.$$" plus cp
# would follow such a symlink — and the README's sudo recipe makes that a
# root-owned write outside "$dir".
staged=$(mktemp "${dir}/.${BIN}.XXXXXX") || die "cannot write to ${dir}"
cat "$tmp/$BIN" > "$staged" || die "cannot write to ${dir}"
chmod 0755 "$staged"
mv -f "$staged" "$dir/$BIN" || die "cannot replace ${dir}/${BIN}"
staged=""

target="$dir/$BIN"
info ""
info "installed ${BIN} ${version} -> ${target}"
"$target" --version >&2 || true

case ":${PATH}:" in
  *":${dir}:"*) ;;
  *) info ""
     info "note: ${dir} is not on your PATH; add it with:"
     info "  export PATH=\"${dir}:\$PATH\"" ;;
esac

info ""
info "MCP client config (e.g. .mcp.json):"
cat >&2 <<EOF
{
  "mcpServers": {
    "deploy-promote": {
      "command": "${target}",
      "args": [],
      "env": {
        "MCP_OPS_REPO_PATH": "/absolute/path/to/your/ops-repo",
        "MCP_ENV_ALLOWLIST": "dev,staging,prod",
        "DS_BASE": "base/apps",
        "DS_PATH_SCHEME": "{base}/{app}/{env}",
        "DS_IMAGE_FILE_NAME": "deployment.yaml",
        "DS_SRC_ENV": "dev"
      }
    }
  }
}
EOF
