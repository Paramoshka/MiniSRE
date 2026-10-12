// Package lab implements the restricted, host-side containerlab toolset.
package lab

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

const (
	ConfigPath       = "/etc/mini-sre/lab.yaml"
	StateDir         = "/var/lib/mini-sre/labs"
	MaxRequestBytes  = 1024 * 1024
	MaxTopologyBytes = 256 * 1024
)

type Request struct {
	Action     string  `json:"action"`
	Topology   string  `json:"topo_yaml,omitempty"`
	TTLMinutes int     `json:"ttl_minutes,omitempty"`
	Name       string  `json:"name,omitempty"`
	AllMine    bool    `json:"all_mine,omitempty"`
	Node       string  `json:"node,omitempty"`
	Checks     []Check `json:"checks,omitempty"`
}

type Check struct {
	Type        string `json:"type"`
	Destination string `json:"destination,omitempty"`
	Interface   string `json:"interface,omitempty"`
}

type CommandResult struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exit_code"`
	TimedOut  bool   `json:"timed_out"`
	Truncated bool   `json:"truncated"`
}

type CheckResult struct {
	Check Check `json:"check"`
	CommandResult
}

type Record struct {
	Name      string    `json:"name"`
	ExpiresAt time.Time `json:"expires_at"`
	Topology  Topology  `json:"topology"`
	Config    Config    `json:"config"`
	NetworkID string    `json:"network_id,omitempty"`
}

func DecodeRequest(reader io.Reader) (Request, error) {
	var req Request
	data, err := io.ReadAll(io.LimitReader(reader, MaxRequestBytes+1))
	if err != nil {
		return req, err
	}
	if len(data) > MaxRequestBytes {
		return req, errors.New("lab: request exceeds 1 MiB")
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, errors.New("lab: invalid request JSON")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return req, errors.New("lab: expected one request")
	}
	return req, ValidateRequest(req)
}

func ValidateRequest(req Request) error {
	switch req.Action {
	case "deploy":
		if req.Topology == "" || len(req.Topology) > MaxTopologyBytes || (req.TTLMinutes != 0 && (req.TTLMinutes < 5 || req.TTLMinutes > 120)) || req.Name != "" || req.AllMine || req.Node != "" || len(req.Checks) != 0 {
			return errors.New("lab: deploy requires topo_yaml (up to 256 KiB) and ttl_minutes=5..120 (default 60)")
		}
	case "inspect":
		if req.Topology != "" || req.TTLMinutes != 0 || req.AllMine || req.Node != "" || len(req.Checks) != 0 || (req.Name != "" && !validName(req.Name)) {
			return errors.New("lab: invalid inspect request")
		}
	case "run":
		if req.Topology != "" || req.TTLMinutes != 0 || req.AllMine || !validName(req.Name) || !validName(req.Node) || len(req.Checks) == 0 || len(req.Checks) > 4 {
			return errors.New("lab: run requires name, node and 1..4 checks")
		}
		for _, check := range req.Checks {
			if _, err := checkCommand(check); err != nil {
				return err
			}
		}
	case "destroy":
		if req.Topology != "" || req.TTLMinutes != 0 || req.Node != "" || len(req.Checks) != 0 || (req.AllMine && req.Name != "") || (!req.AllMine && req.Name == "") || (req.Name != "" && !strings.HasPrefix(req.Name, "msre-lab-")) || (req.Name != "" && !validName(req.Name)) {
			return errors.New("lab: destroy requires name or all_mine=true, exclusively")
		}
	default:
		return errors.New("lab: unknown action")
	}
	return nil
}
