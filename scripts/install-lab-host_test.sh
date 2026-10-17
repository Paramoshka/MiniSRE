#!/usr/bin/env bash
set -euo pipefail
umask 077

project_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
test_root=$(mktemp -d)
trap 'rm -rf -- "$test_root"' EXIT
root="$test_root/root"
mkdir -p "$test_root/bin" "$test_root/package" "$root/usr/bin" "$root/usr/local/sbin" \
  "$root/etc/systemd/system" "$root/etc/sudoers.d" "$root/var/lib"
real_install=$(command -v install)
real_stat=$(command -v stat)

# Rewrite only the test copy's system paths. Production has no root/path override.
sed -e "s|export PATH=.*|export PATH=$test_root/bin:/usr/sbin:/usr/bin:/sbin:/bin|" \
  -e "s|/usr/bin/|$root/usr/bin/|g" \
  -e "s|/usr/local/sbin|$root/usr/local/sbin|g" \
  -e "s|/etc/|$root/etc/|g" \
  -e "s|/var/lib/|$root/var/lib/|g" \
  "$project_dir/scripts/install-lab-host.sh" > "$test_root/installer.sh"

{
  printf '#!/bin/bash\nset -euo pipefail\n'
  printf 'test_root=%q\nreal_install=%q\nreal_stat=%q\n' "$test_root" "$real_install" "$real_stat"
  cat <<'MOCK'
root="$test_root/root"
command=${0##*/}
printf '%s %s\n' "$command" "$*" >> "$test_root/calls"
case "$command" in
  id)
    case "$*" in
      -u) cat "$test_root/uid" ;;
      '-u -- test-user') printf '1000\n' ;;
      '-u -- root') printf '0\n' ;;
      *) exit 1 ;;
    esac ;;
  uname)
    case "$1" in
      -s) printf 'Linux\n' ;;
      -m) cat "$test_root/arch" ;;
      *) exit 1 ;;
    esac ;;
  stat)
    # Tests run without root; trust fixture ownership, but preserve actual modes.
    path=${@: -1}
    mode=$("$real_stat" -c '%a' -- "$path")
    [[ $path != /tmp ]] || mode=755
    uid=0
    [[ ! ( -e $test_root/wrong-owner && $path == "$root/etc/mini-sre/lab.yaml" ) ]] || uid=1000
    printf '%s %s\n' "$uid" "$mode" ;;
  curl)
    [[ ! -e $test_root/fail-download ]] || exit 22
    output=''; url=''
    while (( $# )); do
      case "$1" in
        -o) output=$2; shift 2 ;;
        https://*) url=$1; shift ;;
        *) shift ;;
      esac
    done
    case "$url" in
      */releases/latest) printf 'https://github.com/Paramoshka/MiniSRE/releases/tag/v0.1.0' ;;
      */v0.1.0/mini-sre_linux_*.tar.gz) cp "$test_root/${url##*/}" "$output" ;;
      */v0.1.0/checksums.txt)
        if [[ -e $test_root/bad-checksum ]]; then
          printf '%064d  mini-sre_linux_amd64.tar.gz\n' 0 > "$output"
        else
          cp "$test_root/checksums.txt" "$output"
        fi ;;
      *) exit 1 ;;
    esac ;;
  docker)
    [[ -z ${DOCKER_HOST:-}${DOCKER_CONTEXT:-}${DOCKER_CONFIG:-}${HOME:-} ]] || exit 1
    case "$1" in
      info) [[ ! -e $test_root/fail-docker ]] ;;
      build) [[ ! -e $test_root/fail-build && ! ( -e $test_root/fail-frr && $* == *mini-sre-frr:local* ) ]] ;;
      image) [[ ! -e $test_root/fail-image-inspect ]] ;;
      *) exit 1 ;;
    esac ;;
  containerlab) [[ $1 == version ]] ;;
  ip) [[ $* == '-j -4 address show' ]]; printf '[]\n' ;;
  visudo)
    [[ $1 == -cf && -s $2 && ! -e $test_root/fail-sudoers ]] ;;
  systemctl)
    [[ ! ( -e $test_root/fail-timer && $* == *is-active* ) ]] ;;
  install)
    args=()
    while (( $# )); do
      case "$1" in
        -o|-g) shift 2 ;;
        *) args+=("$1"); shift ;;
      esac
    done
    "$real_install" "${args[@]}"
    destination=${args[${#args[@]}-1]}
    if [[ $destination == "$root/usr/local/sbin/mini-sre-lab.new" ]]; then
      # Preserve the actual release binary for comparison; mock only its request.
      cp "$destination" "$test_root/installed-release-binary"
      cp "$test_root/helper" "$destination"
    fi ;;
  *) printf 'Unexpected command: %s\n' "$command" >&2; exit 1 ;;
esac
MOCK
} > "$test_root/bin/mock"
chmod +x "$test_root/bin/mock"
for command in id uname stat curl visudo systemctl install; do
  ln -s mock "$test_root/bin/$command"
done
for command in docker containerlab ip; do
  ln -s "$test_root/bin/mock" "$root/usr/bin/$command"
done
ln -s /usr/bin/timeout "$root/usr/bin/timeout"
printf '0\n' > "$test_root/uid"
printf 'x86_64\n' > "$test_root/arch"
{
  printf '#!/bin/bash\nset -euo pipefail\ntest_root=%q\n' "$test_root"
  cat <<'HELPER'
if (( $# == 0 )); then
  printf 'usage: mini-sre-lab request|gc\n' >&2
  exit 1
fi
[[ $1 == request && ! -e $test_root/fail-helper ]]
read -r request
[[ $request == '{"action":"inspect"}' ]]
printf 'helper inspect\n' >> "$test_root/calls"
printf '{"labs":[]}\n'
HELPER
} > "$test_root/helper"
chmod +x "$test_root/helper"
cp "$test_root/helper" "$test_root/package/mini-sre-lab"
cp "$project_dir"/examples/mini-sre-lab{.yaml,.sudoers,-gc.service,-gc.timer} "$test_root/package/"
cp "$project_dir/examples/lab-images/Dockerfile" "$test_root/package/Dockerfile.lab"
for arch in amd64 arm64; do
  tar -C "$test_root/package" -czf "$test_root/mini-sre_linux_$arch.tar.gz" .
done
if [[ -n ${MINISRE_TEST_ARCHIVE:-} ]]; then
  cp "$MINISRE_TEST_ARCHIVE" "$test_root/mini-sre_linux_amd64.tar.gz"
fi
(cd "$test_root" && sha256sum mini-sre_linux_*.tar.gz > checksums.txt)

run_installer() {
  # Same piped stdin as curl | sudo bash; interactive settings use /dev/tty.
  cat "$test_root/installer.sh" | env DOCKER_HOST=tcp://untrusted:2375 DOCKER_CONTEXT=untrusted \
    DOCKER_CONFIG=/untrusted bash -s -- "$@" > "$test_root/output" 2>&1
}
expect_failure() {
  local message=$1
  shift
  if run_installer "$@"; then
    printf 'Expected failure: %s\n' "$message" >&2
    exit 1
  fi
  grep -Fq "$message" "$test_root/output" || { cat "$test_root/output"; exit 1; }
}
defaults=(--ssh-user test-user --mgmt-subnet 172.31.250.0/24)
run_installer --help
expect_failure 'Unknown argument' --unknown
expect_failure 'Missing value' --ssh-user
printf '1000\n' > "$test_root/uid"
expect_failure 'Run as root' "${defaults[@]}"
printf '0\n' > "$test_root/uid"
printf 'riscv64\n' > "$test_root/arch"
expect_failure 'Supported architectures' "${defaults[@]}"
printf 'x86_64\n' > "$test_root/arch"
for subnet in 8.8.8.0/24 172.32.0.0/24 192.168.1.1/24 10.0.256.0/24 10.00.1.0/24; do
  case "$subnet" in
    192.168.1.1/24) message='canonical private IPv4 /24' ;;
    10.0.256.0/24|10.00.1.0/24) message='Invalid IPv4 octet' ;;
    *) message='private IPv4 space' ;;
  esac
  expect_failure "$message" --ssh-user test-user --mgmt-subnet "$subnet"
