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

assert_mode() {
    file=$1
    expected=$2
    actual=$(stat -c '%a' "$file")
    [ "$actual" = "$expected" ] || fail "unexpected mode on $file: $actual (want $expected)"
}

assert_no_staging_residue() {
    dir=$1
    for candidate in "$dir"/.melta.tmp.*; do
        if [ -e "$candidate" ]; then
            fail "temporary staging file left behind: $candidate"
        fi
    done
}

make_release() {
    dir=$1
    binary_content=${2:-'#!/bin/sh
exit 0
'}
    mkdir -p "$dir/pkg"
    printf '%s' "$binary_content" >"$dir/pkg/melta"
    chmod 755 "$dir/pkg/melta"
    (cd "$dir/pkg" && tar -czf "$dir/melta-linux-amd64.tar.gz" melta)
    (cd "$dir" && sha256sum install.sh melta-linux-amd64.tar.gz >SHA256SUMS)
}

make_shims() {
    dir=$1
    mode=$2
    mkdir -p "$dir"
    cat >"$dir/ldd" <<'EOF'
#!/bin/sh
case "${MELTA_TEST_LDD_MODE:-ok}" in
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
    if [ -n "${MELTA_TEST_DEPS_MARKER:-}" ] && [ -f "$MELTA_TEST_DEPS_MARKER" ]; then
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

    host_cp=$(command -v cp) || fail "missing host command for shim: cp"
    host_mv=$(command -v mv) || fail "missing host command for shim: mv"

    cat >"$dir/cp" <<EOF
#!/bin/sh
if [ "\${MELTA_TEST_CP_FAIL:-}" = 1 ]; then
  for dest do :; done
  printf '%s' partial-write >"\$dest"
  exit 1
fi
exec "$host_cp" "\$@"
EOF
    chmod 755 "$dir/cp"

    cat >"$dir/mv" <<EOF
#!/bin/sh
if [ "\${MELTA_TEST_MV_FAIL:-}" = 1 ]; then
  exit 1
fi
exec "$host_mv" "\$@"
EOF
    chmod 755 "$dir/mv"

    cat >"$dir/sudo" <<'EOF'
#!/bin/sh
case "${1:-}" in
  cp)
    dest=$3
    dir=${dest%/*}
    if [ "${MELTA_TEST_SUDO_CP_FAIL:-}" = 1 ]; then
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
    [ -n "${MELTA_TEST_DEPS_MARKER:-}" ] && : >"$MELTA_TEST_DEPS_MARKER"
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
    [ -n "${MELTA_TEST_DEPS_MARKER:-}" ] && : >"$MELTA_TEST_DEPS_MARKER"
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
        MELTA_RELEASE_BASE="file://$release_dir"
        MELTA_INSTALL_DIR="$install_dir"
        HOME="$home_dir"
        export PATH MELTA_RELEASE_BASE MELTA_INSTALL_DIR HOME
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

# Runs the repository make install recipe from a temporary source tree. The
# root makefile supplies the recipe under test; a temporary companion file,
# read after it, overrides only the build prerequisite with a no-op. An empty
# prefix argument lets the makefile choose its default under the temporary HOME.
run_make_install() {
    out=$1
    prefix=$2
    shift 2
    if [ -n "$prefix" ]; then
        set -- "$@" make -f "$root_dir/Makefile" -f "$make_companion" "PREFIX=$prefix" install
    else
        set -- "$@" make -f "$root_dir/Makefile" -f "$make_companion" install
    fi
    (
        PATH="$shim_dir:$PATH"
        HOME="$make_home"
        export PATH HOME
        cd "$make_source_tree"
        "$@"
    ) >"$out" 2>&1
}

run_make_expect_success() {
    out=$1
    prefix=$2
    shift 2
    if ! run_make_install "$out" "$prefix" "$@"; then
        cat "$out" >&2 || true
        fail "expected make install success"
    fi
}

run_make_expect_failure() {
    out=$1
    prefix=$2
    shift 2
    if run_make_install "$out" "$prefix" "$@"; then
        cat "$out" >&2 || true
        fail "expected make install failure"
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
run_expect_success "$test_root/auto-deps.out" env MELTA_TEST_LDD_MODE=auto-install MELTA_TEST_DEPS_MARKER="$deps_marker"
[ -f "$deps_marker" ] || fail "runtime package installer was not invoked"
[ -x "$install_dir/melta" ] || fail "installed melta is missing or not executable after dependency install"
assert_file_contains "$test_root/auto-deps.out" "Installing WebKitGTK/GTK runtime packages"

rm -rf "$release_dir"
mkdir -p "$release_dir"
cp "$installer" "$release_dir/install.sh"
make_release "$release_dir"

printf 'old-binary' >"$install_dir/melta"
chmod 700 "$install_dir/melta"
run_expect_success "$test_root/success.out" env MELTA_TEST_LDD_MODE=ok
[ -x "$install_dir/melta" ] || fail "installed melta is missing or not executable"
cmp "$install_dir/melta" "$release_dir/pkg/melta" >/dev/null || fail "installed binary does not match the release binary"
assert_mode "$install_dir/melta" 755
assert_no_staging_residue "$install_dir"
assert_file_contains "$test_root/success.out" "Installed Melta"
[ ! -e "$home_dir/.melta/config.json" ] || fail "installer wrote config.json"
[ ! -e "$home_dir/.melta/.env" ] || fail "installer wrote .env"

privileged_dir=$test_root/privileged-bin
mkdir -p "$privileged_dir"
printf 'old-binary' >"$privileged_dir/melta"
chmod 555 "$privileged_dir"
install_dir=$privileged_dir
run_expect_failure "$test_root/privileged-copy-fail.out" env MELTA_TEST_LDD_MODE=ok MELTA_TEST_SUDO_CP_FAIL=1
chmod 755 "$privileged_dir"
assert_equal_file "$privileged_dir/melta" "old-binary"
install_dir=$test_root/bin

printf 'old-binary' >"$install_dir/melta"
chmod 700 "$install_dir/melta"
run_expect_failure "$test_root/writable-copy-fail.out" env MELTA_TEST_LDD_MODE=ok MELTA_TEST_CP_FAIL=1
assert_equal_file "$install_dir/melta" "old-binary"
assert_mode "$install_dir/melta" 700
assert_file_contains "$test_root/writable-copy-fail.out" "could not copy binary into"
assert_no_staging_residue "$install_dir"

printf 'old-binary' >"$install_dir/melta"
chmod 700 "$install_dir/melta"
run_expect_failure "$test_root/writable-move-fail.out" env MELTA_TEST_LDD_MODE=ok MELTA_TEST_MV_FAIL=1
assert_equal_file "$install_dir/melta" "old-binary"
assert_mode "$install_dir/melta" 700
assert_file_contains "$test_root/writable-move-fail.out" "could not move binary into place"
assert_no_staging_residue "$install_dir"

printf 'old-binary' >"$install_dir/melta"
echo "bad checksum melta-linux-amd64.tar.gz" >"$release_dir/SHA256SUMS"
run_expect_failure "$test_root/checksum.out" env MELTA_TEST_LDD_MODE=ok
assert_equal_file "$install_dir/melta" "old-binary"
assert_file_contains "$test_root/checksum.out" "checksum verification failed"

rm -rf "$release_dir"
mkdir -p "$release_dir/pkg"
cp "$installer" "$release_dir/install.sh"
printf 'wrong binary' >"$release_dir/pkg/lightcode"
(cd "$release_dir/pkg" && tar -czf "$release_dir/melta-linux-amd64.tar.gz" lightcode)
(cd "$release_dir" && sha256sum install.sh melta-linux-amd64.tar.gz >SHA256SUMS)
printf 'old-binary' >"$install_dir/melta"
chmod 700 "$install_dir/melta"
run_expect_failure "$test_root/wrong-member.out" env MELTA_TEST_LDD_MODE=ok
assert_file_contains "$test_root/wrong-member.out" "archive did not contain melta"
assert_equal_file "$install_dir/melta" "old-binary"
assert_mode "$install_dir/melta" 700
assert_no_staging_residue "$install_dir"

rm -rf "$release_dir"
mkdir -p "$release_dir"
cp "$installer" "$release_dir/install.sh"
make_release "$release_dir"
printf 'old-binary' >"$install_dir/melta"
mv "$release_dir/melta-linux-amd64.tar.gz" "$release_dir/lightcode-linux-amd64.tar.gz"
(cd "$release_dir" && sha256sum install.sh lightcode-linux-amd64.tar.gz >SHA256SUMS)
run_expect_failure "$test_root/download.out" env MELTA_TEST_LDD_MODE=ok
assert_equal_file "$install_dir/melta" "old-binary"
assert_file_contains "$test_root/download.out" "download failed"

rm -rf "$release_dir"
mkdir -p "$release_dir"
cp "$installer" "$release_dir/install.sh"
make_release "$release_dir"
printf 'old-binary' >"$install_dir/melta"
run_expect_failure "$test_root/deps.out" env MELTA_TEST_LDD_MODE=missing
assert_equal_file "$install_dir/melta" "old-binary"
assert_file_contains "$test_root/deps.out" "Missing required runtime libraries"
assert_file_contains "$test_root/deps.out" "libwebkit2gtk-4.1.so.0"

isolated_dir=$test_root/isolated-no-ldd
make_isolated_path_without_ldd "$isolated_dir"
printf 'old-binary' >"$install_dir/melta"
(
    PATH="$isolated_dir"
    MELTA_RELEASE_BASE="file://$release_dir"
    MELTA_INSTALL_DIR="$install_dir"
    HOME="$home_dir"
    export PATH MELTA_RELEASE_BASE MELTA_INSTALL_DIR HOME
    "$shell_sh" "$installer"
) >"$test_root/no-ldd.out" 2>&1 && fail "expected missing ldd failure"
assert_equal_file "$install_dir/melta" "old-binary"
assert_file_contains "$test_root/no-ldd.out" "required command not found: ldd"

isolated_no_ldconfig=$test_root/isolated-no-ldconfig
make_isolated_path_without_ldconfig "$isolated_no_ldconfig"
(
    PATH="$isolated_no_ldconfig"
    MELTA_RELEASE_BASE="file://$release_dir"
    MELTA_INSTALL_DIR="$install_dir"
    HOME="$home_dir"
    export PATH MELTA_RELEASE_BASE MELTA_INSTALL_DIR HOME
    "$shell_sh" "$installer"
) >"$test_root/no-ldconfig.out" 2>&1 || {
    cat "$test_root/no-ldconfig.out" >&2 || true
    fail "expected installer success without ldconfig"
}
[ -x "$install_dir/melta" ] || fail "install without ldconfig failed"

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
printf 'old-binary' >"$install_dir/melta"
run_expect_failure "$test_root/os.out" env MELTA_TEST_LDD_MODE=ok
assert_equal_file "$install_dir/melta" "old-binary"
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
printf 'old-binary' >"$install_dir/melta"
run_expect_failure "$test_root/arch.out" env MELTA_TEST_LDD_MODE=ok
assert_equal_file "$install_dir/melta" "old-binary"
assert_file_contains "$test_root/arch.out" "unsupported architecture"

log_file=$test_root/help.out
sh "$installer" --help >"$log_file"
assert_file_contains "$log_file" "MELTA_INSTALL_DIR"

make_source_tree=$test_root/make-src
make_companion=$test_root/make-companion.mk
make_home=$test_root/make-home
mkdir -p "$make_source_tree/build/bin" "$make_home"
printf '%s' 'make-fixture-binary' >"$make_source_tree/build/bin/melta"
chmod 644 "$make_source_tree/build/bin/melta"
printf 'build:\n\t@:\n' >"$make_companion"

make_copy_fail_prefix=$test_root/make-copy-fail
mkdir -p "$make_copy_fail_prefix/bin"
printf 'old-binary' >"$make_copy_fail_prefix/bin/melta"
chmod 700 "$make_copy_fail_prefix/bin/melta"
run_make_expect_failure "$test_root/make-copy-fail.out" "$make_copy_fail_prefix" env MELTA_TEST_CP_FAIL=1
assert_equal_file "$make_copy_fail_prefix/bin/melta" "old-binary"
assert_mode "$make_copy_fail_prefix/bin/melta" 700
assert_no_staging_residue "$make_copy_fail_prefix/bin"

make_move_fail_prefix=$test_root/make-move-fail
mkdir -p "$make_move_fail_prefix/bin"
printf 'old-binary' >"$make_move_fail_prefix/bin/melta"
chmod 700 "$make_move_fail_prefix/bin/melta"
run_make_expect_failure "$test_root/make-move-fail.out" "$make_move_fail_prefix" env MELTA_TEST_MV_FAIL=1
assert_equal_file "$make_move_fail_prefix/bin/melta" "old-binary"
assert_mode "$make_move_fail_prefix/bin/melta" 700
assert_no_staging_residue "$make_move_fail_prefix/bin"

mkdir -p "$make_home/.local/bin"
printf 'old-binary' >"$make_home/.local/bin/melta"
chmod 700 "$make_home/.local/bin/melta"
run_make_expect_success "$test_root/make-default.out" ""
assert_equal_file "$make_home/.local/bin/melta" "make-fixture-binary"
assert_mode "$make_home/.local/bin/melta" 755
assert_no_staging_residue "$make_home/.local/bin"

spaced_prefix="$test_root/prefix with space"
run_make_expect_success "$test_root/make-spaced.out" "$spaced_prefix"
assert_equal_file "$spaced_prefix/bin/melta" "make-fixture-binary"
assert_mode "$spaced_prefix/bin/melta" 755
assert_no_staging_residue "$spaced_prefix/bin"

# Captured Wails argv: the real root Makefile build and install recipes run
# through a fake Wails CLI planted at the fixture HOME, which records its
# arguments. No companion file is used here, so these calls exercise the
# actual recipes, including the tagged build command and the unchanged
# version-stamping flags. The record is cleared immediately before each target
# so every assertion proves that target's own Wails call, never a previous one.
make_wails_argv=$test_root/make-wails.argv
make_wails_expected=$test_root/make-wails.expected
mkdir -p "$make_home/go/bin"
cat >"$make_home/go/bin/wails" <<EOF
#!/bin/sh
printf '%s\n' "\$@" >"$make_wails_argv"
EOF
chmod 755 "$make_home/go/bin/wails"

cat >"$make_wails_expected" <<'EOF'
build
-tags
webkit2_41
-ldflags
-X github.com/Syntheresis/melta/internal/version.Version=v1.2.3 -X github.com/Syntheresis/melta/internal/version.Commit=abc1234
EOF

run_make_real() {
    out=$1
    shift
    (
        PATH="$shim_dir:$PATH"
        HOME="$make_home"
        export PATH HOME
        cd "$make_source_tree"
        make -f "$root_dir/Makefile" "$@"
    ) >"$out" 2>&1
}

run_make_real_expect_success() {
    out=$1
    shift
    if ! run_make_real "$out" "$@"; then
        cat "$out" >&2 || true
        fail "expected real make target success"
    fi
}

assert_wails_argv() {
    [ -f "$make_wails_argv" ] || fail "Wails CLI was not invoked for this target"
    assert_equal_file "$make_wails_argv" "$(cat "$make_wails_expected")"
}

make_real_prefix=$test_root/make-real-prefix
rm -f "$make_wails_argv"
run_make_real_expect_success "$test_root/make-real-build.out" VERSION=v1.2.3 COMMIT=abc1234 build
assert_wails_argv
rm -f "$make_wails_argv"
run_make_real_expect_success "$test_root/make-real-install.out" VERSION=v1.2.3 COMMIT=abc1234 PREFIX="$make_real_prefix" install
assert_wails_argv
assert_equal_file "$make_real_prefix/bin/melta" "make-fixture-binary"
assert_mode "$make_real_prefix/bin/melta" 755
assert_no_staging_residue "$make_real_prefix/bin"

printf 'installer tests passed\n'
