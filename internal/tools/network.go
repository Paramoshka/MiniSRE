package tools

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

var (
	networkInterface = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:@-]{0,14}$`)
	routingTable     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)
	pingHostname     = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?\.?$`)
)

func networkFamily(family string, allowAll bool) (string, error) {
	switch family {
	case "":
		if allowAll {
			return "", nil
		}
		return "-4", nil
	case "ipv4":
		return "-4", nil
	case "ipv6":
		return "-6", nil
	case "all":
		if allowAll {
			return "", nil
		}
	}
	return "", errors.New("tools: invalid address family")
}

func validateInterface(name string) error {
	if name != "" && !networkInterface.MatchString(name) {
		return errors.New("tools: interface must be one interface name, at most 15 bytes")
	}
	return nil
}

func validateRoutingTable(table string, allowAll bool) error {
	if table == "" {
		return nil
	}
	if table == "all" {
		if allowAll {
			return nil
		}
		return errors.New("tools: all tables is only supported by get_ip_routes")
	}
	if routingTable.MatchString(table) {
		return nil
	}
	id, err := strconv.ParseUint(table, 10, 32)
	if err != nil || id == 0 || strconv.FormatUint(id, 10) != table {
		return errors.New("tools: table must be a routing table name or a positive decimal 32-bit ID without leading zeros")
	}
	return nil
}

func (t *Tools) IPRules(ctx context.Context, args toolArgs) (string, error) {
	family, err := networkFamily(args.Family, false)
	if err != nil {
		return "", err
	}
	if err := validateRoutingTable(args.Table, false); err != nil {
		return "", err
	}
	flags := []string{family, "rule", "show"}
	if args.Table != "" {
		flags = append(flags, "table", args.Table)
	}
	result, err := t.command(ctx, args.HostID, "ip", flags...)
	if err != nil {
		return "", err
	}
	return commandOutput(result), nil
}

func (t *Tools) IPRoutes(ctx context.Context, args toolArgs) (string, error) {
	family, err := networkFamily(args.Family, false)
	if err != nil {
		return "", err
	}
	table := args.Table
	if table == "" {
		table = "main"
	}
	if err := validateRoutingTable(table, true); err != nil {
		return "", err
	}
	if err := validateInterface(args.Interface); err != nil {
		return "", err
	}
	flags := []string{family, "route", "show", "table", table}
	if args.Interface != "" {
		flags = append(flags, "dev", args.Interface)
	}
	result, err := t.command(ctx, args.HostID, "ip", flags...)
	if err != nil {
		return "", err
	}
	return commandOutput(result), nil
}

func (t *Tools) RouteTo(ctx context.Context, args toolArgs) (string, error) {
	destination, err := netip.ParseAddr(args.Destination)
	if err != nil || destination.Zone() != "" {
		return "", errors.New("tools: destination must be an IP address without a zone; use output_interface for link-local routes")
	}
	family := "-6"
	if destination.Is4() {
		family = "-4"
	}
	flags := []string{family, "route", "get", destination.String()}
	if args.Source != "" {
		source, err := netip.ParseAddr(args.Source)
		if err != nil || source.Zone() != "" || source.Is4() != destination.Is4() {
			return "", errors.New("tools: source must be an IP address of the same family as destination, without a zone")
		}
		flags = append(flags, "from", source.String())
	}
	if args.Mark != nil {
		flags = append(flags, "mark", strconv.FormatUint(uint64(*args.Mark), 10))
	}
	for _, item := range []struct{ name, value string }{{"iif", args.InputInterface}, {"oif", args.OutputInterface}} {
		if err := validateInterface(item.value); err != nil {
			return "", err
		}
		if item.value != "" {
			flags = append(flags, item.name, item.value)
		}
	}
	result, err := t.command(ctx, args.HostID, "ip", flags...)
	if err != nil {
		return "", err
	}
	return commandOutput(result), nil
}

