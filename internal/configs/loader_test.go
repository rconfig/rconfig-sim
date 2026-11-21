package configs

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRelativeConfigPaths: a relative config_file is relative to the
// manifest's directory, whatever the working directory; an absolute one
// is used as given.
func TestRelativeConfigPaths(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "pkg")
	if err := os.MkdirAll(filepath.Join(pkg, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(dir, "elsewhere.cfg")
	for p, body := range map[string]string{filepath.Join(pkg, "configs", "a.cfg"): "hostname a\n", abs: "hostname b\n"} {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	man := filepath.Join(pkg, "manifest.csv")
	rows := "hostname,ip,port,vendor,template,username,password,enable_password,config_file,size_bucket\n" +
		"a,10.0.0.1,10000,Cisco,cisco_ios,admin,,,configs/a.cfg,sm\n" +
		"b,10.0.0.1,10001,Cisco,cisco_ios,admin,,," + abs + ",sm\n"
	if err := os.WriteFile(man, []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
	// Run from an unrelated directory.
	t.Chdir(t.TempDir())
	devs, err := LoadForListener(man, "10.0.0.1", 10000, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer UnloadAll(devs)
	if string(devs[0].Data) != "hostname a\n" || devs[0].ConfigPath != filepath.Join(pkg, "configs", "a.cfg") {
		t.Fatalf("relative path: %q from %s", devs[0].Data, devs[0].ConfigPath)
	}
	if string(devs[1].Data) != "hostname b\n" || devs[1].ConfigPath != abs {
		t.Fatalf("absolute path: %q from %s", devs[1].Data, devs[1].ConfigPath)
	}
	// A relative manifest path works too.
	t.Chdir(dir)
	devs2, err := LoadForListener(filepath.Join("pkg", "manifest.csv"), "10.0.0.1", 10000, 1)
	if err != nil || string(devs2[0].Data) != "hostname a\n" {
		t.Fatalf("relative manifest: %v", err)
	}
	UnloadAll(devs2)
}
