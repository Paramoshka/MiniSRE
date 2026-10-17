#!/usr/bin/env bash
set -euo pipefail

project_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
test_root=$(mktemp -d)
trap 'rm -rf -- "$test_root"' EXIT
export TEST_ROOT="$test_root"
export HOME="$test_root/home with spaces"
export XDG_CONFIG_HOME="$HOME/.config"
export DEEPSEEK_API_KEY='test-api-key'
export TELEGRAM_BOT_TOKEN='123:test-bot-token'
export TELEGRAM_USER_ID=123456789
mkdir -p "$test_root/bin" "$test_root/package" "$HOME"

cat > "$test_root/bin/mock" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
command=${0##*/}
printf '%s %s\n' "$command" "$*" >> "$TEST_ROOT/calls"
case "$command" in
  id)
    case "$1" in
      -u) printf '1000\n' ;;
      -un) printf 'test-user\n' ;;
      *) exit 1 ;;
    esac ;;
  uname)
    case "$1" in
      -s) printf 'Linux\n' ;;
      -m) printf '%s\n' "${MOCK_ARCH:-x86_64}" ;;
      *) exit 1 ;;
    esac ;;
  systemctl)
    [[ ${MOCK_SERVICE_FAILURE:-0} != 1 || $* != *is-active* ]] ;;
  loginctl)
    case "$1" in
      show-user) cat "$TEST_ROOT/linger" ;;
      enable-linger)
        [[ ${MOCK_LINGER_FAILURE:-0} != 1 ]] || exit 1
        printf 'yes\n' > "$TEST_ROOT/linger" ;;
      *) exit 1 ;;
    esac ;;
  sudo) exec "$@" ;;
  curl)
    output=''
    url=''
    while (( $# )); do
      case "$1" in
        -o) output=$2; shift 2 ;;
        https://*) url=$1; shift ;;
        *) shift ;;
      esac
    done
    case "$url" in
      */releases/latest) printf 'https://github.com/Paramoshka/MiniSRE/releases/tag/v0.1.0' ;;
      */v0.1.0/mini-sre_linux_*.tar.gz) cp "$TEST_ROOT/${url##*/}" "$output" ;;
      */v0.1.0/checksums.txt)
        if [[ ${MOCK_BAD_CHECKSUM:-0} == 1 ]]; then
          printf '%064d  mini-sre_linux_amd64.tar.gz\n' 0 > "$output"
        else
          cp "$TEST_ROOT/checksums.txt" "$output"
        fi ;;
      *) printf 'Unexpected URL: %s\n' "$url" >&2; exit 1 ;;
    esac ;;
  *) exit 1 ;;
esac
MOCK
chmod +x "$test_root/bin/mock"
for command in id uname systemctl loginctl sudo curl; do
  ln -s mock "$test_root/bin/$command"
done
export PATH="$test_root/bin:$PATH"
printf 'no\n' > "$test_root/linger"
printf '#!/usr/bin/env bash\n[[ ${1:-} == -h ]]\n' > "$test_root/package/mini-sre"
chmod +x "$test_root/package/mini-sre"
cp "$project_dir/examples/mini-sre.service" "$test_root/package/mini-sre.service"
for arch in amd64 arm64; do
  tar -C "$test_root/package" -czf "$test_root/mini-sre_linux_$arch.tar.gz" .
done
if [[ -n ${MINISRE_TEST_ARCHIVE:-} ]]; then
  cp "$MINISRE_TEST_ARCHIVE" "$test_root/mini-sre_linux_amd64.tar.gz"
fi
(cd "$test_root" && sha256sum mini-sre_linux_*.tar.gz > checksums.txt)

run_installer() {
  # stdin is a pipe, as with curl | bash; supplied credentials avoid a real TTY.
  bash -c 'cat "$1" | bash -s -- "${@:2}"' _ "$project_dir/scripts/install.sh" "$@"
}

expect_failure() {
  if run_installer "$@" > "$test_root/output" 2>&1; then
    printf 'Expected installer failure\n' >&2
    exit 1
  fi
}

MOCK_BAD_CHECKSUM=1 expect_failure
[[ ! -e $HOME/mini-sre ]]
[[ $(cat "$test_root/linger") == no ]]
MOCK_ARCH=riscv64 expect_failure
[[ ! -e $HOME/mini-sre ]]
MOCK_LINGER_FAILURE=1 expect_failure
[[ ! -e $HOME/mini-sre ]]

