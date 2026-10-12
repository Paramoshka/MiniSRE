package remote

import (
	"net/url"
	"os"
	"sort"
	"strings"
)

// RedactRuntimeSecrets masks known credentials of the running tool executor.
func RedactRuntimeSecrets(value string, additional ...string) string {
	secrets := append([]string{os.Getenv("TELEGRAM_BOT_TOKEN"), os.Getenv("DEEPSEEK_API_KEY")}, additional...)
	for _, key := range []string{"TELEGRAM_HTTP_PROXY", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		proxy := strings.TrimSpace(os.Getenv(key))
		secrets = append(secrets, proxy)
		parsed, err := url.Parse(proxy)
		if err != nil || parsed.User == nil {
			continue
		}
		password, ok := parsed.User.Password()
		if ok && password != "" {
			secrets = append(secrets, password, strings.TrimPrefix(url.UserPassword("", password).String(), ":"))
		}
	}
	// Replace complete URLs/tokens before any credential that is their substring.
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	return value
}
