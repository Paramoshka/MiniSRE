package remote

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
)

//go:embed pipeline.sh
var pipelineShell string

const ArtifactLimit = 10 * 1024 * 1024

// Commands are prepared by tools, never decoded directly from model arguments.
type Command struct {
	Program      string
	Args         []string
	HTTPMetadata bool
}

type Stage struct {
	Commands  []Command // A source may contain multiple sequential commands.
	Pipe      []Command // Commands joined by shell pipes; mutually exclusive with Commands.
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
	// Let the shell trap clean up its worker group without timeout also signalling that group.
	raw, runErr := r.run(ctx, hostID, true, "timeout", "--foreground", "--signal=TERM", "--kill-after=1s", seconds,
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
		script, err := stageScript(stage, plan.Stderr == "merge")
		if err != nil {
			return "", err
		}
		stages = append(stages, script)
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
	pipeline := fmt.Sprintf(`{ %s; codes=("${PIPESTATUS[@]}"); printf '%%s\n' "${codes[*]}" >"$dir/codes"; } >%s 2>%s`,
		strings.Join(stages, " | "), stdoutRedirect, stderrRedirect)
	// NewReplacer does one pass: placeholder-like text in arguments stays literal.
	script := strings.NewReplacer(
		"{{STDOUT_TARGET}}", stdoutTarget,
		"{{CLEANUP}}", cleanup,
		"{{FILENAME}}", shellQuote(filename),
		"{{PIPELINE}}", pipeline,
		"{{PREVIEWS}}", modePreviewScript("out", plan.Stdout, 32768)+modePreviewScript("err", plan.Stderr, 16384),
	).Replace(pipelineShell)
	return script, nil
}

func stageScript(stage Stage, mergeStderr bool) (string, error) {
	if len(stage.Commands) == 0 && len(stage.Pipe) == 0 {
		return "", errors.New("pipeline: empty stage")
	}
	if len(stage.Commands) > 0 && len(stage.Pipe) > 0 {
		return "", errors.New("pipeline: stage cannot mix sequential commands and a pipe")
	}
	var body string
	if len(stage.Pipe) > 0 {
		parts := make([]string, 0, len(stage.Pipe))
		for _, cmd := range stage.Pipe {
			command, err := commandString(cmd)
			if err != nil {
				return "", err
			}
			parts = append(parts, command)
		}
		// Report failures from every command in the pipe. SIGPIPE is expected
		// only for commands before a limiting head, which closes the pipe early.
		body = fmt.Sprintf(
			`( %s; codes=("${PIPESTATUS[@]}"); ((${#codes[@]})) || exit 125; `+
				`code=0; last=$((${#codes[@]}-1)); `+
				`for index in "${!codes[@]}"; do `+
				`if (( codes[index] != 0 )) && ! (( index < last && codes[index] == 141 )); then `+
				`code=${codes[index]}; fi; done; exit "$code" )`,
			strings.Join(parts, " | "),
		)
	} else {
		commands := make([]string, 0, len(stage.Commands))
		for _, cmd := range stage.Commands {
			command, err := commandString(cmd)
			if err != nil {
				return "", err
			}
			commands = append(commands, command+" || exit $?")
		}
		body = "( " + strings.Join(commands, "; ") + " )"
	}
	if stage.HeadLines > 0 {
		body = fmt.Sprintf(`( %s | { head -n %d; cat >/dev/null; }; codes=("${PIPESTATUS[@]}"); exit "${codes[0]}" )`, body, stage.HeadLines)
	}
	if stage.Drain {
		body = fmt.Sprintf(`( %s; code=$?; cat >/dev/null; exit "$code" )`, body)
	}
	if mergeStderr {
		body += " 2>&1"
	}
	return body, nil
}

func commandString(cmd Command) (string, error) {
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
	return command, nil
}

func modePreviewScript(prefix, mode string, limit int) string {
	if mode != "inline" {
		return ""
	}
	return fmt.Sprintf("%s_preview=$%s_size\n(( %s_preview > %d )) && %s_preview=%d\n", prefix, prefix, prefix, limit, prefix, limit)
}

func parsePipelineOutput(raw string, plan Pipeline) (PipelineResult, error) {
	var result PipelineResult
	// Header: magic, directory, filename, stage codes, six byte counts; then payload.
	const headerLines = 10
	fields := strings.SplitN(raw, "\n", headerLines+1)
	if len(fields) < 4 || fields[0] != "MINISRE_PIPELINE_V1" || !strings.HasPrefix(fields[1], "/tmp/mini-sre-") {
		return result, errors.New("pipeline: malformed report")
	}
	if plan.Stdout == "file" {
		result.StdoutPath = fields[1] + "/data/" + fields[2]
	}
	if plan.Stderr == "file" {
		result.StderrPath = fields[1] + "/stderr.txt"
	}
	if len(fields) != headerLines+1 {
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
	stdoutBytes, stderrBytes := sizes[0], sizes[1]
	stdoutPreview, stderrPreview := sizes[2], sizes[3]
	headersPreview, metadataPreview := sizes[4], sizes[5]
	data := fields[headerLines]
	if stdoutPreview+stderrPreview+headersPreview+metadataPreview != int64(len(data)) {
		return result, errors.New("pipeline: incomplete payload")
	}
	result.StdoutBytes, result.StderrBytes = stdoutBytes, stderrBytes
	result.Stdout, data = data[:stdoutPreview], data[stdoutPreview:]
	result.Stderr, data = data[:stderrPreview], data[stderrPreview:]
	result.HTTPHeaders, result.HTTPMetadata = data[:headersPreview], data[headersPreview:]
	result.StdoutTruncated = plan.Stdout == "inline" && stdoutBytes > stdoutPreview
	result.StderrTruncated = plan.Stderr == "inline" && stderrBytes > stderrPreview
	result.Complete = true
	return result, nil
}