run_installer
[[ $(cat "$test_root/linger") == yes ]]
[[ -x $HOME/mini-sre/mini-sre ]]
[[ $(stat -c %a "$HOME/mini-sre/.env") == 600 ]]
[[ $(stat -c %a "$HOME/mini-sre/hosts.yaml") == 600 ]]
grep -q 'allowed_user_ids: \[123456789\]' "$HOME/mini-sre/hosts.yaml"
grep -q 'TELEGRAM_BOT_TOKEN=' "$HOME/mini-sre/.env"
cmp "$project_dir/examples/mini-sre.service" "$XDG_CONFIG_HOME/systemd/user/mini-sre.service"
grep -q 'sudo loginctl enable-linger test-user' "$test_root/calls"
grep -q 'systemctl --user restart mini-sre.service' "$test_root/calls"

printf '# custom settings\n' >> "$HOME/mini-sre/.env"
printf '# custom hosts\n' >> "$HOME/mini-sre/hosts.yaml"
cp "$HOME/mini-sre/.env" "$test_root/saved.env"
cp "$HOME/mini-sre/hosts.yaml" "$test_root/saved.yaml"
mkdir -p "$HOME/.local/state/mini-sre"
printf 'history\n' > "$HOME/.local/state/mini-sre/telegram.json"
printf 'tasks\n' > "$HOME/.local/state/mini-sre/tasks.json"
printf 'unrelated source\n' > "$HOME/mini-sre/keep.go"
printf '' > "$test_root/calls"
MOCK_ARCH=aarch64 run_installer
cmp "$test_root/saved.env" "$HOME/mini-sre/.env"
cmp "$test_root/saved.yaml" "$HOME/mini-sre/hosts.yaml"
grep -q '/v0.1.0/mini-sre_linux_arm64.tar.gz' "$test_root/calls"
if grep -q 'enable-linger' "$test_root/calls"; then
  printf 'Already enabled linger should be preserved\n' >&2
  exit 1
fi

MOCK_BAD_CHECKSUM=1 expect_failure
[[ -x $HOME/mini-sre/mini-sre ]]
MOCK_SERVICE_FAILURE=1 expect_failure
grep -q 'journalctl' "$test_root/output"

run_installer --uninstall
[[ ! -e $HOME/mini-sre/mini-sre ]]
[[ ! -e $XDG_CONFIG_HOME/systemd/user/mini-sre.service ]]
cmp "$test_root/saved.env" "$HOME/mini-sre/.env"
cmp "$test_root/saved.yaml" "$HOME/mini-sre/hosts.yaml"
[[ -f $HOME/.local/state/mini-sre/telegram.json ]]
[[ -f $HOME/.local/state/mini-sre/tasks.json ]]
[[ -f $HOME/mini-sre/keep.go ]]
[[ $(cat "$test_root/linger") == yes ]]
expect_failure --uninstall
grep -q 'No installer-managed installation' "$test_root/output"

if command -v script >/dev/null && command -v timeout >/dev/null; then
  mkdir "$test_root/interactive-home"
  printf '%s\n' 'interactive-api-key' '456:interactive-bot-token' '987654321' |
    env -u DEEPSEEK_API_KEY -u TELEGRAM_BOT_TOKEN -u TELEGRAM_USER_ID \
      HOME="$test_root/interactive-home" XDG_CONFIG_HOME="$test_root/interactive-home/.config" \
      MINISRE_TEST_INSTALLER="$project_dir/scripts/install.sh" \
      timeout 15s script -q -e -c 'cat "$MINISRE_TEST_INSTALLER" | bash' /dev/null > "$test_root/terminal-output"
  grep -q "DEEPSEEK_API_KEY='interactive-api-key'" "$test_root/interactive-home/mini-sre/.env"
  grep -q 'allowed_user_ids: \[987654321\]' "$test_root/interactive-home/mini-sre/hosts.yaml"
else
  printf 'Skipping terminal prompt test: script/timeout are unavailable.\n'
fi

ln -s "$test_root/saved.env" "$HOME/mini-sre/.mini-sre.new"
expect_failure
grep -q 'Refusing to replace symlink' "$test_root/output"
run_installer --help
expect_failure --unknown
printf 'Installer tests passed.\n'
