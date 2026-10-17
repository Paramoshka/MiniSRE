#!/usr/bin/env bash
set -Eeuo pipefail
umask 077
export PATH=/usr/sbin:/usr/bin:/sbin:/bin

fail() {
  printf 'MiniSRE lab host: %s\n' "$*" >&2
  exit 1
}

usage() {
  printf '%s\n' \
    'Usage: sudo bash install-lab-host.sh [--ssh-user USER] [--mgmt-subnet CIDR]' \
    'Install/update the lab helper, build local images and enable TTL cleanup.' \
    'Requires existing Docker, containerlab and systemd. Linux amd64/arm64 only.' \
    'Missing first-install settings are requested through /dev/tty.' \
    'Existing configuration and sudoers are preserved; arguments only create missing files.'
}

ssh_user=''
mgmt_subnet=''
while (( $# )); do
  case "$1" in
    --ssh-user|--mgmt-subnet)
      (( $# >= 2 )) && [[ -n $2 && $2 != --* ]] || fail "Missing value for $1."
      case "$1" in
        --ssh-user) ssh_user=$2 ;;
        --mgmt-subnet) mgmt_subnet=$2 ;;
      esac
      shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) usage >&2; fail "Unknown argument: $1" ;;
  esac
done

[[ $(uname -s) == Linux ]] || fail 'Linux with systemd is required.'
[[ $(id -u) == 0 ]] || fail 'Run as root, for example: sudo bash install-lab-host.sh.'
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) fail 'Supported architectures: x86_64 and aarch64.' ;;
esac

missing=()
for command in curl tar sha256sum install mktemp stat visudo systemctl env; do
  command -v "$command" >/dev/null || missing+=("$command")
done
for command in docker containerlab ip timeout; do
  [[ -x /usr/bin/$command ]] || missing+=("/usr/bin/$command")
