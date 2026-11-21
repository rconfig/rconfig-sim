//go:build integration

package sshsrv_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/admin"
	"github.com/rcfg-sim/rcfg-sim/internal/configs"
	"github.com/rcfg-sim/rcfg-sim/internal/sshsrv"
)

// TestAllowEmpty: with no devices in the manifest for its port range, the
// server refuses to start unless AllowEmpty is set. With it, it binds no
// SSH listener but serves /healthz and the admin API, reporting 0 devices.
func TestAllowEmpty(t *testing.T) {
	tmp := t.TempDir()
	devPort, emptyStart, metricsPort := freePort(t), freePort(t), freePort(t)
	manifest := filepath.Join(tmp, "manifest.csv")
	if _, err := configs.Run(configs.Config{
		Count: 1, OutputDir: filepath.Join(tmp, "configs"), ManifestPath: manifest,
		IPBase: "127.0.0.1", IPCount: 1, PortStart: devPort, DevicesPerIP: 1,
		Seed: 42, Distribution: "sm:100,md:0,lg:0,xl:0",
		Username: "admin", Password: "admin", EnablePassword: "enable123",
	}, io.Discard); err != nil {
		t.Fatalf("generator: %v", err)
	}
	cfg := sshsrv.Config{
		ListenIP: "127.0.0.1", PortStart: emptyStart, PortCount: 1,
		ManifestPath: manifest, HostKeyPath: filepath.Join(tmp, "host"),
		Username: "admin", Password: "admin", EnablePassword: "enable123",
		MaxConcurrentSessions: 8,
		MetricsAddr:           fmt.Sprintf("127.0.0.1:%d", metricsPort),
	}
	if _, err := sshsrv.New(cfg); err == nil || !strings.Contains(err.Error(), "no devices in manifest match") {
		t.Fatalf("without AllowEmpty: %v", err)
	}

	cfg.AllowEmpty = true
	srv, err := sshsrv.New(cfg)
	if err != nil {
		t.Fatalf("New with AllowEmpty: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle(admin.Prefix, admin.Handler(srv, admin.Options{
		Version: "test", ListenIP: cfg.ListenIP, PortStart: cfg.PortStart, PortCount: cfg.PortCount,
		Devices: srv.DeviceCount(),
	}))
	if err := srv.ServeAdmin("127.0.0.1:0", mux); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(5 * time.Second) })

	if srv.DeviceCount() != 0 {
		t.Fatalf("devices: %d", srv.DeviceCount())
	}
	if code, _ := adminCall(t, "GET", fmt.Sprintf("http://127.0.0.1:%d/healthz", metricsPort), ""); code != http.StatusOK {
		t.Fatalf("/healthz: %d", code)
	}
	code, body := adminCall(t, "GET", "http://"+srv.AdminAddr()+"/admin/v1/state", "")
	var st admin.State
	if code != http.StatusOK || json.Unmarshal(body, &st) != nil || st.Devices != 0 {
		t.Fatalf("admin state: %d %s", code, body)
	}
	if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", emptyStart), time.Second); err == nil {
		c.Close()
		t.Fatal("an SSH listener is bound with no devices")
	}
}
