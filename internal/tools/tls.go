package tools

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"mini-sre/internal/remote"
)

const tlsConnectScript = `exec openssl "$@" </dev/null`

var (
	tlsVerifyCode = regexp.MustCompile(`(?m)^\s*Verify return code:\s*(\d+)\s*\((.*)\)\s*$`)
	tlsProtocol   = regexp.MustCompile(`(?m)^\s*Protocol\s*:\s*(\S+)`)
	tlsCipher     = regexp.MustCompile(`(?m)^\s*Cipher\s*:\s*(\S+)`)
	tlsNewSession = regexp.MustCompile(`New,\s*([^,]+),\s*Cipher is\s+(\S+)`)
)

type tlsTarget struct {
	destination string
	port        int
	sni         string
	expected    string
	expectedIP  netip.Addr
}

func tlsTargetFromArgs(args toolArgs) (tlsTarget, error) {
	if err := validateConnectDestination(args.Destination); err != nil {
		return tlsTarget{}, err
	}
	destination := strings.TrimSuffix(args.Destination, ".")
	port := 443
	if args.Port != nil {
		port = *args.Port
	}
	if port < 1 || port > 65535 {
		return tlsTarget{}, errors.New("tools: port must be 1..65535")
	}
	expected := args.ServerName
	if expected != "" {
		if err := validateConnectDestination(expected); err != nil {
			return tlsTarget{}, errors.New("tools: server_name must be one IP address or DNS hostname")
		}
		expected = strings.TrimSuffix(expected, ".")
	} else {
		expected = destination
	}
	target := tlsTarget{destination: destination, port: port, expected: expected}
	if address, err := netip.ParseAddr(expected); err == nil {
		target.expectedIP = address
	} else {
		target.sni = expected
	}
	return target, nil
}

func tlsCommand(args toolArgs) (remote.Command, tlsTarget, error) {
	target, err := tlsTargetFromArgs(args)
	if err != nil {
		return remote.Command{}, tlsTarget{}, err
	}
	opensslArgs := []string{"s_client", "-connect", net.JoinHostPort(target.destination, strconv.Itoa(target.port)), "-showcerts", "-verify", "5"}
	if target.sni != "" {
		opensslArgs = append(opensslArgs, "-servername", target.sni)
	}
	if target.expectedIP.IsValid() {
		opensslArgs = append(opensslArgs, "-verify_ip", target.expectedIP.String())
	} else {
		opensslArgs = append(opensslArgs, "-verify_hostname", target.expected)
	}
	commandArgs := append([]string{"8", "bash", "--noprofile", "--norc", "-c", tlsConnectScript, "mini-sre-tls"}, opensslArgs...)
	return remote.Command{Program: "timeout", Args: commandArgs}, target, nil
}

func (t *Tools) TLSCertificate(ctx context.Context, args toolArgs) (string, error) {
	cmd, target, err := tlsCommand(args)
	if err != nil {
		return "", err
	}
	start := time.Now()
	result, err := t.Runner.Run(ctx, args.HostID, cmd.Program, cmd.Args...)
	elapsed := time.Since(start)
	if err != nil && result.ExitCode != 1 && result.ExitCode != 124 {
		return "", fmt.Errorf("tools: TLS probe could not run on the selected host (OpenSSL, Bash and timeout are required): %w; %s", err, commandOutput(result))
	}
	if result.Truncated {
		return "", errors.New("tools: OpenSSL output was truncated")
	}
	host := args.HostID
	if host == "" {
		host = "local"
	}
	header := fmt.Sprintf("TLS certificate from %s to %s (elapsed %s including command startup)\n", host, net.JoinHostPort(target.destination, strconv.Itoa(target.port)), elapsed.Round(time.Millisecond))
	if result.ExitCode == 124 {
		return header + "TLS certificate check timed out (exit code 124).\n" + strings.TrimSpace(result.Stderr) + "\n", nil
	}
	body, parseErr := formatTLSCertificate(result.Stdout, time.Now())
	if parseErr != nil {
		if err != nil && result.ExitCode == 1 && !hasCertificate(result.Stdout) {
			out := header + fmt.Sprintf("TLS certificate check failed (exit code 1):\n%s\n", strings.TrimSpace(commandOutput(result)))
			out += "A failed TLS handshake can mean DNS, routing, filtering or service availability problems."
			return out, nil
		}
		return "", parseErr
	}
	out := header + body
	if target.sni != "" {
		out = header + "server_name=" + target.sni + "\n" + body
	}
	if stderr := strings.TrimSpace(result.Stderr); stderr != "" && strings.Contains(body, "verification=unavailable") {
		out += "\nOpenSSL diagnostics:\n" + stderr
	}
	return out, nil
}