done
(( ${#missing[@]} == 0 )) || fail "Install required dependencies first: ${missing[*]}"
systemctl show-environment >/dev/null || fail 'No running system systemd manager.'
timeout_version=$(/usr/bin/timeout --version) || fail 'GNU timeout is required.'
[[ $timeout_version == *'GNU coreutils'* ]] || fail 'GNU timeout is required.'
/usr/bin/ip -j -4 address show >/dev/null || fail 'ip must support JSON output.'
CLAB_VERSION_CHECK=disable /usr/bin/containerlab version || fail 'containerlab cannot run.'

# Match the helper's environment: never build through a caller's Docker context/host.
docker_local() {
  env -i PATH="$PATH" LC_ALL=C /usr/bin/docker "$@"
}
docker_local info >/dev/null || fail 'Root must be able to access the default local Docker daemon.'

binary=/usr/local/sbin/mini-sre-lab
config=/etc/mini-sre/lab.yaml
state=/var/lib/mini-sre/labs
sudoers=/etc/sudoers.d/mini-sre-lab
unit_dir=/etc/systemd/system

# Check existing ancestors too, before install/mkdir can follow a symlink.
check_path() {
  local path=$1 kind=$2 metadata uid mode
  while :; do
    [[ ! -L $path ]] || fail "Refusing symlink: $path"
    if [[ -e $path ]]; then
      if [[ $kind == directory ]]; then
        [[ -d $path ]] || fail "Expected directory: $path"
      else
        [[ -f $path ]] || fail "Expected regular file: $path"
      fi
      metadata=$(stat -c '%u %a' -- "$path")
      read -r uid mode <<< "$metadata"
      [[ $uid == 0 ]] && (( (8#$mode & 0022) == 0 )) || fail "Unsafe ownership or permissions: $path"
    fi
    [[ $path != / ]] || break
    path=${path%/*}
    path=${path:-/}
    kind=directory
  done
}
check_path "$state" directory
for path in "$binary" "${binary}.new" "$config" "$sudoers" \
  "$unit_dir/mini-sre-lab-gc.service" "$unit_dir/mini-sre-lab-gc.timer"; do
  check_path "$path" file
done

if [[ ! -f $sudoers ]]; then
  if [[ -z $ssh_user ]]; then
    read -r -p 'SSH account used by MiniSRE: ' ssh_user < /dev/tty || fail 'Supply --ssh-user USER (no interactive terminal).'
  fi
  [[ $ssh_user =~ ^[a-zA-Z_][a-zA-Z0-9_-]*\$?$ ]] || fail 'Invalid SSH account name.'
  ssh_uid=$(id -u -- "$ssh_user") || fail "SSH account does not exist: $ssh_user"
  [[ $ssh_uid != 0 ]] || fail 'Choose a non-root SSH account for the restricted sudo rule.'
else
  printf 'Keeping %s; --ssh-user is only used to create a missing rule.\n' "$sudoers"
  visudo -cf "$sudoers" || fail 'Existing sudoers rule is invalid.'
fi

if [[ ! -f $config ]]; then
  if [[ -z $mgmt_subnet ]]; then
    read -r -p 'Unused private management IPv4 /24 (e.g. 172.31.250.0/24): ' mgmt_subnet < /dev/tty || fail 'Supply --mgmt-subnet CIDR (no interactive terminal).'
  fi
  [[ $mgmt_subnet =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.0/24$ ]] || fail 'Management subnet must be a canonical private IPv4 /24.'
  octets=("${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}")
  for octet in "${octets[@]}"; do
    [[ $octet == 0 || $octet != 0* ]] && (( 10#$octet <= 255 )) || fail 'Invalid IPv4 octet.'
  done
  a=${octets[0]}; b=${octets[1]}
  (( a == 10 || (a == 172 && b >= 16 && b <= 31) || (a == 192 && b == 168) )) || fail 'Management subnet must use private IPv4 space.'
else
  printf 'Keeping %s; --mgmt-subnet is only used to create a missing config.\n' "$config"
fi

tmpdir=$(mktemp -d /tmp/mini-sre-lab.XXXXXXXX)
trap 'rm -rf -- "$tmpdir"' EXIT
stage=download
trap 'printf "MiniSRE lab host: failed during %s (line %s).\n" "$stage" "$LINENO" >&2' ERR
curl_options=(-fsSL --retry 2 --connect-timeout 15 --max-time 180 --proto '=https' --proto-redir '=https')
repository=https://github.com/Paramoshka/MiniSRE
release_url=$(curl "${curl_options[@]}" -o /dev/null -w '%{url_effective}' "$repository/releases/latest")
version=${release_url##*/}
[[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail 'Could not resolve a stable latest release.'
archive="mini-sre_linux_$arch.tar.gz"
download_url="$repository/releases/download/$version"
printf 'Downloading MiniSRE lab helper %s (%s)...\n' "$version" "$arch"
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
members=(mini-sre-lab mini-sre-lab.yaml mini-sre-lab.sudoers mini-sre-lab-gc.service mini-sre-lab-gc.timer Dockerfile.lab)
for file in "${members[@]}"; do
  tar -tzf "$tmpdir/$archive" "./$file" >/dev/null 2>&1 || fail "Release $version lacks $file; publish a release with lab-host support."
done
tar -xzf "$tmpdir/$archive" -C "$tmpdir/package" --no-same-owner "${members[@]/#/./}"
for file in "${members[@]}"; do
  [[ -f $tmpdir/package/$file && ! -L $tmpdir/package/$file ]] || fail "Invalid archive member: $file"
done
[[ -x $tmpdir/package/mini-sre-lab ]] || fail 'Helper is not executable.'
# No arguments exits before reading configuration or accessing lab state.
helper_usage=$("$tmpdir/package/mini-sre-lab" 2>&1) && fail 'Unexpected helper usage result.'
[[ $helper_usage == 'usage: mini-sre-lab request|gc' ]] || fail 'Downloaded helper cannot run on this host.'

if [[ ! -f $config ]]; then
  sed "s|^mgmt_subnet:.*|mgmt_subnet: $mgmt_subnet|" "$tmpdir/package/mini-sre-lab.yaml" > "$tmpdir/lab.yaml"
fi
if [[ ! -f $sudoers ]]; then
  printf '%s ALL=(root) NOPASSWD: /usr/local/sbin/mini-sre-lab request\n' "$ssh_user" > "$tmpdir/sudoers"
  visudo -cf "$tmpdir/sudoers" || fail 'Generated sudoers rule is invalid.'
fi

stage='local image builds'
alpine_base='docker.io/library/alpine@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6'
frr_base='quay.io/frrouting/frr@sha256:e995beaa50fdc9edb35eadcfefa29b7f062cc06f2b812613789b68fa541554d2'
docker_local build -f "$tmpdir/package/Dockerfile.lab" --build-arg "BASE_IMAGE=$alpine_base" -t mini-sre-alpine:local "$tmpdir/package"
docker_local build -f "$tmpdir/package/Dockerfile.lab" --build-arg "BASE_IMAGE=$frr_base" -t mini-sre-frr:local "$tmpdir/package"
docker_local image inspect mini-sre-alpine:local mini-sre-frr:local >/dev/null

stage=installation
install -d -o root -g root -m 0755 /usr/local/sbin /etc/sudoers.d "$unit_dir"
install -d -o root -g root -m 0700 /etc/mini-sre /var/lib/mini-sre "$state"
if [[ ! -f $config ]]; then
  install -o root -g root -m 0600 "$tmpdir/lab.yaml" "$config"
fi
if [[ ! -f $sudoers ]]; then
  install -o root -g root -m 0440 "$tmpdir/sudoers" "$sudoers"
fi
install -o root -g root -m 0755 "$tmpdir/package/mini-sre-lab" "${binary}.new"
mv -f -- "${binary}.new" "$binary"
install -o root -g root -m 0644 "$tmpdir/package/mini-sre-lab-gc.service" "$tmpdir/package/mini-sre-lab-gc.timer" "$unit_dir/"

stage='helper verification'
printf '%s\n' '{"action":"inspect"}' | "$binary" request
stage='timer activation'
systemctl daemon-reload
systemctl enable --now mini-sre-lab-gc.timer
systemctl restart mini-sre-lab-gc.timer
systemctl is-active --quiet mini-sre-lab-gc.timer || fail 'Timer failed; inspect: journalctl -u mini-sre-lab-gc.timer -n 50'
printf 'MiniSRE lab host %s installed. Config: %s\n' "$version" "$config"
printf 'Local images: mini-sre-alpine:local, mini-sre-frr:local (existing config image references are preserved).\n'
printf 'TTL cleanup: every minute. Logs: journalctl -u mini-sre-lab-gc.service -n 50\n'
printf 'Add this SSH host ID to lab_hosts in the bot hosts.yaml to enable lab tools.\n'
printf 'Reserve the management subnet: deployment checks overlap with host/VPN routes and Docker networks.\n'
