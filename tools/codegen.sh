#!/bin/sh
# AGROGRAM contract codegen: the single implementation behind every "generate
# the contracts" target (back/Makefile and each services/*/Makefile).
#
# It needs curl and network access only. The pinned buf release binary is
# downloaded into a user cache OUTSIDE the repo, verified against the release's
# sha256.txt, and reused on every later run. `buf generate` then runs from
# back/ with back/buf.gen.yaml (remote BSR plugins), and back/gen/go/go.mod is
# written from tools/gen-go.mod.tmpl so the generated Go code is a module.
#
# Usage: back/tools/codegen.sh [command]   (runs from any directory)
#
#   generate    resolve buf, run `buf generate`, write gen/go/go.mod (default)
#   go-mod      only write gen/go/go.mod, for callers that ran buf themselves
#               (back/'s Docker-based `make proto-gen`)
#   tools       resolve buf and print its version
#   buf-path    resolve buf and print its path
#
# Environment:
#   BUF_VERSION  buf release to use; default: the pin in tools/buf.version
#   TOOLS_DIR    cache directory for the binary, outside the repo
#                default: ${XDG_CACHE_HOME:-$HOME/.cache}/agrogram/bin
#   BUF          path to an existing buf binary; skips the download entirely
#   BUF_TOKEN    read by buf itself; authenticates BSR access (rate limits)

set -eu

TOOLS_SRC="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
BACK_DIR="$(CDPATH= cd -- "$TOOLS_SRC/.." && pwd)"
VERSION_FILE="$TOOLS_SRC/buf.version"
TEMPLATE="$TOOLS_SRC/gen-go.mod.tmpl"
GEN_GO_DIR="$BACK_DIR/gen/go"

# Resolved by resolve_buf.
BUF_BIN=""

die() {
	echo "codegen: $*" >&2
	exit 1
}

note() {
	echo "codegen: $*" >&2
}

pinned_version() {
	if [ -n "${BUF_VERSION:-}" ]; then
		echo "$BUF_VERSION"
		return
	fi
	[ -f "$VERSION_FILE" ] || die "missing $VERSION_FILE; set BUF_VERSION to override"
	version_pin="$(tr -d ' \t\r\n' <"$VERSION_FILE")"
	[ -n "$version_pin" ] || die "$VERSION_FILE is empty; set BUF_VERSION to override"
	echo "$version_pin"
}

# Release assets are buf-<uname -s>-<arch>: Linux-x86_64, Linux-aarch64,
# Darwin-x86_64, Darwin-arm64.
release_asset() {
	case "$(uname -s)-$(uname -m)" in
	Linux-x86_64 | Linux-amd64) echo "buf-Linux-x86_64" ;;
	Linux-aarch64 | Linux-arm64) echo "buf-Linux-aarch64" ;;
	Darwin-x86_64 | Darwin-amd64) echo "buf-Darwin-x86_64" ;;
	Darwin-arm64 | Darwin-aarch64) echo "buf-Darwin-arm64" ;;
	*) die "no buf release asset for $(uname -s)/$(uname -m); set BUF to a buf binary" ;;
	esac
}

cache_dir() {
	if [ -n "${TOOLS_DIR:-}" ]; then
		echo "$TOOLS_DIR"
	elif [ -n "${XDG_CACHE_HOME:-}" ]; then
		echo "$XDG_CACHE_HOME/agrogram/bin"
	elif [ -n "${HOME:-}" ]; then
		echo "$HOME/.cache/agrogram/bin"
	else
		die "none of TOOLS_DIR, XDG_CACHE_HOME or HOME is set; nowhere to cache buf"
	fi
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		die "neither sha256sum nor shasum is available; cannot verify the download"
	fi
}

# download_buf <version> <cache dir> <destination>
download_buf() {
	download_version="$1"
	download_dir="$2"
	download_dest="$3"
	asset="$(release_asset)"
	url="https://github.com/bufbuild/buf/releases/download/v$download_version"

	command -v curl >/dev/null 2>&1 || die "curl is required to download buf $download_version"
	mkdir -p "$download_dir" || die "cannot create $download_dir"
	tmp="$(mktemp -d "$download_dir/.buf-download.XXXXXX")" ||
		die "cannot create a temporary directory in $download_dir"
	trap 'rm -rf "$tmp"' EXIT INT TERM

	note "downloading buf $download_version ($asset) into $download_dir"
	curl -fsSL --retry 3 -o "$tmp/buf" "$url/$asset" ||
		die "cannot download $url/$asset"
	curl -fsSL --retry 3 -o "$tmp/sha256.txt" "$url/sha256.txt" ||
		die "cannot download $url/sha256.txt"

	want="$(awk -v f="$asset" '$2 == f {print $1}' "$tmp/sha256.txt")"
	[ -n "$want" ] || die "sha256.txt of buf v$download_version has no entry for $asset"
	got="$(sha256_of "$tmp/buf")"
	[ "$got" = "$want" ] || die "checksum mismatch for $asset: got $got, want $want"

	chmod +x "$tmp/buf" || die "cannot make $tmp/buf executable"
	# Atomic install: other runs see either no binary or the verified one.
	mv "$tmp/buf" "$download_dest" || die "cannot install $download_dest"
	rm -rf "$tmp"
	trap - EXIT INT TERM
	note "buf $download_version OK (sha256 $got)"
}

# Sets BUF_BIN, downloading the pinned release once when it is not cached yet.
resolve_buf() {
	if [ -n "${BUF:-}" ]; then
		[ -x "$BUF" ] || die "BUF=$BUF is not an executable file"
		BUF_BIN="$BUF"
		return
	fi
	version="$(pinned_version)"
	dir="$(cache_dir)"
	BUF_BIN="$dir/buf-$version"
	[ -x "$BUF_BIN" ] || download_buf "$version" "$dir" "$BUF_BIN"
}

write_go_mod() {
	[ -f "$TEMPLATE" ] || die "missing $TEMPLATE"
	[ -d "$GEN_GO_DIR" ] || die "$GEN_GO_DIR does not exist; generate the code first"
	cp "$TEMPLATE" "$GEN_GO_DIR/go.mod" || die "cannot write $GEN_GO_DIR/go.mod"
	module="$(awk '$1 == "module" {print $2; exit}' "$TEMPLATE")"
	echo "codegen: wrote back/gen/go/go.mod (module $module)"
}

run_generate() {
	resolve_buf
	echo "codegen: $("$BUF_BIN" --version 2>/dev/null || die "cannot run $BUF_BIN") from $BUF_BIN"
	(cd "$BACK_DIR" && "$BUF_BIN" generate) || die "buf generate failed"
	write_go_mod
}

case "${1:-generate}" in
generate) run_generate ;;
go-mod) write_go_mod ;;
tools)
	resolve_buf
	"$BUF_BIN" --version
	echo "$BUF_BIN"
	;;
buf-path)
	resolve_buf
	echo "$BUF_BIN"
	;;
-h | --help | help)
	# The header comment block, without the shebang and the leading "# ".
	awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "$0"
	;;
*) die "unknown command '$1'; try: generate, go-mod, tools, buf-path, help" ;;
esac