func (t *Tools) PingHost(ctx context.Context, args toolArgs) (string, error) {
	address, addressErr := netip.ParseAddr(args.Destination)
	if addressErr != nil {
		if len(args.Destination) > 253 || !pingHostname.MatchString(args.Destination) {
			return "", errors.New("tools: destination must be one IP address or DNS hostname")
		}
	} else if address.Zone() != "" {
		return "", errors.New("tools: use interface instead of a zone in the ping destination")
	}
	family, err := networkFamily(args.Family, true)
	if err != nil {
		return "", err
	}
	if addressErr == nil && ((family == "-4" && !address.Is4()) || (family == "-6" && address.Is4())) {
		return "", errors.New("tools: destination does not match the selected address family")
	}
	if err := validateInterface(args.Interface); err != nil {
		return "", err
	}
	count := 3
	if args.Count != nil {
		count = *args.Count
	}
	if count < 1 || count > 5 {
		return "", errors.New("tools: count must be 1..5")
	}
	flags := []string{"-n", "-c", strconv.Itoa(count), "-W", "2", "-w", "8"}
	if family != "" {
		flags = append(flags, family)
	}
	if args.Interface != "" {
		flags = append(flags, "-I", args.Interface)
	}
	flags = append(flags, "--", args.Destination)
	result, err := t.command(ctx, args.HostID, "ping", flags...)
	if err != nil {
		if result.ExitCode == 1 {
			return "ICMP probe did not receive all requested replies (exit code 1).\n" + commandOutput(result), nil
		}
		return "", err
	}
	return commandOutput(result), nil
}

func (t *Tools) NetworkSockets(ctx context.Context, args toolArgs) (string, error) {
	family, err := networkFamily(args.Family, true)
	if err != nil {
		return "", err
	}
	flags := []string{"-n", "-p"}
	switch args.Protocol {
	case "", "both":
		flags = append(flags, "-t", "-u")
	case "tcp":
		flags = append(flags, "-t")
	case "udp":
		flags = append(flags, "-u")
	default:
		return "", errors.New("tools: protocol must be tcp, udp or both")
	}
	if family != "" {
		flags = append(flags, family)
	}
	switch args.State {
	case "", "listening":
		flags = append(flags, "-l")
	case "all":
		flags = append(flags, "-a")
	case "established", "syn-sent", "syn-recv", "time-wait", "close-wait":
		if args.Protocol != "tcp" {
			return "", errors.New("tools: TCP connection states require protocol=tcp")
		}
		flags = append(flags, "-a", "state", args.State)
	default:
		return "", errors.New("tools: invalid socket state")
	}
	if args.Port != nil {
		if *args.Port < 1 || *args.Port > 65535 {
			return "", errors.New("tools: port must be 1..65535")
		}
		flags = append(flags, fmt.Sprintf("( sport = :%d or dport = :%d )", *args.Port, *args.Port))
	}
	result, err := t.command(ctx, args.HostID, "ss", flags...)
	if err != nil {
		return "", err
	}
	return commandOutput(result), nil
}

func (t *Tools) NetworkInterfaces(ctx context.Context, args toolArgs) (string, error) {
	family, err := networkFamily(args.Family, true)
	if err != nil {
		return "", err
	}
	if err := validateInterface(args.Interface); err != nil {
		return "", err
	}
	flags := []string{"address", "show"}
	if family != "" {
		flags = append([]string{family}, flags...)
	}
	if args.Interface != "" {
		flags = append(flags, "dev", args.Interface)
	}
	addresses, err := t.command(ctx, args.HostID, "ip", flags...)
	if err != nil {
		return "", err
	}
	flags = []string{"-s", "link", "show"}
	if args.Interface != "" {
		flags = append(flags, "dev", args.Interface)
	}
	links, err := t.command(ctx, args.HostID, "ip", flags...)
	if err != nil {
		return "", err
	}
	return "Addresses:\n" + commandOutput(addresses) + "\nLink statistics:\n" + commandOutput(links), nil
}

func (t *Tools) IPNeighbors(ctx context.Context, args toolArgs) (string, error) {
	family, err := networkFamily(args.Family, false)
	if err != nil {
		return "", err
	}
	if err := validateInterface(args.Interface); err != nil {
		return "", err
	}
	flags := []string{family, "neigh", "show"}
	if args.Interface != "" {
		flags = append(flags, "dev", args.Interface)
	}
	result, err := t.command(ctx, args.HostID, "ip", flags...)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(result.Stdout) == "" && result.Stderr == "" {
		return "No neighbor entries.", nil
	}
	return commandOutput(result), nil
}
