package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"mini-sre/internal/lab"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, input io.Reader, output io.Writer) error {
	if len(args) != 1 || (args[0] != "request" && args[0] != "gc") {
		return errors.New("usage: mini-sre-lab request|gc")
	}
	if os.Geteuid() != 0 {
		return errors.New("lab: helper must run as root through the restricted sudo rule")
	}
	if err := lab.CheckTrustedPath(lab.ConfigPath, false); err != nil {
		return err
	}
	if err := lab.CheckTrustedPath(lab.StateDir, true); err != nil {
		return err
	}
	data, err := os.ReadFile(lab.ConfigPath)
	if err != nil {
		return err
	}
	cfg, err := lab.ParseConfig(string(data))
	if err != nil {
		return err
	}
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	ctx, cancel := context.WithTimeout(signalCtx, 120*time.Second)
	defer cancel()
	manager := lab.New(cfg)
	var result any
	if args[0] == "gc" {
		result, err = manager.GC(ctx)
	} else {
		// An interrupted SSH client must not leave a helper blocked on stdin forever.
		type decoded struct {
			request lab.Request
			err     error
		}
		requests := make(chan decoded, 1)
		go func() { req, err := lab.DecodeRequest(input); requests <- decoded{req, err} }()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case decoded := <-requests:
			if decoded.err != nil {
				return decoded.err
			}
			result, err = manager.Handle(ctx, decoded.request)
		}
	}
	if err != nil {
		return err
	}
	encoded, err := lab.EncodeResponse(result)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, string(encoded))
	return err
}