func hasCertificate(raw string) bool {
	block, _ := pem.Decode([]byte(raw))
	return block != nil && block.Type == "CERTIFICATE"
}

func formatTLSCertificate(raw string, now time.Time) (string, error) {
	block, rest := pem.Decode([]byte(raw))
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("tools: no certificate in OpenSSL output")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", errors.New("tools: cannot parse the reported TLS certificate")
	}
	chain := 1
	for len(rest) > 0 {
		next, remaining := pem.Decode(rest)
		if next == nil || next.Type != "CERTIFICATE" {
			break
		}
		chain++
		rest = remaining
	}

	until := cert.NotAfter.Sub(now)
	validity := ""
	switch {
	case now.After(cert.NotAfter):
		validity = "expired " + durationDays(now.Sub(cert.NotAfter)) + " ago"
	case now.Before(cert.NotBefore):
		validity = "not yet valid"
	case until < 24*time.Hour:
		validity = "expires in less than 1 day"
	default:
		validity = "expires in " + durationDays(until)
	}

	verification := "unavailable (OpenSSL did not report a verify result)"
	if match := tlsVerifyCode.FindStringSubmatch(raw); match != nil {
		code, _ := strconv.Atoi(match[1])
		if code == 0 {
			verification = "ok (code 0)"
		} else {
			verification = fmt.Sprintf("failed: %s (code %d)", strings.TrimSpace(match[2]), code)
		}
	}

	sans := append([]string{}, cert.DNSNames...)
	for _, address := range cert.IPAddresses {
		sans = append(sans, address.String())
	}
	sans = append(sans, cert.EmailAddresses...)
	for _, uri := range cert.URIs {
		sans = append(sans, uri.String())
	}
	sort.Strings(sans)
	if len(sans) > 20 {
		sans = append(sans[:20], fmt.Sprintf("... (%d more)", len(sans)-20))
	}
	sanText := strings.Join(sans, ", ")
	if sanText == "" {
		sanText = "(none)"
	}

	protocol, cipher := "", ""
	if match := tlsProtocol.FindStringSubmatch(raw); match != nil {
		protocol = match[1]
	}
	if match := tlsCipher.FindStringSubmatch(raw); match != nil {
		cipher = match[1]
	}
	if match := tlsNewSession.FindStringSubmatch(raw); match != nil {
		if protocol == "" {
			protocol = strings.TrimSpace(match[1])
		}
		if cipher == "" {
			cipher = match[2]
		}
	}
	connection := ""
	if protocol != "" || cipher != "" {
		connection = fmt.Sprintf("protocol=%s cipher=%s\n", protocol, cipher)
	}

	var out strings.Builder
	fmt.Fprintf(&out, "subject=%s\n", cert.Subject.String())
	fmt.Fprintf(&out, "issuer=%s\n", cert.Issuer.String())
	fmt.Fprintf(&out, "valid_from=%s\n", cert.NotBefore.UTC().Format(time.RFC3339))
	fmt.Fprintf(&out, "valid_until=%s\n", cert.NotAfter.UTC().Format(time.RFC3339))
	fmt.Fprintf(&out, "validity=%s\n", validity)
	fmt.Fprintf(&out, "serial=%s\n", cert.SerialNumber.Text(16))
	fmt.Fprintf(&out, "san=%s\n", sanText)
	fmt.Fprintf(&out, "key=%s\n", publicKeyDescription(cert))
	out.WriteString(connection)
	fmt.Fprintf(&out, "chain_certificates=%d\n", chain)
	fmt.Fprintf(&out, "verification=%s\n", verification)
	out.WriteString("Notes: certificate validity and chain verification depend on the probe host's trust store and do not prove application health. The probe retrieves the certificate but sends no application data.")
	return out.String(), nil
}

func durationDays(duration time.Duration) string {
	days := int(math.Floor(duration.Hours() / 24))
	switch days {
	case 0:
		return "less than 1 day"
	case 1:
		return "1 day"
	default:
		return fmt.Sprintf("%d days", days)
	}
}

func publicKeyDescription(cert *x509.Certificate) string {
	switch key := cert.PublicKey.(type) {
	case *rsa.PublicKey:
		return fmt.Sprintf("RSA %d bits", key.N.BitLen())
	case *ecdsa.PublicKey:
		return "ECDSA " + key.Curve.Params().Name
	case ed25519.PublicKey:
		return "Ed25519"
	default:
		return cert.PublicKeyAlgorithm.String()
	}
}
