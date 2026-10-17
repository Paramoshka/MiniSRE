package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIInfoProcess(t *testing.T) {
	infoFlag := os.Getenv("MINI_SRE_TEST_INFO_FLAG")
	if infoFlag == "" {
		return
	}
	flag.CommandLine = flag.NewFlagSet("mini-sre", flag.ExitOnError)
	os.Args = []string{"mini-sre", infoFlag, "-config", "/nonexistent/mini-sre.yaml"}
	if os.Getenv("MINI_SRE_TEST_BUILD_VERSION") != "" {
		version, commit = "v0.1.0", "abc1234"
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestCLIInfoWithoutConfiguration(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("not a valid dotenv line\n"), 0600); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		flag, buildVersion, want string
	}{
		{"--version", "", "MiniSRE dev (unknown)\n"},
		{"--version", "yes", "MiniSRE v0.1.0 (abc1234)\n"},
		{"--help", "", "-version"},
		{"-h", "", "-version"},
	} {
		t.Run(tc.flag+tc.buildVersion, func(t *testing.T) {
			cmd := exec.Command(binary, "-test.run=^TestCLIInfoProcess$")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "MINI_SRE_TEST_INFO_FLAG="+tc.flag,
				"MINI_SRE_TEST_BUILD_VERSION="+tc.buildVersion, "DEEPSEEK_API_KEY=", "DEEPSEEK_TIMEOUT=invalid")
			output, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(output), tc.want) {
				t.Fatalf("info command: output=%q err=%v", output, err)
			}
		})
	}
}
