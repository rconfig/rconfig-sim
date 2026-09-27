//go:build integration

package sshsrv_test

import (
	"bytes"
	"encoding/csv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/sshsrv"
)

// meshCampusDir is the checked-in scenario, relative to this package.
const meshCampusDir = "../../scenarios/mesh-campus"

// meshCampusServer serves scenarios/mesh-campus with --commands-root on free loopback ports.
// The checked-in manifest targets the lab (10.1.1.2, fixed ports, absolute config paths), so
// a copy is written with the ip, port and config_file columns overridden; hostnames and
// templates are used as checked in. Returns hostname -> port.
func meshCampusServer(t *testing.T) map[string]int {
	t.Helper()
	root, err := filepath.Abs(meshCampusDir)
	if err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(filepath.Join(root, "manifest.csv"))
	if err != nil {
		t.Fatalf("open scenario manifest: %v", err)
	}
	rows, err := csv.NewReader(f).ReadAll()
	f.Close()
	if err != nil {
		t.Fatalf("read scenario manifest: %v", err)
	}

	devices := len(rows) - 1
	portStart := freePorts(t, devices)
	ports := map[string]int{}
	for i, row := range rows[1:] {
		port := portStart + i
		row[1] = "127.0.0.1"
		row[2] = strconv.Itoa(port)
		row[8] = filepath.Join(root, "configs", filepath.Base(row[8]))
		ports[row[0]] = port
	}

	tmp := t.TempDir()
	manifest := filepath.Join(tmp, "manifest.csv")
	out, err := os.Create(manifest)
	if err != nil {
		t.Fatal(err)
	}
	w := csv.NewWriter(out)
	if err := w.WriteAll(rows); err != nil {
		t.Fatal(err)
	}
	out.Close()

	srv, err := sshsrv.New(sshsrv.Config{
		ListenIP: "127.0.0.1", PortStart: portStart, PortCount: devices,
		ManifestPath: manifest, HostKeyPath: filepath.Join(tmp, "host"),
		Username: "admin", Password: "", EnablePassword: "",
		ResponseDelayMinMS: 0, ResponseDelayMaxMS: 0,
		MaxConcurrentSessions: 8,
		CommandsRoot:          filepath.Join(root, "commands"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(5 * time.Second) })

	return ports
}

// TestMeshCampus_CommandFiles drives every device in the scenario over SSH and asserts, for
// every command file it has, that the session shows exactly: the echoed command, the file
// (LF converted to CRLF, as the server sends it), and the next prompt. It also pins each
// driver's prompt and that its paging command answers silently.
func TestMeshCampus_CommandFiles(t *testing.T) {
	ports := meshCampusServer(t)

	cases := []struct {
		host, prompt, paging string
	}{
		{"core-01", "core-01>", "terminal length 0"},
		{"dist-01", "admin@dist-01> ", "set cli screen-length 0"},
		{"access-01", "access-01>", "terminal length 0"},
	}
	if len(cases) != len(ports) {
		t.Fatalf("scenario has %d devices, test covers %d", len(ports), len(cases))
	}

	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			port, ok := ports[tc.host]
			if !ok {
				t.Fatalf("%s not in scenario manifest", tc.host)
			}

			files, err := filepath.Glob(filepath.Join(meshCampusDir, "commands", tc.host, "*.txt"))
			if err != nil || len(files) == 0 {
				t.Fatalf("no command files for %s: %v", tc.host, err)
			}

			ec := dialExpect(t, port, "admin", "anything")
			defer ec.close()

			ec.expect(tc.prompt, 3*time.Second)
			ec.reset()

			ec.send(tc.paging)
			if got, want := ec.expect(tc.prompt, 3*time.Second), tc.paging+"\r\n"+tc.prompt; got != want {
				t.Errorf("%q should answer silently:\n got %q\nwant %q", tc.paging, got, want)
			}
			ec.reset()

			for _, file := range files {
				body, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				// The slug rule is reversible for these names: "_" was a space.
				cmd := strings.ReplaceAll(strings.TrimSuffix(filepath.Base(file), ".txt"), "_", " ")
				// What the server sends for a file: CRLF line endings, ending in CRLF.
				crlf := bytes.ReplaceAll(bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n")), []byte("\n"), []byte("\r\n"))
				if !bytes.HasSuffix(crlf, []byte("\r\n")) {
					crlf = append(crlf, '\r', '\n')
				}

				ec.send(cmd)
				want := cmd + "\r\n" + string(crlf) + tc.prompt
				if got := ec.expect(want, 3*time.Second); got != want {
					t.Errorf("%s %q:\n got %q\nwant %q", tc.host, cmd, got, want)
				}
				ec.reset()
			}
		})
	}
}
