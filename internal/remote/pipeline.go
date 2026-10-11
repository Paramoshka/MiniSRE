package remote

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
)

const ArtifactLimit = 10 * 1024 * 1024

// Commands are prepared by tools, never decoded directly from model arguments.
type Command struct {
	Program      string
	Args         []string
	HTTPMetadata bool
}

type Stage struct {
	Commands  []Command // A source may contain multiple sequential commands.
	Drain     bool      // Keep consuming stdin after a limiting filter finishes.
	HeadLines int       // Preserve the top-process source's row limit.
}

type Pipeline struct {
	Stages   []Stage
	Stdout   string
	Stderr   string
	Filename string
}

type PipelineResult struct {
	Codes                            []int
	Stdout, Stderr                   string
	StdoutPath, StderrPath           string
	StdoutBytes, StderrBytes         int64
	StdoutTruncated, StderrTruncated bool
	Complete, Limited                bool
	HTTPHeaders, HTTPMetadata        string
}

func (r *Runner) RunPipeline(ctx context.Context, hostID string, plan Pipeline) (PipelineResult, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	if plan.Stdout == "" {
		plan.Stdout = "inline"
	}
	if plan.Stderr == "" {
		plan.Stderr = "inline"
	}
	result := PipelineResult{Codes: make([]int, len(plan.Stages))}
	for i := range result.Codes {
		result.Codes[i] = -1
	}
	script, err := pipelineScript(plan)
	if err != nil {
		return result, err
	}
	deadline, _ := ctx.Deadline()
	seconds := strconv.FormatFloat(time.Until(deadline).Seconds(), 'f', 3, 64)
	// The remote watchdog also bounds jobs after a connection is lost.
	raw, runErr := r.run(ctx, hostID, true, "timeout", "--signal=TERM", "--kill-after=1s", seconds,
		"bash", "--noprofile", "--norc", "-p", "-c", script)
	parsed, parseErr := parsePipelineOutput(raw.Stdout, plan)
	if parseErr == nil {
		result = parsed
	} else {
		result.StdoutPath, result.StderrPath = parsed.StdoutPath, parsed.StderrPath
	}
	if runErr != nil {
		return result, fmt.Errorf("pipeline: %w; %s", runErr, raw.Stderr)
	}
	if parseErr != nil || raw.Truncated {
		return result, fmt.Errorf("pipeline: incomplete execution report; %s", raw.Stderr)
	}
	return result, nil
}

func pipelineScript(plan Pipeline) (string, error) {
	if len(plan.Stages) < 1 || len(plan.Stages) > 5 {
		return "", errors.New("pipeline: expected 1..5 stages")
	}
	if plan.Stdout == "" {
		plan.Stdout = "inline"
	}
	if plan.Stderr == "" {
		plan.Stderr = "inline"
	}
	for _, mode := range []string{plan.Stdout, plan.Stderr} {
		if mode != "inline" && mode != "file" && mode != "discard" && !(mode == "merge" && plan.Stderr == mode) {
			return "", errors.New("pipeline: invalid output mode")
		}
	}
	if plan.Stdout == "merge" {
		return "", errors.New("pipeline: merge is only supported for stderr")
	}
	filename := plan.Filename
	if filename == "" {
		filename = "stdout.txt"
	}
	if filename == "." || filename == ".." || strings.HasPrefix(filename, ".") || path.Base(filename) != filename ||
		len(filename) > 255 || strings.ContainsAny(filename, "\\\x00\r\n") {
		return "", errors.New("pipeline: invalid output filename")
	}
	var stages []string
	for _, stage := range plan.Stages {
		if len(stage.Commands) == 0 {
			return "", errors.New("pipeline: empty stage")
		}
		var commands []string
		for _, cmd := range stage.Commands {
			if cmd.Program == "" || strings.ContainsRune(cmd.Program, 0) {
				return "", errors.New("pipeline: invalid program")
			}
			command := shellQuote(cmd.Program)
			for _, arg := range cmd.Args {
				if strings.ContainsRune(arg, 0) {
					return "", errors.New("pipeline: NUL in argument")
				}
				command += " " + shellQuote(arg)
			}
			if cmd.HTTPMetadata {
				command += ` --dump-header "$dir/headers" --write-out "%output{$dir/http}%{http_code}\n%{remote_ip}\n%{time_total}\n%{url_effective}\n"`
			}
			commands = append(commands, command+" || exit $?")
		}
		body := "( " + strings.Join(commands, "; ") + " )"
		if stage.HeadLines > 0 {
			body = "( " + body + " | { head -n " + strconv.Itoa(stage.HeadLines) + "; cat >/dev/null; }; codes=(\"${PIPESTATUS[@]}\"); exit \"${codes[0]}\" )"
		}
		if stage.Drain {
			body = "( " + body + "; code=$?; cat >/dev/null; exit \"$code\" )"
		}
		if plan.Stderr == "merge" {
			body += " 2>&1"
		}
		stages = append(stages, body)
	}
	// Filename is shell-quoted separately, even when it contains quotes or dollars.
	stdoutTarget := `"$dir/data/"` + shellQuote(filename)
	stdoutRedirect := stdoutTarget
	stderrRedirect := `"$dir/stderr.txt"`
	if plan.Stdout == "discard" {
		stdoutRedirect = "/dev/null"
	}
	if plan.Stderr == "discard" || plan.Stderr == "merge" {
		stderrRedirect = "/dev/null"
	}
	cleanup := ""
	if plan.Stdout != "file" {
		cleanup += "rm -f -- \"$stdout_file\" || cleanup_code=125\n"
	}
	if plan.Stderr != "file" {
		cleanup += "rm -f -- \"$stderr_file\" || cleanup_code=125\n"
	}
	cleanup += "rm -f -- \"$dir/codes\" \"$dir/headers\" \"$dir/http\" || cleanup_code=125\n"
	if plan.Stdout != "file" && plan.Stderr != "file" {
		cleanup += "rmdir -- \"$dir/data\" \"$dir\" || cleanup_code=125\n"
	}
	script := `set +o posix
umask 077
ulimit -f 10240 || exit 125
dir=$(mktemp -d /tmp/mini-sre-XXXXXXXXXX) || exit 125
mkdir "$dir/data" || exit 125
stdout_file=` + stdoutTarget + `
stderr_file="$dir/stderr.txt"
cleanup() {
cleanup_code=$?
` + cleanup + `exit "$cleanup_code"
}
trap cleanup EXIT
: > "$dir/headers"
: > "$dir/http"
printf 'MINISRE_PIPELINE_V1\n%s\n%s\n' "$dir" ` + shellQuote(filename) + `
set -m
worker() {
set +m
` + "{ " + strings.Join(stages, " | ") + "; codes=(\"${PIPESTATUS[@]}\"); printf '%s\\n' \"${codes[*]}\" >\"$dir/codes\"; } >" + stdoutRedirect + " 2>" + stderrRedirect + `
}
abort() {
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
` + modePreviewScript("out", plan.Stdout, 32768) + modePreviewScript("err", plan.Stderr, 16384) + `
headers_size=$(stat -c %s -- "$dir/headers") || exit 125
http_size=$(stat -c %s -- "$dir/http") || exit 125
(( headers_size > 8192 )) && headers_size=8192
(( http_size > 4096 )) && http_size=4096
printf '%s\n%s\n%s\n%s\n%s\n%s\n%s\n' "$(cat "$dir/codes")" "$out_size" "$err_size" "$out_preview" "$err_preview" "$headers_size" "$http_size"
head -c "$out_preview" -- "$stdout_file"
head -c "$err_preview" -- "$stderr_file"
head -c "$headers_size" -- "$dir/headers"
head -c "$http_size" -- "$dir/http"
exit 0
`
	return script, nil
}

