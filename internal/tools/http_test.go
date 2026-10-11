package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mini-sre/internal/remote"
)

func requireCurl(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip(err)
	}
	if err := (&Tools{Runner: &remote.Runner{}}).checkCurlVersion(context.Background(), "local"); err != nil {
		t.Skip(err)
	}
	t.Setenv("NO_PROXY", "*")
	t.Setenv("no_proxy", "*")
}

func TestCurlHTTPResponsesAndPipeline(t *testing.T) {
	requireCurl(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/answer", http.StatusFound)
			return
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
			return
		case "/file-redirect":
			w.Header().Set("Location", "file:///etc/passwd")
			w.WriteHeader(http.StatusFound)
			return
		case "/fail":
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "unavailable")
			return
		}
		w.Header().Set("X-Diagnostic", "present")
		fmt.Fprintf(w, "method=%s host=%s accept=%s\nneedle\n", r.Method, r.Host, r.Header.Get("Accept"))
	}))
	defer server.Close()
	for _, host := range []string{"local", "web"} {
		t.Run(host, func(t *testing.T) {
			runner := &remote.Runner{}
			if host == "web" {
				runner = fileSSHRunner(t)
			}
			tool := &Tools{Runner: runner}
			for _, tc := range []struct {
				path, method, want string
				follow, failure    bool
			}{
				{"/answer", "GET", "method=GET host=custom.example accept=application/json", false, false},
				{"/answer", "HEAD", "X-Diagnostic: present", false, false},
				{"/fail", "GET", "503\n", false, false},
				{"/redirect", "GET", "302\n", false, false},
				{"/redirect", "GET", "200\n", true, false},
				{"/loop", "GET", "exit code 47", true, true},
				{"/file-redirect", "GET", "exit code 1", true, true},
			} {
				out, err := tool.CurlHTTP(context.Background(), toolArgs{HostID: host, URL: server.URL + tc.path, Method: tc.method, FollowRedirects: tc.follow, Headers: map[string]string{"Host": "custom.example", "Accept": "application/json"}})
				if tc.failure {
					if err == nil {
						t.Fatalf("expected failure for %s", tc.path)
					}
					out = err.Error()
				} else if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(out, tc.want) {
					t.Fatalf("%s %s = %q", tc.method, tc.path, out)
				}
				if tc.method == "HEAD" && strings.Contains(out, "needle") {
					t.Fatalf("HEAD returned a body: %q", out)
				}
			}
			raw, err := json.Marshal(map[string]any{"host_id": host, "source": map[string]any{"tool": "curl_http", "args": map[string]any{"url": server.URL + "/answer"}}, "filters": []any{map[string]any{"type": "grep", "pattern": "needle"}}})
			if err != nil {
				t.Fatal(err)
			}
			out, err := Registry(runner)["run_pipeline"](context.Background(), raw)
			if err != nil || !strings.Contains(out, "stdout:\nneedle") || !strings.Contains(out, "HTTP metadata") {
				t.Fatalf("curl pipeline = %q, %v", out, err)
			}
		})
	}
}

