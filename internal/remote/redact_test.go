package remote

import (
	"strings"
	"testing"

	"mini-sre/internal/config"
)

func TestRuntimeSecretRedaction(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "fake-bot-token")
	t.Setenv("DEEPSEEK_API_KEY", "fake-api-key")
	t.Setenv("TELEGRAM_HTTP_PROXY", "http://user:fake%40proxy-pass@proxy.test:8080")
	t.Setenv("HTTPS_PROXY", "http://user:fake-standard-pass@proxy.test:8081")
	runner := &Runner{Config: config.Config{Hosts: map[string]config.Host{"web": {Password: "fake-ssh-pass"}}}}
	secrets := []string{"fake-bot-token", "fake-api-key", "http://user:fake%40proxy-pass@proxy.test:8080", "fake@proxy-pass", "fake%40proxy-pass", "http://user:fake-standard-pass@proxy.test:8081", "fake-standard-pass", "fake-ssh-pass"}
	out := runner.Redact("diagnostic " + strings.Join(secrets, " | "))
	for _, secret := range secrets {
		if strings.Contains(out, secret) {
			t.Fatalf("secret survived masking: %q", out)
		}
	}
	if strings.Count(out, "[redacted]") != len(secrets) || !strings.HasPrefix(out, "diagnostic ") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestRuntimeRedactionEmptyAndMalformedProxy(t *testing.T) {
	for _, key := range []string{"TELEGRAM_BOT_TOKEN", "DEEPSEEK_API_KEY", "TELEGRAM_HTTP_PROXY", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(key, "")
	}
	if got := RedactRuntimeSecrets("ordinary diagnostics"); got != "ordinary diagnostics" {
		t.Fatal(got)
	}
	t.Setenv("TELEGRAM_HTTP_PROXY", "http://user:fake%zz@proxy.test")
	if got := RedactRuntimeSecrets("http://user:fake%zz@proxy.test"); got != "[redacted]" {
		t.Fatal(got)
	}
}

func TestOverlappingRuntimeAndSSHSecrets(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "fake-prefix-long-api")
	runner := &Runner{Config: config.Config{Hosts: map[string]config.Host{"web": {Password: "fake-prefix"}}}}
	if got := runner.Redact("fake-prefix-long-api fake-prefix"); got != "[redacted] [redacted]" {
		t.Fatal(got)
	}
	if got := RedactRuntimeSecrets("fake-prefix-long-api-extra", "fake-prefix-long-api-extra"); got != "[redacted]" {
		t.Fatal(got)
	}
}
