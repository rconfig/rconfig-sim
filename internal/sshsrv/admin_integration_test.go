//go:build integration

package sshsrv_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/activity"
	"github.com/rcfg-sim/rcfg-sim/internal/admin"
	"github.com/rcfg-sim/rcfg-sim/internal/configs"
	"github.com/rcfg-sim/rcfg-sim/internal/sshsrv"
)

// adminServer stands up a one-device server with a metrics listener and,
// when withAdmin is set, the admin API mounted as cmd/rcfg-sim does it.
// Returns the server, its SSH port and the admin base URL.
func adminServer(t *testing.T, withAdmin bool) (*sshsrv.Server, int, string) {
	t.Helper()
	tmp := t.TempDir()
	sshPort := freePort(t)
	metricsPort := freePort(t)

	manifest := filepath.Join(tmp, "manifest.csv")
	if _, err := configs.Run(configs.Config{
		Count: 1, OutputDir: filepath.Join(tmp, "configs"), ManifestPath: manifest,
		IPBase: "127.0.0.1", IPCount: 1, PortStart: sshPort, DevicesPerIP: 1,
		Seed: 42, Distribution: "sm:100,md:0,lg:0,xl:0",
		Username: "admin", Password: "admin", EnablePassword: "enable123",
	}, io.Discard); err != nil {
		t.Fatalf("generator: %v", err)
	}
	cfg := sshsrv.Config{
		ListenIP: "127.0.0.1", PortStart: sshPort, PortCount: 1,
		ManifestPath: manifest, HostKeyPath: filepath.Join(tmp, "host"),
		Username: "admin", Password: "admin", EnablePassword: "enable123",
		MaxConcurrentSessions: 8,
		MetricsAddr:           fmt.Sprintf("127.0.0.1:%d", metricsPort),
	}
	srv, err := sshsrv.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if withAdmin {
		srv.Mount(admin.Prefix, admin.Handler(srv, admin.Options{
			Version: "test", ListenIP: cfg.ListenIP, PortStart: cfg.PortStart, PortCount: cfg.PortCount,
			Devices: srv.DeviceCount(), Token: "tok",
		}))
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(5 * time.Second) })
	return srv, sshPort, fmt.Sprintf("http://127.0.0.1:%d/admin/v1/", metricsPort)
}

func adminCall(t *testing.T, method, url, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestAdmin_OffByDefault(t *testing.T) {
	_, _, base := adminServer(t, false)
	for _, p := range []string{"state", "devices", "events", "faults"} {
		if code, _ := adminCall(t, "GET", base+p, ""); code != http.StatusNotFound {
			t.Errorf("/admin/v1/%s with admin off: want 404, got %d", p, code)
		}
	}
}

// TestAdmin_FaultsOverAPI changes faults over the API and checks a new SSH
// session gets them, the device table records the result, and an event
// stream sees the session.
func TestAdmin_FaultsOverAPI(t *testing.T) {
	_, port, base := adminServer(t, true)

	// Baseline: a clean session.
	if err := dialErr(port); err != nil {
		t.Fatalf("baseline dial: %v", err)
	}

	// The stream reads below block; cut the stream off if events never come.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"events?sample=1", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	if !sc.Scan() || !strings.HasPrefix(sc.Text(), ":") {
		t.Fatalf("events: no opening comment: %q", sc.Text())
	}

	code, b := adminCall(t, "PUT", base+"faults", `{"auth_fail": 1}`)
	if code != http.StatusOK {
		t.Fatalf("PUT faults: %d %s", code, b)
	}
	var st admin.State
	if err := json.Unmarshal(b, &st); err != nil || st.Faults["auth_fail"] != 1 || st.Generation != 2 {
		t.Fatalf("PUT faults state: %+v, err %v", st, err)
	}

	driveExpectFail(t, port, "admin", "admin", "handshake failed")

	// The stream sees session_start, the auth_fail fault and session_end.
	seen := map[string]activity.Event{}
	deadline := time.Now().Add(3 * time.Second)
	for len(seen) < 3 && time.Now().Before(deadline) && sc.Scan() {
		d, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev activity.Event
		if err := json.Unmarshal([]byte(d), &ev); err != nil {
			t.Fatalf("event %q: %v", d, err)
		}
		seen[ev.Kind] = ev
	}
	if ev := seen[activity.KindFault]; ev.Fault != "auth_fail" || ev.Port != port || ev.Host == "" {
		t.Errorf("fault event: %+v", ev)
	}
	if ev := seen[activity.KindSessionEnd]; ev.Outcome != "auth_fail" || ev.Fault != "auth_fail" {
		t.Errorf("session_end event: %+v", ev)
	}
	if _, ok := seen[activity.KindSessionStart]; !ok {
		t.Errorf("no session_start event; saw %v", seen)
	}

	code, b = adminCall(t, "GET", base+"devices", "")
	if code != http.StatusOK {
		t.Fatalf("GET devices: %d %s", code, b)
	}
	var d admin.Devices
	_ = json.Unmarshal(b, &d)
	states, _ := base64.StdEncoding.DecodeString(d.State)
	if len(states) != 1 || activity.State(states[0]) != activity.StateAuthFail || d.PortStart != port {
		t.Fatalf("devices after auth_fail session: %+v states %v", d, states)
	}

	// Clear faults: new sessions succeed again.
	if code, b := adminCall(t, "PUT", base+"faults", `{}`); code != http.StatusOK {
		t.Fatalf("clear faults: %d %s", code, b)
	}
	if err := dialErr(port); err != nil {
		t.Fatalf("dial after clearing faults: %v", err)
	}
}

// TestAdmin_DeviceStateHang checks a session hung by a hang profile leaves
// its device in the hang state, and others record their outcome.
func TestAdmin_DeviceStateHang(t *testing.T) {
	srv, port := withProfiledServer(t, "", 2, []string{"hang:300ms", ""})
	tb := srv.DeviceTable()
	if tb.State(0) != activity.StateNever {
		t.Fatalf("before any session: %d", tb.State(0))
	}
	for i := 0; i < 2; i++ {
		_, _ = driveAndCollect(port+i, "admin", "admin", []string{"show version", "exit"}, 3*time.Second)
	}
	deadline := time.Now().Add(2 * time.Second)
	for (tb.State(0) != activity.StateHang || tb.State(1) != activity.StateOK) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if tb.State(0) != activity.StateHang || tb.State(1) != activity.StateOK || tb.LastSeen(0) == 0 {
		t.Fatalf("states: device0 %d (want hang), device1 %d (want ok), last seen %d", tb.State(0), tb.State(1), tb.LastSeen(0))
	}
}
