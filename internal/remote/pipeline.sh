# Embedded shell template; Go validates commands and quotes every argument.
set +o posix
umask 077
ulimit -f 10240 || exit 125
dir=$(mktemp -d /tmp/mini-sre-XXXXXXXXXX) || exit 125
mkdir "$dir/data" || exit 125
stdout_file={{STDOUT_TARGET}}
stderr_file="$dir/stderr.txt"
cleanup() {
cleanup_code=$?
{{CLEANUP}}
exit "$cleanup_code"
}
trap cleanup EXIT
: > "$dir/headers"
: > "$dir/http"
printf 'MINISRE_PIPELINE_V1\n%s\n%s\n' "$dir" {{FILENAME}}
# Give the worker its own process group; its pipeline children stay in that group.
set -m
worker() {
set +m
{{PIPELINE}}
}
abort() {
  # The context and watchdog may signal together; finish cleanup without reentry.
  trap '' HUP INT TERM
  running=$!
  if test -n "$running"; then
    kill -KILL -- "-$running" 2>/dev/null
  fi
  wait 2>/dev/null
  exit 124
}
trap abort HUP INT TERM
worker &
job=$!
wait "$job"
trap - HUP INT TERM
test -f "$dir/codes" || exit 125
test -f "$stdout_file" || : >"$stdout_file"
test -f "$stderr_file" || : >"$stderr_file"
out_size=$(stat -c %s -- "$stdout_file") || exit 125
err_size=$(stat -c %s -- "$stderr_file") || exit 125
out_preview=0
err_preview=0
{{PREVIEWS}}
headers_size=$(stat -c %s -- "$dir/headers") || exit 125
http_size=$(stat -c %s -- "$dir/http") || exit 125
(( headers_size > 8192 )) && headers_size=8192
(( http_size > 4096 )) && http_size=4096
# Stage codes and byte counts precede the concatenated stream previews.
printf '%s\n%s\n%s\n%s\n%s\n%s\n%s\n' "$(cat "$dir/codes")" "$out_size" "$err_size" "$out_preview" "$err_preview" "$headers_size" "$http_size"
head -c "$out_preview" -- "$stdout_file"
head -c "$err_preview" -- "$stderr_file"
head -c "$headers_size" -- "$dir/headers"
head -c "$http_size" -- "$dir/http"
exit 0
