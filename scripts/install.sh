#!/bin/sh
set -eu

repo_url="https://github.com/MMinasyan/lightcode"
asset="lightcode-linux-amd64.tar.gz"
binary_name="lightcode"

usage() {
    printf '%s\n' \
'Install Lightcode for Linux amd64.

Usage:
  sh install.sh
  LIGHTCODE_INSTALL_DIR=/path/to/bin sh install.sh
  LIGHTCODE_VERSION=vX.Y.Z sh install.sh

Environment:
  LIGHTCODE_INSTALL_DIR   Install directory. Default: /usr/local/bin
  LIGHTCODE_VERSION       Release tag to install. Default: latest release
  LIGHTCODE_RELEASE_BASE  Release asset base URL for tests

Downloads the release tarball, verifies SHA256SUMS, installs supported Linux
runtime packages when needed, and writes lightcode into the target directory.'
}

log() {
    printf '%s\n' "$*"
}

fail() {
    printf 'lightcode installer: %s\n' "$*" >&2
    exit 1
}

have() {
    command -v "$1" >/dev/null 2>&1
}

require_cmd() {
    have "$1" || fail "required command not found: $1"
}

as_root() {
    if [ "$(id -u)" = 0 ]; then
        "$@"
    else
        require_cmd sudo
        sudo "$@"
    fi
}

download() {
    url=$1
    dest=$2
    if have curl; then
        curl -fsSL "$url" -o "$dest"
    elif have wget; then
        wget -qO "$dest" "$url"
    else
        fail "required command not found: curl or wget"
    fi
}

is_path_on_path() {
    dir=$1
    old_ifs=$IFS
    IFS=:
    for entry in ${PATH:-}; do
        if [ "$entry" = "$dir" ]; then
            IFS=$old_ifs
            return 0
        fi
    done
    IFS=$old_ifs
    return 1
}

install_runtime_deps() {
    if [ -r /etc/os-release ]; then
        # shellcheck disable=SC1091
        . /etc/os-release
        distro_id=${ID:-}
        distro_like=${ID_LIKE:-}
    else
        distro_id=
        distro_like=
    fi

    case " $distro_id $distro_like " in
        *" debian "*|*" ubuntu "*)
            require_cmd apt-get
            log "Installing WebKitGTK/GTK runtime packages..."
            as_root apt-get update
            as_root apt-get install -y libwebkit2gtk-4.1-0 libgtk-3-0
            ;;
        *" fedora "*|*" rhel "*)
            require_cmd dnf
            log "Installing WebKitGTK/GTK runtime packages..."
            as_root dnf install -y webkit2gtk4.1 gtk3
            ;;
        *)
            log "Automatic runtime package installation is not supported for this Linux distribution."
            log "Install packages that provide the missing sonames above and run this installer again."
            return 1
            ;;
    esac
}

required_sonames='libwebkit2gtk-4.1.so.0
libjavascriptcoregtk-4.1.so.0
libgtk-3.so.0
libgdk-3.so.0
libsoup-3.0.so.0'

append_missing_from_ldd() {
    out=$1
    missing_file=$2
    while IFS= read -r line; do
        case "$line" in
            *"=> not found"*)
                set -- $line
                [ -n "${1:-}" ] && printf '%s\n' "$1" >>"$missing_file"
                ;;
        esac
    done <"$out"
}

print_unique_missing() {
    file=$1
    seen=" "
    while IFS= read -r soname; do
        [ -n "$soname" ] || continue
        case "$seen" in
            *" $soname "*) ;;
            *)
                log "  $soname"
                seen="$seen$soname "
                ;;
        esac
    done <"$file"
}

collect_missing_runtime_deps() {
    bin=$1
    missing_file=$2

    require_cmd ldd

    ldd_out=$tmpdir/ldd.out
    if ! ldd "$bin" >"$ldd_out" 2>&1; then
        while IFS= read -r line; do
            printf '%s\n' "$line" >&2
        done <"$ldd_out"
        fail "could not inspect runtime dependencies with ldd"
    fi

    : >"$missing_file"
    append_missing_from_ldd "$ldd_out" "$missing_file"

    if have ldconfig; then
        ldconfig_out=$tmpdir/ldconfig.out
        if ldconfig -p >"$ldconfig_out" 2>/dev/null; then
            printf '%s\n' "$required_sonames" | while IFS= read -r soname; do
                [ -n "$soname" ] || continue
                if grep -F -q "$soname" "$ldd_out"; then
                    continue
                fi
                if ! grep -F -q "$soname" "$ldconfig_out"; then
                    printf '%s\n' "$soname"
                fi
            done >>"$missing_file"
        fi
    fi

    [ ! -s "$missing_file" ]
}

