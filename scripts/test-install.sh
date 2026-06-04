#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
installer=$root_dir/scripts/install.sh
shell_sh=$(command -v sh)

test_root=
cleanup() {
    if [ -n "${test_root:-}" ]; then
        rm -rf "$test_root"
    fi
}
trap cleanup EXIT HUP INT TERM

fail() {
    printf 'test-install: %s\n' "$*" >&2
    exit 1
}

assert_file_contains() {
    file=$1
    pattern=$2
    grep -q "$pattern" "$file" || {
        printf '%s\n' "--- $file ---" >&2
        cat "$file" >&2 || true
        fail "expected pattern not found: $pattern"
    }
}

assert_equal_file() {
    file=$1
    expected=$2
    actual=$(cat "$file")
    [ "$actual" = "$expected" ] || fail "unexpected content in $file: $actual"
}

make_release() {
    dir=$1
    binary_content=${2:-'#!/bin/sh
exit 0
'}
    mkdir -p "$dir/pkg"
    printf '%s' "$binary_content" >"$dir/pkg/lightcode"
    chmod 755 "$dir/pkg/lightcode"
    (cd "$dir/pkg" && tar -czf "$dir/lightcode-linux-amd64.tar.gz" lightcode)
    (cd "$dir" && sha256sum install.sh lightcode-linux-amd64.tar.gz >SHA256SUMS)
}

make_shims() {
    dir=$1
    mode=$2
    mkdir -p "$dir"
    cat >"$dir/ldd" <<'EOF'
#!/bin/sh
case "${LIGHTCODE_TEST_LDD_MODE:-ok}" in
  ok)
    cat <<'OUT'
linux-vdso.so.1 (0x0000000000000000)
libwebkit2gtk-4.1.so.0 => /lib/libwebkit2gtk-4.1.so.0 (0x0000000000000000)
libjavascriptcoregtk-4.1.so.0 => /lib/libjavascriptcoregtk-4.1.so.0 (0x0000000000000000)
libgtk-3.so.0 => /lib/libgtk-3.so.0 (0x0000000000000000)
libgdk-3.so.0 => /lib/libgdk-3.so.0 (0x0000000000000000)
libsoup-3.0.so.0 => /lib/libsoup-3.0.so.0 (0x0000000000000000)
OUT
    ;;
  missing)
    cat <<'OUT'
libwebkit2gtk-4.1.so.0 => not found
libgtk-3.so.0 => /lib/libgtk-3.so.0 (0x0000000000000000)
OUT
    ;;
  fail)
    echo "not a dynamic executable" >&2
    exit 1
    ;;
  auto-install)
    if [ -n "${LIGHTCODE_TEST_DEPS_MARKER:-}" ] && [ -f "$LIGHTCODE_TEST_DEPS_MARKER" ]; then
      cat <<'OUT'
linux-vdso.so.1 (0x0000000000000000)
libwebkit2gtk-4.1.so.0 => /lib/libwebkit2gtk-4.1.so.0 (0x0000000000000000)
libjavascriptcoregtk-4.1.so.0 => /lib/libjavascriptcoregtk-4.1.so.0 (0x0000000000000000)
libgtk-3.so.0 => /lib/libgtk-3.so.0 (0x0000000000000000)
libgdk-3.so.0 => /lib/libgdk-3.so.0 (0x0000000000000000)
libsoup-3.0.so.0 => /lib/libsoup-3.0.so.0 (0x0000000000000000)
OUT
    else
      cat <<'OUT'
libwebkit2gtk-4.1.so.0 => not found
libjavascriptcoregtk-4.1.so.0 => not found
libgtk-3.so.0 => /lib/libgtk-3.so.0 (0x0000000000000000)
OUT
    fi
    ;;
esac
EOF
    chmod 755 "$dir/ldd"

    if [ "$mode" = with-ldconfig ]; then
        cat >"$dir/ldconfig" <<'EOF'
