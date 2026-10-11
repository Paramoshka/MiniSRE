package tools

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"mini-sre/internal/remote"
)

var httpHeaderName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

func curlCommand(args toolArgs) (remote.Command, string, error) {
	u, err := url.Parse(args.URL)
	if err != nil || u.Hostname() == "" || u.Opaque != "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return remote.Command{}, "", errors.New("tools: url must be an absolute HTTP/HTTPS URL without embedded credentials")
	}
	for _, char := range args.URL {
		if char <= 32 || char == 127 {
			return remote.Command{}, "", errors.New("tools: URL must not contain whitespace or control characters")
		}
	}
	method := strings.ToUpper(args.Method)
	if method == "" {
		method = "GET"
	}
	if method != "GET" && method != "HEAD" {
		return remote.Command{}, "", errors.New("tools: method must be GET or HEAD")
	}
	if method == "HEAD" && args.Download {
		return remote.Command{}, "", errors.New("tools: download is only supported with GET")
	}
	filename := ""
	if args.Download {
		filename = path.Base(u.EscapedPath())
		if u.EscapedPath() == "" || strings.HasSuffix(u.EscapedPath(), "/") || strings.HasPrefix(filename, ".") ||
			len(filename) > 255 || strings.ContainsAny(filename, "\\\x00\r\n") {
			return remote.Command{}, "", errors.New("tools: download URL must end in a non-hidden filename, at most 255 bytes")
		}
	}
	flags := []string{"-q", "--silent", "--show-error", "--globoff", "--proto", "=http,https", "--proto-redir", "=http,https",
		"--connect-timeout", "3", "--max-time", "10", "--max-filesize", strconv.Itoa(remote.ArtifactLimit), "--retry", "0"}
	if method == "HEAD" {
		flags = append(flags, "--head")
	}
	if args.FollowRedirects {
		flags = append(flags, "--location", "--max-redirs", "5")
	}
	if args.Insecure {
		flags = append(flags, "--insecure")
	}
	var names []string
	for name, value := range args.Headers {
		if !httpHeaderName.MatchString(name) || strings.ContainsAny(value, "\x00\r\n") {
			return remote.Command{}, "", errors.New("tools: invalid HTTP header name or value")
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		flags = append(flags, "--header", name+": "+args.Headers[name])
	}
	flags = append(flags, "--url", args.URL)
	return remote.Command{Program: "curl", Args: flags, HTTPMetadata: true}, filename, nil
}

func (t *Tools) checkCurlVersion(ctx context.Context, host string) error {
	result, err := t.command(ctx, host, "curl", "-q", "--version")
	if err != nil {
		return err
	}
	fields := strings.Fields(result.Stdout)
	if result.Truncated || len(fields) < 2 || fields[0] != "curl" {
		return errors.New("tools: cannot determine curl version")
	}
	parts := strings.Split(fields[1], ".")
	if len(parts) < 2 {
		return errors.New("tools: cannot determine curl version")
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	if majorErr != nil || minorErr != nil || major < 8 || (major == 8 && minor < 4) {
		return errors.New("tools: curl >= 8.4 is required to bound downloads without Content-Length")
	}
	return nil
}

func (t *Tools) CurlHTTP(ctx context.Context, args toolArgs) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd, filename, err := curlCommand(args)
	if err != nil {
		return "", err
	}
	if err := t.checkCurlVersion(ctx, args.HostID); err != nil {
		return "", err
	}
	plan := remote.Pipeline{Stages: []remote.Stage{{Commands: []remote.Command{cmd}}}, Stdout: "inline", Stderr: "inline", Filename: filename}
	if args.Download {
		plan.Stdout = "file"
	}
	result, err := t.Runner.RunPipeline(ctx, args.HostID, plan)
	if len(result.Codes) > 0 && result.Codes[0] == 63 {
		result.Limited = true
	}
	if strings.EqualFold(args.Method, "HEAD") {
		result.Stdout = ""
	}
	out := pipelineOutput(args.HostID, "curl_http", nil, result)
	if args.Insecure {
		out = "TLS certificate verification disabled (insecure=true).\n" + out
	}
	if err != nil {
		return "", fmt.Errorf("%w\n%s", err, out)
	}
	if result.Codes[0] != 0 {
		return "", fmt.Errorf("tools: curl failed with exit code %d\n%s", result.Codes[0], out)
	}
	if result.HTTPMetadata == "" {
		return "", fmt.Errorf("tools: curl returned no HTTP metadata\n%s", out)
	}
	return out, nil
}