print_missing_runtime_deps() {
    missing=$1
    if [ -s "$missing" ]; then
        log "Missing required runtime libraries:"
        print_unique_missing "$missing"
    fi
}

ensure_runtime_deps() {
    bin=$1
    missing=$tmpdir/missing.out

    if collect_missing_runtime_deps "$bin" "$missing"; then
        return 0
    fi

    print_missing_runtime_deps "$missing"
    install_runtime_deps || exit 1

    if collect_missing_runtime_deps "$bin" "$missing"; then
        return 0
    fi

    print_missing_runtime_deps "$missing"
    fail "runtime dependencies are still missing after package installation"
}

case "${1:-}" in
    --help|-h)
        usage
        exit 0
        ;;
    "")
        ;;
    *)
        fail "unknown argument: $1"
        ;;
esac

os=$(uname -s)
arch=$(uname -m)
case "$os" in
    Linux) ;;
    *) fail "unsupported operating system: $os (Linux required)" ;;
esac
case "$arch" in
    x86_64|amd64) ;;
    *) fail "unsupported architecture: $arch (linux amd64 required)" ;;
esac

for cmd in uname mktemp tar gzip chmod mkdir mv rm cp grep sha256sum ldd id; do
    require_cmd "$cmd"
done
if ! have curl && ! have wget; then
    fail "required command not found: curl or wget"
fi

install_dir=${LIGHTCODE_INSTALL_DIR:-/usr/local/bin}
release_base=${LIGHTCODE_RELEASE_BASE:-}
if [ -z "$release_base" ]; then
    if [ -n "${LIGHTCODE_VERSION:-}" ]; then
        release_base="$repo_url/releases/download/$LIGHTCODE_VERSION"
    else
        release_base="$repo_url/releases/latest/download"
    fi
fi
release_base=${release_base%/}

tmpdir=$(mktemp -d)
cleanup() {
    rm -rf "$tmpdir"
}
trap cleanup EXIT HUP INT TERM

tarball=$tmpdir/$asset
checksums=$tmpdir/SHA256SUMS

download "$release_base/$asset" "$tarball" || fail "download failed: $asset"
download "$release_base/SHA256SUMS" "$checksums" || fail "download failed: SHA256SUMS"

(
    cd "$tmpdir"
    checksum_line=
    while IFS= read -r line; do
        set -- $line
        [ "${2:-}" = "$asset" ] || continue
        checksum_line=$line
        break
    done <SHA256SUMS
    [ -n "$checksum_line" ] || fail "SHA256SUMS has no entry for $asset"
    printf '%s\n' "$checksum_line" | sha256sum -c - >/dev/null
) || fail "checksum verification failed for $asset"

tar -xzf "$tarball" -C "$tmpdir" || fail "could not extract $asset"
extracted=$tmpdir/$binary_name
[ -f "$extracted" ] || fail "archive did not contain $binary_name"
chmod 755 "$extracted" || fail "could not mark $binary_name executable"

ensure_runtime_deps "$extracted"

mkdir -p "$install_dir" 2>/dev/null || as_root mkdir -p "$install_dir" || fail "could not create install directory: $install_dir"
[ -d "$install_dir" ] || fail "install target is not a directory: $install_dir"

if [ -w "$install_dir" ]; then
    tmp_install=$install_dir/.lightcode.tmp.$$
    rm -f "$tmp_install"
    cp "$extracted" "$tmp_install" || fail "could not copy binary into $install_dir"
    chmod 755 "$tmp_install" || {
        rm -f "$tmp_install"
        fail "could not set executable bit on temporary install"
    }
    mv "$tmp_install" "$install_dir/$binary_name" || {
        rm -f "$tmp_install"
        fail "could not move binary into place"
    }
else
    tmp_install=$install_dir/.lightcode.tmp.$$
    as_root rm -f "$tmp_install"
    as_root cp "$extracted" "$tmp_install" || {
        as_root rm -f "$tmp_install"
        fail "could not copy binary into $install_dir"
    }
    as_root chmod 755 "$tmp_install" || {
        as_root rm -f "$tmp_install"
        fail "could not set executable bit on temporary install"
    }
    as_root mv "$tmp_install" "$install_dir/$binary_name" || {
        as_root rm -f "$tmp_install"
        fail "could not move binary into place"
    }
fi

log "Installed Lightcode to $install_dir/$binary_name"
if ! is_path_on_path "$install_dir"; then
    log "Add $install_dir to PATH to run lightcode from any shell."
fi
log "Run: lightcode"
log "Or:  lightcode cli"
