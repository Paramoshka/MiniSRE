package tools

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"mini-sre/internal/remote"
)

func testCertificate(t *testing.T, notBefore, notAfter time.Time) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "Test CA"},
		NotBefore:    notBefore.Add(-time.Hour),
		NotAfter:     notAfter.Add(time.Hour),
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(12345),
		Subject:      pkix.Name{CommonName: "example.com"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		DNSNames:     []string{"example.com", "www.example.com"},
		IPAddresses:  []net.IP{net.ParseIP("192.0.2.10")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestFormatTLSCertificate(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	certificate := testCertificate(t, now.Add(-24*time.Hour), now.Add(42*24*time.Hour))
	raw := certificate + "depth=0 CN = example.com\n" +
		"Verify return code: 0 (ok)\n" +
		"Protocol  : TLSv1.3\n" +
		"Cipher    : TLS_AES_256_GCM_SHA384\n"
	out, err := formatTLSCertificate(raw, now)
	if err != nil {
		t.Fatalf("formatTLSCertificate: %v", err)
	}
	for _, want := range []string{
		"subject=CN=example.com",
		"issuer=CN=Test CA",
		"valid_until=2026-11-28T12:00:00Z",
		"validity=expires in 42 days",
		"serial=3039",
		"san=192.0.2.10, example.com, www.example.com",
		"key=RSA 2048 bits",
		"protocol=TLSv1.3 cipher=TLS_AES_256_GCM_SHA384",
		"chain_certificates=1",
		"verification=ok (code 0)",
		"Notes: certificate validity",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("TLS report missing %q:\n%s", want, out)
		}
	}

	out, err = formatTLSCertificate(strings.Replace(raw, "Verify return code: 0 (ok)", "Verify return code: 18 (self-signed certificate)", 1), now)
	if err != nil || !strings.Contains(out, "verification=failed: self-signed certificate (code 18)") {
		t.Fatalf("verification failure = %q, %v", out, err)
	}
	out, err = formatTLSCertificate(strings.Replace(raw, "Verify return code: 0 (ok)\n", "", 1), now)
	if err != nil || !strings.Contains(out, "verification=unavailable") {
		t.Fatalf("missing verify result = %q, %v", out, err)
	}
}

func TestFormatTLSCertificateValidity(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	out, err := formatTLSCertificate(testCertificate(t, now.Add(-10*24*time.Hour), now.Add(-3*24*time.Hour)), now)
	if err != nil || !strings.Contains(out, "validity=expired 3 days ago") {
		t.Fatalf("expired certificate = %q, %v", out, err)
	}
	out, err = formatTLSCertificate(testCertificate(t, now.Add(24*time.Hour), now.Add(48*time.Hour)), now)
	if err != nil || !strings.Contains(out, "validity=not yet valid") {
		t.Fatalf("not-yet-valid certificate = %q, %v", out, err)
	}
	out, err = formatTLSCertificate(testCertificate(t, now.Add(-2*time.Hour), now.Add(36*time.Hour)), now)
	if err != nil || !strings.Contains(out, "validity=expires in 1 day") {
		t.Fatalf("singular validity = %q, %v", out, err)
	}
	out, err = formatTLSCertificate(testCertificate(t, now.Add(-48*time.Hour), now.Add(-36*time.Hour)), now)
	if err != nil || !strings.Contains(out, "validity=expired 1 day ago") {
		t.Fatalf("singular expiry = %q, %v", out, err)
	}
	if _, err := formatTLSCertificate("no certificate here", now); err == nil {
		t.Fatal("missing certificate accepted")
	}
}

func TestTLSCommand(t *testing.T) {
	port := 8443
	cmd, target, err := tlsCommand(toolArgs{Destination: "example.com", Port: &port})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Program != "timeout" || target.sni != "example.com" {
		t.Fatalf("command = %+v, target = %+v", cmd, target)
	}
	args := strings.Join(cmd.Args, "\n")
	for _, want := range []string{
		tlsConnectScript, "s_client", "-connect\nexample.com:8443", "-showcerts", "-verify\n5",
		"-servername\nexample.com", "-verify_hostname\nexample.com",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("TLS arguments missing %q:\n%s", want, args)
		}
	}

	cmd, target, err = tlsCommand(toolArgs{Destination: "192.0.2.10"})
	if err != nil {
		t.Fatal(err)
	}
	if target.expectedIP.String() != "192.0.2.10" || target.sni != "" {
		t.Fatalf("IP target = %+v", target)
	}
	args = strings.Join(cmd.Args, "\n")
	if !strings.Contains(args, "-connect\n192.0.2.10:443") || !strings.Contains(args, "-verify_ip\n192.0.2.10") || strings.Contains(args, "-servername") {
		t.Fatalf("IP TLS arguments: %s", args)
	}

	cmd, target, err = tlsCommand(toolArgs{Destination: "192.0.2.10", ServerName: "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if target.sni != "example.com" {
		t.Fatalf("SNI override = %+v", target)
	}
	args = strings.Join(cmd.Args, "\n")
	if !strings.Contains(args, "-servername\nexample.com") || !strings.Contains(args, "-verify_hostname\nexample.com") {
		t.Fatalf("SNI TLS arguments: %s", args)
	}
}

func TestTLSCertificateTool(t *testing.T) {
	now := time.Now()
	certificate := testCertificate(t, now.Add(-time.Hour), now.Add(24*time.Hour))
	raw := certificate + "Verify return code: 0 (ok)\nProtocol  : TLSv1.3\nCipher    : TLS_AES_256_GCM_SHA384\n"
	argsFile := fakeCommand(t, "timeout", raw, "", "0")
	out, err := Registry(&remote.Runner{})["check_tls_certificate"](context.Background(), json.RawMessage(`{"destination":"example.com"}`))
	if err != nil || !strings.Contains(out, "TLS certificate from local to example.com:443") ||
		!strings.Contains(out, "verification=ok (code 0)") || !strings.Contains(out, "server_name=example.com") {
		t.Fatalf("TLS tool = %q, %v", out, err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "-connect\nexample.com:443\n") || !strings.Contains(string(args), "-verify_hostname\nexample.com\n") {
		t.Fatalf("TLS command arguments: %s", args)
	}
}

func TestTLSCertificateToolFailures(t *testing.T) {
	call := Registry(&remote.Runner{})["check_tls_certificate"]
	fakeCommand(t, "timeout", "", "connect: Connection refused\n", "1")
	out, err := call(context.Background(), json.RawMessage(`{"destination":"example.com"}`))
	if err != nil || !strings.Contains(out, "failed (exit code 1)") || !strings.Contains(out, "DNS, routing") {
		t.Fatalf("connection failure = %q, %v", out, err)
	}

	fakeCommand(t, "timeout", "", "", "124")
	out, err = call(context.Background(), json.RawMessage(`{"destination":"example.com"}`))
	if err != nil || !strings.Contains(out, "timed out") {
		t.Fatalf("timeout = %q, %v", out, err)
	}

	fakeCommand(t, "timeout", "", "openssl: not found\n", "127")
	if _, err := call(context.Background(), json.RawMessage(`{"destination":"example.com"}`)); err == nil {
		t.Fatal("missing OpenSSL was reported as a diagnostic result")
	}

	fakeCommand(t, "timeout", "CONNECTED(00000003)\n", "", "0")
	if _, err := call(context.Background(), json.RawMessage(`{"destination":"example.com"}`)); err == nil {
		t.Fatal("missing certificate was accepted")
	}

	fakeCommand(t, "timeout", strings.Repeat("x", 70000), "", "0")
	if _, err := call(context.Background(), json.RawMessage(`{"destination":"example.com"}`)); err == nil {
		t.Fatal("truncated TLS output was accepted")
	}
}

func TestTLSCertificateRejectsArgumentsBeforeExecution(t *testing.T) {
	argsFile := fakeCommand(t, "timeout", "", "", "0")
	call := Registry(&remote.Runner{})["check_tls_certificate"]
	for _, raw := range []string{
		`{}`, `{"destination":""}`, `{"destination":"bad/name"}`,
		`{"destination":"example.com","port":0}`, `{"destination":"example.com","port":65536}`,
		`{"destination":"example.com","port":"443"}`, `{"destination":"example.com","port":1.5}`,
		`{"destination":"example.com","server_name":"bad/name"}`,
		`{"destination":"example.com","server_name":"fe80::1%eth0"}`,
		`{"destination":"example.com","command":"id"}`, `null`, `[]`, `{bad`,
	} {
		if _, err := call(context.Background(), json.RawMessage(raw)); err == nil {
			t.Fatalf("invalid arguments accepted: %s", raw)
		}
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatal("timeout ran before argument validation")
	}
}