done
expect_failure 'Invalid SSH account' --ssh-user 'user ALL=ALL' --mgmt-subnet 10.1.1.0/24
expect_failure 'does not exist' --ssh-user missing --mgmt-subnet 10.1.1.0/24
expect_failure 'non-root SSH account' --ssh-user root --mgmt-subnet 10.1.1.0/24
mv "$root/usr/bin/ip" "$test_root/saved-ip"
expect_failure 'Install required dependencies' "${defaults[@]}"
mv "$test_root/saved-ip" "$root/usr/bin/ip"
for failure in fail-docker fail-download bad-checksum fail-sudoers fail-build fail-frr fail-image-inspect; do
  : > "$test_root/calls"
  touch "$test_root/$failure"
  case "$failure" in
    fail-docker) message='default local Docker daemon' ;;
    fail-download) message='failed during download' ;;
    bad-checksum) message='FAILED' ;;
    fail-sudoers) message='Generated sudoers rule is invalid' ;;
    *) message='local image builds' ;;
  esac
  expect_failure "$message" "${defaults[@]}"
  [[ ! -e $root/usr/local/sbin/mini-sre-lab && ! -e $root/etc/mini-sre/lab.yaml ]]
  ! grep -q '^install ' "$test_root/calls"
  ! grep -q 'systemctl enable' "$test_root/calls"
  rm -- "$test_root/$failure"
