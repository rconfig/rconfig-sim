package sshsrv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCommandSlug(t *testing.T) {
	cases := map[string]string{
		"show lldp neighbors detail":          "show_lldp_neighbors_detail",
		"  Show   LLDP  Neighbors  ":          "show_lldp_neighbors",
		"show cdp neighbors detail | no-more": "show_cdp_neighbors_detail",
		"show lldp neighbors |   no-more  ":   "show_lldp_neighbors",
		"/interface ethernet print":           "/interface_ethernet_print",
		"show configuration | display set":    "show_configuration_|_display_set",
		"":                                    "",
		"\tshow\tversion\t":                   "show_version",
	}
	for in, want := range cases {
		if got := commandSlug(in); got != want {
			t.Errorf("commandSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func writeCmdFile(t *testing.T, root, host, name, body string) {
	t.Helper()
	dir := filepath.Join(root, host)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCmdFilesLookup(t *testing.T) {
	root := t.TempDir()
	writeCmdFile(t, root, "sw-01", "show_lldp_neighbors_detail.txt", "line one\nline two")

	c := newCmdFiles(root)

	// Hit: LF normalised to CRLF, trailing CRLF added.
	got, ok := c.lookup("sw-01", "Show LLDP neighbors detail | no-more")
	if !ok || string(got) != "line one\r\nline two\r\n" {
		t.Fatalf("hit: ok=%v got=%q", ok, got)
	}

	// Served from cache after the file is gone.
	if err := os.Remove(filepath.Join(root, "sw-01", "show_lldp_neighbors_detail.txt")); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.lookup("sw-01", "show lldp neighbors detail"); !ok {
		t.Error("cached hit: want ok")
	}

	// Misses: unknown command, unknown host folder, path escape, empty line.
	for _, tc := range []struct{ host, line string }{
		{"sw-01", "show version"},
		{"sw-02", "show lldp neighbors detail"},
		{"sw-01", "../sw-01/show_lldp_neighbors_detail"},
		{"sw-01", "   "},
	} {
		if _, ok := c.lookup(tc.host, tc.line); ok {
			t.Errorf("lookup(%q, %q): want miss", tc.host, tc.line)
		}
	}

	// Missing root: silently off.
	if _, ok := newCmdFiles(filepath.Join(root, "nope")).lookup("sw-01", "show version"); ok {
		t.Error("missing root: want miss")
	}
}

func TestCmdFilesFlagOff(t *testing.T) {
	c := newCmdFiles("")
	if c != nil {
		t.Fatal("empty root should disable the feature")
	}
	if _, ok := c.lookup("sw-01", "show version"); ok {
		t.Error("nil cmdFiles: want miss")
	}
}

func TestCmdFilesMissingHostFolder(t *testing.T) {
	root := t.TempDir()
	writeCmdFile(t, root, "sw-01", "show_version.txt", "v1")
	c := newCmdFiles(root)

	if _, ok := c.lookup("sw-02", "show version"); ok {
		t.Fatal("device with no folder: want miss")
	}
	// The folder appearing later is not seen until restart.
	writeCmdFile(t, root, "sw-02", "show_version.txt", "v1")
	if _, ok := c.lookup("sw-02", "show version"); ok {
		t.Error("folder created after first lookup: want miss until restart")
	}
	if _, ok := newCmdFiles(root).lookup("sw-02", "show version"); !ok {
		t.Error("after restart: want hit")
	}
}

// Documented behaviour: a device's folder is listed once, so a file added after
// the device's first command is not served until the process restarts.
func TestCmdFilesAddedAfterStart(t *testing.T) {
	root := t.TempDir()
	writeCmdFile(t, root, "sw-01", "show_version.txt", "v1")
	c := newCmdFiles(root)

	if _, ok := c.lookup("sw-01", "show version"); !ok {
		t.Fatal("existing file: want hit")
	}
	writeCmdFile(t, root, "sw-01", "show_lldp_neighbors.txt", "n1")
	if _, ok := c.lookup("sw-01", "show lldp neighbors"); ok {
		t.Error("file added after start: want miss until restart")
	}
	if got, ok := newCmdFiles(root).lookup("sw-01", "show lldp neighbors"); !ok || string(got) != "n1\r\n" {
		t.Errorf("after restart: ok=%v got=%q", ok, got)
	}
}

func TestCmdFilesEditedAfterServe(t *testing.T) {
	root := t.TempDir()
	writeCmdFile(t, root, "sw-01", "show_version.txt", "v1")
	c := newCmdFiles(root)

	if got, _ := c.lookup("sw-01", "show version"); string(got) != "v1\r\n" {
		t.Fatalf("first read = %q", got)
	}
	writeCmdFile(t, root, "sw-01", "show_version.txt", "v2")
	if got, _ := c.lookup("sw-01", "show version"); string(got) != "v1\r\n" {
		t.Errorf("edited after serve: got %q, want cached v1", got)
	}
}

func TestCmdFilesIgnoresDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sw-01", "show_version.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := newCmdFiles(root).lookup("sw-01", "show version"); ok {
		t.Error("directory named like a command file: want miss")
	}
}

func TestToCRLF(t *testing.T) {
	cases := map[string]string{
		"a\nb":     "a\r\nb\r\n",
		"a\r\nb\n": "a\r\nb\r\n",
		"":         "\r\n",
	}
	for in, want := range cases {
		if got := string(toCRLF([]byte(in))); got != want {
			t.Errorf("toCRLF(%q) = %q, want %q", in, got, want)
		}
	}
}
