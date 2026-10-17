package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mini-sre/internal/remote"
)

func TestListDirectoryLocal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".hidden"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".hidden", filepath.Join(dir, "secret")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/home/example/.ssh/id", filepath.Join(dir, "abssecret")); err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(map[string]any{"path": dir})
	if err != nil {
		t.Fatal(err)
	}
	out, err := Registry(&remote.Runner{})["list_directory"](context.Background(), raw)
	if err != nil {
		t.Fatalf("list_directory: %v", err)
	}
	for _, want := range []string{
		"Directory " + dir + " on local",
		"file 5 B",
		"file.txt",
		"sub",
		"link -> file.txt",
		"secret -> [hidden target]",
		"abssecret -> [hidden target]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("directory listing missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\nfile .hidden") || strings.Contains(out, " -> .hidden") {
		t.Fatalf("hidden entry or target leaked:\n%s", out)
	}
}

func TestListDirectoryLimitAndEmpty(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(map[string]any{"path": dir, "limit": 2})
	if err != nil {
		t.Fatal(err)
	}
	out, err := Registry(&remote.Runner{})["list_directory"](context.Background(), raw)
	if err != nil {
		t.Fatalf("list_directory: %v", err)
	}
	if !strings.Contains(out, "listing limited to 2 entries; 1 additional entries omitted") {
		t.Fatalf("missing limit marker:\n%s", out)
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "file ") {
			fields := strings.Fields(line)
			names = append(names, fields[len(fields)-1])
		}
	}
	if strings.Join(names, ",") != "a,b" {
		t.Fatalf("wrong entries after limit: %v\n%s", names, out)
	}

	empty := t.TempDir()
	raw, err = json.Marshal(map[string]any{"path": empty})
	if err != nil {
		t.Fatal(err)
	}
	out, err = Registry(&remote.Runner{})["list_directory"](context.Background(), raw)
	if err != nil || !strings.Contains(out, "is empty (hidden entries are not listed)") {
		t.Fatalf("empty directory = %q, %v", out, err)
	}
}

func TestListDirectoryRejectsInvalidPathsAndArguments(t *testing.T) {
	findArgs := fakeCommand(t, "find", "", "", "0")
	call := Registry(&remote.Runner{})["list_directory"]
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".hidden"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{}`, `{"path":""}`, `{"path":"relative"}`, `{"path":"` + file + `"}`,
		`{"path":"` + filepath.Join(dir, ".hidden") + `"}`, `{"path":"` + filepath.Join(dir, "missing") + `"}`,
		`{"path":"` + dir + `","limit":0}`, `{"path":"` + dir + `","limit":501}`,
		`{"path":"` + dir + `","limit":1.5}`, `{"path":"` + dir + `","limit":"1"}`,
		`{"path":"` + dir + `","recursive":true}`, `null`, `[]`, `{bad`,
	} {
		if _, err := call(context.Background(), json.RawMessage(raw)); err == nil {
			t.Fatalf("invalid arguments accepted: %s", raw)
		}
	}
	if _, err := os.Stat(findArgs); !os.IsNotExist(err) {
		t.Fatal("find ran before path or argument validation")
	}
}

func TestParseDirectory(t *testing.T) {
	raw := "d\x004096\x001700000000.5\x00sub\x00\x00" +
		"f\x005\x001700000000\x00file.txt\x00\x00" +
		"l\x005\x001700000000\x00link\x00file.txt\x00"
	entries, complete := parseDirectory(raw)
	if !complete || len(entries) != 3 {
		t.Fatalf("parseDirectory = %+v, complete=%t", entries, complete)
	}
	if entries[0].kind != "d" || entries[0].name != "sub" || entries[0].modified.IsZero() {
		t.Fatalf("directory entry = %+v", entries[0])
	}
	if entries[1].size != 5 || entries[1].name != "file.txt" {
		t.Fatalf("file entry = %+v", entries[1])
	}
	if entries[2].target != "file.txt" {
		t.Fatalf("link entry = %+v", entries[2])
	}

	entries, complete = parseDirectory(raw + "f\x005")
	if complete || len(entries) != 3 {
		t.Fatalf("truncated parse = %+v, complete=%t", entries, complete)
	}
}

func TestDirectoryTargetAndNameSanitizing(t *testing.T) {
	for _, target := range []string{".", "..", ".ssh/key", "/root/.config/x", "foo/.bar"} {
		if !hiddenTarget(target) {
			t.Errorf("hiddenTarget(%q) = false, want true", target)
		}
	}
	for _, target := range []string{"", "file", "/var/log/nginx/access.log", "foo/../bar"} {
		if hiddenTarget(target) != (target == "foo/../bar") {
			t.Errorf("hiddenTarget(%q) = %t", target, hiddenTarget(target))
		}
	}
	if got := sanitizeName("a\nb\tc\\d"); got != `a\x0ab\x09c\\d` {
		t.Fatalf("sanitizeName = %q", got)
	}
}