done

# A pre-helper release must fail explicitly, before any image build.
cp "$test_root/mini-sre_linux_amd64.tar.gz" "$test_root/saved-archive"
mkdir "$test_root/old-package"
tar -C "$test_root/old-package" -czf "$test_root/mini-sre_linux_amd64.tar.gz" .
(cd "$test_root" && sha256sum mini-sre_linux_*.tar.gz > checksums.txt)
expect_failure 'publish a release with lab-host support' "${defaults[@]}"
cp "$test_root/saved-archive" "$test_root/mini-sre_linux_amd64.tar.gz"
(cd "$test_root" && sha256sum mini-sre_linux_*.tar.gz > checksums.txt)

run_installer "${defaults[@]}"
grep -q 'mgmt_subnet: 172.31.250.0/24' "$root/etc/mini-sre/lab.yaml"
grep -Fxq "test-user ALL=(root) NOPASSWD: $root/usr/local/sbin/mini-sre-lab request" "$root/etc/sudoers.d/mini-sre-lab"
[[ $(stat -c %a "$root/etc/mini-sre/lab.yaml") == 600 ]]
[[ $(stat -c %a "$root/etc/sudoers.d/mini-sre-lab") == 440 ]]
[[ $(stat -c %a "$root/var/lib/mini-sre/labs") == 700 ]]
[[ $(stat -c %a "$root/usr/local/sbin/mini-sre-lab") == 755 ]]
cmp "$project_dir/examples/mini-sre-lab-gc.service" "$root/etc/systemd/system/mini-sre-lab-gc.service"
cmp "$project_dir/examples/mini-sre-lab-gc.timer" "$root/etc/systemd/system/mini-sre-lab-gc.timer"
tar -xzf "$test_root/mini-sre_linux_amd64.tar.gz" -C "$test_root/package" ./mini-sre-lab
cmp "$test_root/package/mini-sre-lab" "$test_root/installed-release-binary"
grep -q 'helper inspect' "$test_root/calls"
grep -q 'systemctl enable --now mini-sre-lab-gc.timer' "$test_root/calls"
grep -q 'BASE_IMAGE=docker.io/library/alpine@sha256:' "$test_root/calls"
grep -q 'BASE_IMAGE=quay.io/frrouting/frr@sha256:' "$test_root/calls"