func modePreviewScript(prefix, mode string, limit int) string {
	if mode != "inline" {
		return ""
	}
	return fmt.Sprintf("%s_preview=$%s_size\n(( %s_preview > %d )) && %s_preview=%d\n", prefix, prefix, prefix, limit, prefix, limit)
}

func parsePipelineOutput(raw string, plan Pipeline) (PipelineResult, error) {
	var result PipelineResult
	fields := strings.SplitN(raw, "\n", 11)
	if len(fields) < 4 || fields[0] != "MINISRE_PIPELINE_V1" || !strings.HasPrefix(fields[1], "/tmp/mini-sre-") {
		return result, errors.New("pipeline: malformed report")
	}
	if plan.Stdout == "file" {
		result.StdoutPath = fields[1] + "/data/" + fields[2]
	}
	if plan.Stderr == "file" {
		result.StderrPath = fields[1] + "/stderr.txt"
	}
	if len(fields) != 11 {
		return result, errors.New("pipeline: incomplete report")
	}
	for _, code := range strings.Fields(fields[3]) {
		n, err := strconv.Atoi(code)
		if err != nil || n < 0 || n > 255 {
			return result, errors.New("pipeline: invalid stage status")
		}
		result.Codes = append(result.Codes, n)
		if n == 153 {
			result.Limited = true
		}
	}
	if len(result.Codes) != len(plan.Stages) {
		return result, errors.New("pipeline: missing stage status")
	}
	var sizes [6]int64
	for i := range sizes {
		n, err := strconv.ParseInt(fields[i+4], 10, 64)
		if err != nil || n < 0 || n > ArtifactLimit {
			return result, errors.New("pipeline: invalid output size")
		}
		sizes[i] = n
	}
	if sizes[2]+sizes[3]+sizes[4]+sizes[5] != int64(len(fields[10])) {
		return result, errors.New("pipeline: incomplete payload")
	}
	result.StdoutBytes, result.StderrBytes = sizes[0], sizes[1]
	data := fields[10]
	result.Stdout, data = data[:sizes[2]], data[sizes[2]:]
	result.Stderr, data = data[:sizes[3]], data[sizes[3]:]
	result.HTTPHeaders, result.HTTPMetadata = data[:sizes[4]], data[sizes[4]:]
	result.StdoutTruncated = plan.Stdout == "inline" && sizes[0] > sizes[2]
	result.StderrTruncated = plan.Stderr == "inline" && sizes[1] > sizes[3]
	if plan.Stdout == "file" {
		result.StdoutPath = fields[1] + "/data/" + fields[2]
	}
	if plan.Stderr == "file" {
		result.StderrPath = fields[1] + "/stderr.txt"
	}
	result.Complete = true
	return result, nil
}