func TestCurlDownloadExactBytesAndIsolation(t *testing.T) {
	requireCurl(t)
	payload := []byte{'a', 0, 0xff, '\n', 'b'}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="../../outside"`)
		w.Write(payload)
	}))
	defer server.Close()
	for _, host := range []string{"local", "web"} {
		t.Run(host, func(t *testing.T) {
			runner := &remote.Runner{}
			if host == "web" {
				runner = fileSSHRunner(t)
			}
			type download struct {
				out string
				err error
			}
			results := make(chan download, 3)
			for i := 0; i < 3; i++ {
				go func() {
					out, err := (&Tools{Runner: runner}).CurlHTTP(context.Background(), toolArgs{HostID: host, URL: server.URL + "/file%20name.txt?q=1", Download: true})
					results <- download{out, err}
				}()
			}
			seen := map[string]bool{}
			for i := 0; i < 3; i++ {
				result := <-results
				if result.err != nil {
					t.Fatal(result.err)
				}
				path := pipelineArtifactPath(t, result.out, "stdout_file=")
				t.Cleanup(func() { os.RemoveAll(filepath.Dir(filepath.Dir(path))) })
				if seen[path] || filepath.Base(path) != "file%20name.txt" {
					t.Fatalf("unexpected path %q", path)
				}
				seen[path] = true
				data, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(data, payload) {
					t.Fatalf("download = %v, %v", data, err)
				}
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("file permissions = %v, %v", info, err)
				}
			}
		})
	}
}

func TestCurlTLSAndCancellation(t *testing.T) {
	requireCurl(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "secure reply") }))
	defer server.Close()
	tool := &Tools{Runner: &remote.Runner{}}
	if _, err := tool.CurlHTTP(context.Background(), toolArgs{URL: server.URL}); err == nil {
		t.Fatal("untrusted certificate accepted")
	}
	out, err := tool.CurlHTTP(context.Background(), toolArgs{URL: server.URL, Insecure: true})
	if err != nil || !strings.Contains(out, "secure reply") || !strings.Contains(out, "verification disabled") {
		t.Fatalf("insecure = %q, %v", out, err)
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer slow.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := tool.CurlHTTP(ctx, toolArgs{URL: slow.URL}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel = %v", err)
	}
}

func TestCurlEnforcesSizeWithAndWithoutContentLength(t *testing.T) {
	requireCurl(t)
	for _, chunked := range []bool{false, true} {
		t.Run(fmt.Sprint(chunked), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !chunked {
					w.Header().Set("Content-Length", fmt.Sprint(remote.ArtifactLimit+1024*1024))
				}
				chunk := make([]byte, 1024*1024)
				for i := 0; i < 11; i++ {
					if _, err := w.Write(chunk); err != nil {
						return
					}
					if chunked {
						w.(http.Flusher).Flush()
					}
				}
			}))
			defer server.Close()
			_, err := (&Tools{Runner: &remote.Runner{}}).CurlHTTP(context.Background(), toolArgs{URL: server.URL + "/large.bin", Download: true})
			if err == nil || !strings.Contains(err.Error(), "limited=true") || !strings.Contains(err.Error(), "possibly_partial=true") {
				t.Fatalf("limit = %v", err)
			}
			path := pipelineArtifactPath(t, err.Error(), "stdout_file=")
			t.Cleanup(func() { os.RemoveAll(filepath.Dir(filepath.Dir(path))) })
			info, err := os.Stat(path)
			if err != nil || info.Size() > remote.ArtifactLimit {
				t.Fatalf("oversized file: %v, %v", info, err)
			}
		})
	}
}

func TestCurlRejectsUnsafeArgumentsAndOldVersion(t *testing.T) {
	argsFile := fakeCommand(t, "curl", "curl 7.88.1 test\n", "", "0")
	for _, args := range []toolArgs{
		{}, {URL: "file:///etc/passwd"}, {URL: "ftp://example.com/file"}, {URL: "http://user:password@example.com"},
		{URL: "http://example.com/a\n"}, {URL: "http://example.com", Method: "POST"},
		{URL: "http://example.com/.env", Download: true}, {URL: "http://example.com/", Download: true},
		{URL: "http://example.com/file", Download: true, Method: "HEAD"},
		{URL: "http://example.com", Headers: map[string]string{"@/etc/passwd": "x"}},
		{URL: "http://example.com", Headers: map[string]string{"Host": "x\r\nInjected: value"}},
	} {
		if _, err := (&Tools{Runner: &remote.Runner{}}).CurlHTTP(context.Background(), args); err == nil {
			t.Fatalf("accepted %+v", args)
		}
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatal("curl ran before validation")
	}
	_, err := (&Tools{Runner: &remote.Runner{}}).CurlHTTP(context.Background(), toolArgs{URL: "http://example.com"})
	if err == nil || !strings.Contains(err.Error(), "curl >= 8.4") {
		t.Fatalf("old curl = %v", err)
	}
	flags, err := os.ReadFile(argsFile)
	if err != nil || string(flags) != "-q\n--version\n" {
		t.Fatalf("old curl issued a request: %q, %v", flags, err)
	}
}

func TestCurlIgnoresCurlrcAndURLGlobbing(t *testing.T) {
	requireCurl(t)
	curlHome := t.TempDir()
	t.Setenv("CURL_HOME", curlHome)
	unexpected := filepath.Join(curlHome, "unexpected")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	if err := os.WriteFile(filepath.Join(curlHome, ".curlrc"), []byte("request = POST\noutput = "+unexpected+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := (&Tools{Runner: &remote.Runner{}}).CurlHTTP(context.Background(), toolArgs{URL: server.URL + "/file{one,two}"})
	if err != nil || !strings.Contains(out, "GET /file{one,two}") {
		t.Fatalf("curlrc/globbing = %q, %v", out, err)
	}
	if _, err := os.Stat(unexpected); !os.IsNotExist(err) {
		t.Fatal("curlrc redirected output")
	}
}
