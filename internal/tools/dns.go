package tools

import (
	"context"
	"errors"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"mini-sre/internal/remote"
)

var dnsLabel = regexp.MustCompile(`^[A-Za-z0-9_-]{1,63}$`)

func dnsArguments(args toolArgs) (string, bool, error) {
	address, addressErr := netip.ParseAddr(args.Query)
	reverse := addressErr == nil
	if reverse {
		if address.Zone() != "" {
			return "", false, errors.New("tools: DNS query IP must not have a zone")
		}
	} else {
		name := strings.TrimSuffix(args.Query, ".")
		if args.Query != "." {
			if name == "" || len(name) > 253 || strings.HasPrefix(name, "-") {
				return "", false, errors.New("tools: query must be one ASCII DNS name or IP address")
			}
			for _, label := range strings.Split(name, ".") {
				if !dnsLabel.MatchString(label) {
					return "", false, errors.New("tools: query contains an invalid DNS label")
				}
			}
		}
	}
	recordType := strings.ToUpper(args.RecordType)
	if recordType == "" {
		recordType = "A"
		if reverse {
			recordType = "PTR"
		}
	}
	switch recordType {
	case "A", "AAAA", "CNAME", "MX", "NS", "TXT", "SOA", "SRV", "PTR", "CAA":
	default:
		return "", false, errors.New("tools: unsupported DNS record type")
	}
	if reverse && recordType != "PTR" {
		return "", false, errors.New("tools: IP queries require record_type=PTR")
	}
	if args.Server != "" {
		server, err := netip.ParseAddr(args.Server)
		if err != nil || server.Zone() != "" {
			return "", false, errors.New("tools: DNS server must be an IP address without a zone")
		}
	}
	if args.Port != nil && (*args.Port < 1 || *args.Port > 65535) {
		return "", false, errors.New("tools: port must be 1..65535")
	}
	return recordType, reverse, nil
}

func digCommand(args toolArgs) (remote.Command, error) {
	recordType, reverse, err := dnsArguments(args)
	if err != nil {
		return remote.Command{}, err
	}
	// Ignore .digrc so user configuration cannot enable tracing or extra queries.
	flags := []string{"-r", "+timeout=2", "+tries=1", "+nosearch"}
	if args.Server != "" {
		flags = append(flags, "@"+args.Server)
	}
	if args.Port != nil {
		flags = append(flags, "-p", strconv.Itoa(*args.Port))
	}
	if reverse {
		flags = append(flags, "-x", args.Query)
	} else {
		flags = append(flags, "-q", args.Query, "-t", recordType)
	}
	return remote.Command{Program: "dig", Args: flags}, nil
}

func (t *Tools) DigDNS(ctx context.Context, args toolArgs) (string, error) {
	cmd, err := digCommand(args)
	if err != nil {
		return "", err
	}
	result, err := t.command(ctx, args.HostID, cmd.Program, cmd.Args...)
	if err != nil {
		return "", err
	}
	return commandOutput(result), nil
}

func nslookupCommand(args toolArgs) (remote.Command, error) {
	recordType, _, err := dnsArguments(args)
	if err != nil {
		return remote.Command{}, err
	}
	flags := []string{"-timeout=2", "-retry=1", "-nosearch", "-type=" + recordType}
	if args.Port != nil {
		flags = append(flags, "-port="+strconv.Itoa(*args.Port))
	}
	flags = append(flags, args.Query)
	if args.Server != "" {
		flags = append(flags, args.Server)
	}
	return remote.Command{Program: "nslookup", Args: flags}, nil
}

func (t *Tools) NslookupDNS(ctx context.Context, args toolArgs) (string, error) {
	cmd, err := nslookupCommand(args)
	if err != nil {
		return "", err
	}
	result, err := t.command(ctx, args.HostID, cmd.Program, cmd.Args...)
	if err != nil {
		return "", err
	}
	return commandOutput(result), nil
}