printf '# custom config\n' >> "$root/etc/mini-sre/lab.yaml"
sed -i 's/mini-sre-alpine:local/custom-alpine:local/; s/mini-sre-frr:local/custom-frr:local/' "$root/etc/mini-sre/lab.yaml"
chmod 600 "$root/etc/sudoers.d/mini-sre-lab"
printf '# custom sudoers\n' >> "$root/etc/sudoers.d/mini-sre-lab"
chmod 440 "$root/etc/sudoers.d/mini-sre-lab"
printf 'registered state\n' > "$root/var/lib/mini-sre/labs/keep"
cp "$root/etc/mini-sre/lab.yaml" "$test_root/saved-config"
cp "$root/etc/sudoers.d/mini-sre-lab" "$test_root/saved-sudoers"
printf 'aarch64\n' > "$test_root/arch"
run_installer --ssh-user ignored --mgmt-subnet ignored
grep -q '/v0.1.0/mini-sre_linux_arm64.tar.gz' "$test_root/calls"
cmp "$test_root/saved-config" "$root/etc/mini-sre/lab.yaml"
cmp "$test_root/saved-sudoers" "$root/etc/sudoers.d/mini-sre-lab"
[[ $(cat "$root/var/lib/mini-sre/labs/keep") == 'registered state' ]]
cp "$root/usr/local/sbin/mini-sre-lab" "$test_root/saved-binary"
for failure in fail-build fail-frr; do
  : > "$test_root/calls"
  touch "$test_root/$failure"
  expect_failure 'local image builds'
  ! grep -q '^install ' "$test_root/calls"
  cmp "$test_root/saved-binary" "$root/usr/local/sbin/mini-sre-lab"
  cmp "$project_dir/examples/mini-sre-lab-gc.service" "$root/etc/systemd/system/mini-sre-lab-gc.service"
  rm -- "$test_root/$failure"
done
for failure in fail-helper fail-timer; do
  : > "$test_root/calls"
  touch "$test_root/$failure"
  if [[ $failure == fail-helper ]]; then
    expect_failure 'helper verification'
  else
    expect_failure 'Timer failed'
  fi
  if [[ $failure == fail-helper ]]; then
    ! grep -q 'systemctl enable' "$test_root/calls"
  else
    grep -q 'journalctl' "$test_root/output"
  fi
  rm -- "$test_root/$failure"
done
chmod 666 "$root/etc/mini-sre/lab.yaml"
expect_failure 'Unsafe ownership or permissions'
chmod 600 "$root/etc/mini-sre/lab.yaml"
touch "$test_root/wrong-owner"
expect_failure 'Unsafe ownership or permissions'
rm -- "$test_root/wrong-owner"
ln -s "$test_root/saved-binary" "$root/usr/local/sbin/mini-sre-lab.new"
expect_failure 'Refusing symlink'
rm -- "$root/usr/local/sbin/mini-sre-lab.new"
mv "$root/etc/mini-sre" "$root/etc/saved-mini-sre"
ln -s "$root/etc/saved-mini-sre" "$root/etc/mini-sre"
expect_failure 'Refusing symlink'
rm -- "$root/etc/mini-sre"
mv "$root/etc/saved-mini-sre" "$root/etc/mini-sre"

if command -v script >/dev/null && command -v timeout >/dev/null; then
  # Reuse the sandbox, removing only fixture config/rules so prompts are required.
  rm -- "$root/etc/mini-sre/lab.yaml" "$root/etc/sudoers.d/mini-sre-lab"
  printf '%s\n' test-user 10.70.80.0/24 |
    timeout 15s script -q -e -c "cat '$test_root/installer.sh' | bash" /dev/null > "$test_root/terminal-output"
  grep -q 'mgmt_subnet: 10.70.80.0/24' "$root/etc/mini-sre/lab.yaml"
else
  printf 'Skipping terminal prompt test: script/timeout unavailable.\n'
fi
printf 'Lab host installer tests passed.\n'