#!/bin/sh
cat <<'OUT'
libwebkit2gtk-4.1.so.0 (libc6,x86-64) => /lib/libwebkit2gtk-4.1.so.0
libjavascriptcoregtk-4.1.so.0 (libc6,x86-64) => /lib/libjavascriptcoregtk-4.1.so.0
libgtk-3.so.0 (libc6,x86-64) => /lib/libgtk-3.so.0
libgdk-3.so.0 (libc6,x86-64) => /lib/libgdk-3.so.0
libsoup-3.0.so.0 (libc6,x86-64) => /lib/libsoup-3.0.so.0
OUT
EOF
        chmod 755 "$dir/ldconfig"
    fi

    cat >"$dir/id" <<'EOF'
#!/bin/sh
case "$1" in
  -u) echo 1000 ;;
  *) echo 1000 ;;
esac
EOF
    chmod 755 "$dir/id"

    cat >"$dir/sudo" <<'EOF'
#!/bin/sh
case "${1:-}" in
  cp)
    dest=$3
    dir=${dest%/*}
    if [ "${LIGHTCODE_TEST_SUDO_CP_FAIL:-}" = 1 ]; then
      chmod u+w "$dir"
      printf '%s' partial-write >"$dest"
      chmod u-w "$dir"
      exit 1
    fi
    chmod u+w "$dir"
    "$@"
    status=$?
    chmod u-w "$dir"
    exit "$status"
    ;;
  mv)
    dest=$3
    dir=${dest%/*}
    chmod u+w "$dir"
    "$@"
    status=$?
    chmod u-w "$dir"
    exit "$status"
    ;;
  rm)
    dest=$3
    dir=${dest%/*}
    chmod u+w "$dir"
    "$@"
    status=$?
    chmod u-w "$dir"
    exit "$status"
    ;;
esac
exec "$@"
EOF
    chmod 755 "$dir/sudo"

    cat >"$dir/apt-get" <<'EOF'
#!/bin/sh
case "$1" in
  update)
    exit 0
    ;;
  install)
    shift
    [ "$1" = "-y" ] || exit 1
    shift
    [ "$1" = "libwebkit2gtk-4.1-0" ] || exit 1
    [ "$2" = "libgtk-3-0" ] || exit 1
    [ -n "${LIGHTCODE_TEST_DEPS_MARKER:-}" ] && : >"$LIGHTCODE_TEST_DEPS_MARKER"
    exit 0
    ;;
  *)
    exit 1
    ;;
esac
EOF
    chmod 755 "$dir/apt-get"

    cat >"$dir/dnf" <<'EOF'
#!/bin/sh
case "$1" in
  install)
    shift
    [ "$1" = "-y" ] || exit 1
    shift
    [ "$1" = "webkit2gtk4.1" ] || exit 1
    [ "$2" = "gtk3" ] || exit 1
    [ -n "${LIGHTCODE_TEST_DEPS_MARKER:-}" ] && : >"$LIGHTCODE_TEST_DEPS_MARKER"
    exit 0
    ;;
  *)
    exit 1
    ;;
esac
EOF
    chmod 755 "$dir/dnf"
}

make_isolated_path_without_ldd() {
    dir=$1
    mkdir -p "$dir"
    for cmd in uname mktemp tar gzip chmod mkdir mv rm cp grep sha256sum curl id; do
        src=$(command -v "$cmd") || fail "missing host command for isolated path: $cmd"
        ln -s "$src" "$dir/$cmd"
    done
}

make_isolated_path_without_ldconfig() {
    dir=$1
    mkdir -p "$dir"
    for cmd in uname mktemp tar gzip chmod mkdir mv rm cp grep sha256sum curl id; do
        src=$(command -v "$cmd") || fail "missing host command for isolated path: $cmd"
        ln -s "$src" "$dir/$cmd"
    done
    cat >"$dir/ldd" <<'EOF'
#!/bin/sh
printf '%s\n' \
'linux-vdso.so.1 (0x0000000000000000)' \
'libwebkit2gtk-4.1.so.0 => /lib/libwebkit2gtk-4.1.so.0 (0x0000000000000000)' \
'libjavascriptcoregtk-4.1.so.0 => /lib/libjavascriptcoregtk-4.1.so.0 (0x0000000000000000)' \
'libgtk-3.so.0 => /lib/libgtk-3.so.0 (0x0000000000000000)' \
'libgdk-3.so.0 => /lib/libgdk-3.so.0 (0x0000000000000000)' \
'libsoup-3.0.so.0 => /lib/libsoup-3.0.so.0 (0x0000000000000000)'
EOF
    chmod 755 "$dir/ldd"
}

run_installer() {
    out=$1
    shift
    (
        PATH="$shim_dir:$PATH"
        LIGHTCODE_RELEASE_BASE="file://$release_dir"
        LIGHTCODE_INSTALL_DIR="$install_dir"
        HOME="$home_dir"
        export PATH LIGHTCODE_RELEASE_BASE LIGHTCODE_INSTALL_DIR HOME
        "$@" sh "$installer"
    ) >"$out" 2>&1
}

run_expect_success() {
    out=$1
    shift
    if ! run_installer "$out" "$@"; then
        cat "$out" >&2 || true
        fail "expected installer success"
    fi
}

run_expect_failure() {
    out=$1
    shift
    if run_installer "$out" "$@"; then
        cat "$out" >&2 || true
        fail "expected installer failure"
    fi
}

sh -n "$installer"

test_root=$(mktemp -d)
release_dir=$test_root/release
install_dir=$test_root/bin
home_dir=$test_root/home
shim_dir=$test_root/shims
mkdir -p "$release_dir" "$install_dir" "$home_dir"
cp "$installer" "$release_dir/install.sh"
make_shims "$shim_dir" with-ldconfig
make_release "$release_dir"

deps_marker=$test_root/deps-installed
rm -f "$deps_marker"
run_expect_success "$test_root/auto-deps.out" env LIGHTCODE_TEST_LDD_MODE=auto-install LIGHTCODE_TEST_DEPS_MARKER="$deps_marker"
[ -f "$deps_marker" ] || fail "runtime package installer was not invoked"
[ -x "$install_dir/lightcode" ] || fail "installed lightcode is missing or not executable after dependency install"
assert_file_contains "$test_root/auto-deps.out" "Installing WebKitGTK/GTK runtime packages"

rm -rf "$release_dir"
mkdir -p "$release_dir"
cp "$installer" "$release_dir/install.sh"
make_release "$release_dir"

run_expect_success "$test_root/success.out" env LIGHTCODE_TEST_LDD_MODE=ok
[ -x "$install_dir/lightcode" ] || fail "installed lightcode is missing or not executable"
assert_file_contains "$test_root/success.out" "Installed Lightcode"
[ ! -e "$home_dir/.lightcode/config.json" ] || fail "installer wrote config.json"
[ ! -e "$home_dir/.lightcode/.env" ] || fail "installer wrote .env"

privileged_dir=$test_root/privileged-bin
mkdir -p "$privileged_dir"
printf 'old-binary' >"$privileged_dir/lightcode"
chmod 555 "$privileged_dir"
install_dir=$privileged_dir
run_expect_failure "$test_root/privileged-copy-fail.out" env LIGHTCODE_TEST_LDD_MODE=ok LIGHTCODE_TEST_SUDO_CP_FAIL=1
chmod 755 "$privileged_dir"
assert_equal_file "$privileged_dir/lightcode" "old-binary"
install_dir=$test_root/bin

printf 'old-binary' >"$install_dir/lightcode"
echo "bad checksum lightcode-linux-amd64.tar.gz" >"$release_dir/SHA256SUMS"
run_expect_failure "$test_root/checksum.out" env LIGHTCODE_TEST_LDD_MODE=ok
assert_equal_file "$install_dir/lightcode" "old-binary"
assert_file_contains "$test_root/checksum.out" "checksum verification failed"

rm -rf "$release_dir"
mkdir -p "$release_dir"
cp "$installer" "$release_dir/install.sh"
make_release "$release_dir"
printf 'old-binary' >"$install_dir/lightcode"
rm -f "$release_dir/lightcode-linux-amd64.tar.gz"
run_expect_failure "$test_root/download.out" env LIGHTCODE_TEST_LDD_MODE=ok
assert_equal_file "$install_dir/lightcode" "old-binary"
assert_file_contains "$test_root/download.out" "download failed"

rm -rf "$release_dir"
mkdir -p "$release_dir"
cp "$installer" "$release_dir/install.sh"
make_release "$release_dir"
printf 'old-binary' >"$install_dir/lightcode"
run_expect_failure "$test_root/deps.out" env LIGHTCODE_TEST_LDD_MODE=missing
assert_equal_file "$install_dir/lightcode" "old-binary"
assert_file_contains "$test_root/deps.out" "Missing required runtime libraries"
assert_file_contains "$test_root/deps.out" "libwebkit2gtk-4.1.so.0"

isolated_dir=$test_root/isolated-no-ldd
make_isolated_path_without_ldd "$isolated_dir"
printf 'old-binary' >"$install_dir/lightcode"
(
    PATH="$isolated_dir"
    LIGHTCODE_RELEASE_BASE="file://$release_dir"
    LIGHTCODE_INSTALL_DIR="$install_dir"
    HOME="$home_dir"
    export PATH LIGHTCODE_RELEASE_BASE LIGHTCODE_INSTALL_DIR HOME
    "$shell_sh" "$installer"
) >"$test_root/no-ldd.out" 2>&1 && fail "expected missing ldd failure"
assert_equal_file "$install_dir/lightcode" "old-binary"
assert_file_contains "$test_root/no-ldd.out" "required command not found: ldd"

isolated_no_ldconfig=$test_root/isolated-no-ldconfig
make_isolated_path_without_ldconfig "$isolated_no_ldconfig"
(
    PATH="$isolated_no_ldconfig"
    LIGHTCODE_RELEASE_BASE="file://$release_dir"
    LIGHTCODE_INSTALL_DIR="$install_dir"
    HOME="$home_dir"
    export PATH LIGHTCODE_RELEASE_BASE LIGHTCODE_INSTALL_DIR HOME
    "$shell_sh" "$installer"
) >"$test_root/no-ldconfig.out" 2>&1 || {
    cat "$test_root/no-ldconfig.out" >&2 || true
    fail "expected installer success without ldconfig"
}
[ -x "$install_dir/lightcode" ] || fail "install without ldconfig failed"

rm -rf "$shim_dir"
mkdir -p "$shim_dir"
make_shims "$shim_dir" with-ldconfig

cat >"$shim_dir/uname" <<'EOF'
#!/bin/sh
case "$1" in
  -s) echo Darwin ;;
  -m) echo x86_64 ;;
  *) echo Darwin ;;
esac
EOF
chmod 755 "$shim_dir/uname"
printf 'old-binary' >"$install_dir/lightcode"
run_expect_failure "$test_root/os.out" env LIGHTCODE_TEST_LDD_MODE=ok
assert_equal_file "$install_dir/lightcode" "old-binary"
assert_file_contains "$test_root/os.out" "unsupported operating system"

cat >"$shim_dir/uname" <<'EOF'
#!/bin/sh
case "$1" in
  -s) echo Linux ;;
  -m) echo aarch64 ;;
  *) echo Linux ;;
esac
EOF
chmod 755 "$shim_dir/uname"
printf 'old-binary' >"$install_dir/lightcode"
run_expect_failure "$test_root/arch.out" env LIGHTCODE_TEST_LDD_MODE=ok
assert_equal_file "$install_dir/lightcode" "old-binary"
assert_file_contains "$test_root/arch.out" "unsupported architecture"

log_file=$test_root/help.out
sh "$installer" --help >"$log_file"
assert_file_contains "$log_file" "LIGHTCODE_INSTALL_DIR"

printf 'installer tests passed\n'
