#!/usr/bin/env bash
set -euo pipefail
umask 077

fail() {
  printf 'MiniSRE: %s\n' "$*" >&2
  exit 1
}

usage() {
  printf '%s\n' \
    'Usage: bash install.sh [--uninstall | --help]' \
    'Install/update the latest MiniSRE release as a systemd user service.' \
    'Run as your regular user, without sudo.' \
    '--uninstall removes the service and binary; configuration and history are kept.'
}

action=install
case "${1:-}" in
  '') ;;
  --uninstall) action=uninstall ;;
  --help|-h) usage; exit 0 ;;
  *) usage >&2; exit 1 ;;
esac
(( $# <= 1 )) || fail 'Too many arguments.'

[[ $(uname -s) == Linux ]] || fail 'Linux with systemd is required.'
[[ $(id -u) != 0 ]] || fail 'Run as your regular user, without sudo.'
[[ ${HOME:-} == /* && $HOME != / ]] || fail 'HOME must be an absolute user home directory.'
install_dir="$HOME/mini-sre"
unit_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
unit_path="$unit_dir/mini-sre.service"
marker="$install_dir/.installed-by-minisre"

for path in "$install_dir" "$install_dir/mini-sre" "$install_dir/.env" \
  "$install_dir/.mini-sre.new" "$install_dir/hosts.yaml" "$marker" "$unit_path"; do
  [[ ! -L $path ]] || fail "Refusing to replace symlink: $path"
done
command -v systemctl >/dev/null || fail 'systemctl is required.'
systemctl --user show-environment >/dev/null || fail 'No systemd user session. Run from a normal login/SSH session.'

if [[ $action == uninstall ]]; then
  [[ -f $marker ]] || fail "No installer-managed installation at $install_dir. Remove a manual installation manually."
  load_state=$(systemctl --user show mini-sre.service -p LoadState --value)
  if [[ $load_state != not-found ]]; then
    systemctl --user stop mini-sre.service
  fi
  if [[ -f $unit_path ]]; then
    systemctl --user disable mini-sre.service
    rm -- "$unit_path"
  fi
  systemctl --user daemon-reload
  rm -f -- "$install_dir/mini-sre" "$marker"
  printf 'MiniSRE removed. Configuration in %s and Telegram history/tasks are kept.\n' "$install_dir"
  printf 'Linger is unchanged because other user services may depend on it.\n'
  exit 0
fi

for command in curl tar sha256sum loginctl install mktemp; do
  command -v "$command" >/dev/null || fail "$command is required."
done
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) fail 'Supported architectures: x86_64 and aarch64.' ;;
esac

tmpdir=$(mktemp -d)
trap 'rm -rf -- "$tmpdir"' EXIT
curl_options=(-fsSL --retry 2 --connect-timeout 15 --max-time 180 --proto '=https' --proto-redir '=https')
repository=https://github.com/Paramoshka/MiniSRE
# Resolve latest once so the archive and checksum always come from the same release.
release_url=$(curl "${curl_options[@]}" -o /dev/null -w '%{url_effective}' "$repository/releases/latest")
version=${release_url##*/}
[[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail 'Could not resolve a stable latest release.'
archive="mini-sre_linux_$arch.tar.gz"
download_url="$repository/releases/download/$version"
printf 'Downloading MiniSRE %s (%s)...\n' "$version" "$arch"
curl "${curl_options[@]}" "$download_url/$archive" -o "$tmpdir/$archive"
curl "${curl_options[@]}" "$download_url/checksums.txt" -o "$tmpdir/checksums.txt"
checksum_count=0
while read -r hash filename; do
  if [[ $filename == "$archive" ]]; then
    [[ $hash =~ ^[[:xdigit:]]{64}$ ]] || fail 'Invalid SHA256 checksum.'
    printf '%s  %s\n' "$hash" "$archive" > "$tmpdir/selected.sha256"
    checksum_count=$((checksum_count + 1))
  fi
done < "$tmpdir/checksums.txt"
[[ $checksum_count == 1 ]] || fail 'Expected exactly one archive checksum.'
(cd "$tmpdir" && sha256sum --check selected.sha256)
mkdir "$tmpdir/package"
tar -xzf "$tmpdir/$archive" -C "$tmpdir/package" --no-same-owner \
  ./mini-sre ./mini-sre.service
for file in mini-sre mini-sre.service; do
  [[ -f $tmpdir/package/$file && ! -L $tmpdir/package/$file ]] || fail "Invalid archive member: $file"
done
"$tmpdir/package/mini-sre" -h >/dev/null 2>&1 || fail 'The downloaded binary cannot run on this machine.'

if [[ ! -f $install_dir/.env ]]; then
  api_key=${DEEPSEEK_API_KEY:-}
  bot_token=${TELEGRAM_BOT_TOKEN:-}
  if [[ -z $api_key ]]; then
    [[ -r /dev/tty ]] || fail 'Set DEEPSEEK_API_KEY or run from an interactive terminal.'
    read -r -s -p 'DeepSeek API key: ' api_key < /dev/tty
    printf '\n'
  fi
  if [[ -z $bot_token ]]; then
    [[ -r /dev/tty ]] || fail 'Set TELEGRAM_BOT_TOKEN or run from an interactive terminal.'
    read -r -s -p 'Telegram bot token: ' bot_token < /dev/tty
    printf '\n'
  fi
  for secret in "$api_key" "$bot_token"; do
    [[ -n $secret && $secret != *$'\n'* && $secret != *$'\r'* ]] || fail 'Tokens must be nonempty single-line values.'
  done
fi
if [[ ! -f $install_dir/hosts.yaml ]]; then
  telegram_id=${TELEGRAM_USER_ID:-}
  if [[ -z $telegram_id ]]; then
    [[ -r /dev/tty ]] || fail 'Set TELEGRAM_USER_ID or run from an interactive terminal.'
    read -r -p 'Your numeric Telegram user ID: ' telegram_id < /dev/tty
  fi
  [[ $telegram_id =~ ^[1-9][0-9]{0,17}$ ]] || fail 'Telegram user ID must be a positive integer (at most 18 digits).'
fi

account=$(id -un)
linger=$(loginctl show-user "$account" -p Linger --value)
if [[ $linger != yes ]]; then
  command -v sudo >/dev/null || fail "Ask an administrator to run: loginctl enable-linger $account"
  printf 'Enabling linger so MiniSRE runs after logout and at boot (sudo may prompt).\n'
  sudo loginctl enable-linger "$account"
fi
[[ $(loginctl show-user "$account" -p Linger --value) == yes ]] || fail 'Linger is not enabled; installation stopped.'

mkdir -p "$install_dir" "$unit_dir"
if [[ ! -f $install_dir/.env ]]; then
  (set -o noclobber; printf "DEEPSEEK_API_KEY='%s'\nTELEGRAM_BOT_TOKEN='%s'\n" "$api_key" "$bot_token" > "$install_dir/.env")
fi
if [[ ! -f $install_dir/hosts.yaml ]]; then
  (set -o noclobber; printf 'language: ru\nhosts: {}\ntelegram:\n  allowed_user_ids: [%s]\n' "$telegram_id" > "$install_dir/hosts.yaml")
fi
chmod 600 "$install_dir/.env" "$install_dir/hosts.yaml"
# Rename into place: overwriting a running executable directly can fail with ETXTBSY.
install -m 755 "$tmpdir/package/mini-sre" "$install_dir/.mini-sre.new"
mv -f -- "$install_dir/.mini-sre.new" "$install_dir/mini-sre"
install -m 644 "$tmpdir/package/mini-sre.service" "$unit_path"
printf '%s\n' "$version" > "$marker"
systemctl --user daemon-reload
systemctl --user enable mini-sre.service
systemctl --user restart mini-sre.service
systemctl --user is-active --quiet mini-sre.service || fail 'Service failed; inspect: journalctl --user -u mini-sre.service -n 50'
printf 'MiniSRE %s installed in %s. Linger=yes.\n' "$version" "$install_dir"
printf 'Edit hosts: %s/hosts.yaml\nLogs: journalctl --user -u mini-sre.service -f\n' "$install_dir"
