# Feature Tests

Per-feature manual verification procedures. Each test isolates a specific behaviour (command dispatch, label cardinality, fault semantics, serial agreement, etc.) and defines a pass condition. Use this during development, PR review, or after a code change to confirm invariants still hold. For end-to-end load-testing scenarios — smoke test through 50k-device production runs — see TEST-SCENARIOS.md.

**Prerequisites:**
- Binaries built: `cd /opt/rcfg-sim && CGO_ENABLED=0 go build -trimpath -o bin/rcfg-sim ./cmd/rcfg-sim && CGO_ENABLED=0 go build -trimpath -o bin/rcfg-sim-gen ./cmd/rcfg-sim-gen`
- You are running from `/opt/rcfg-sim/`.
- For SSH-client samples you will need a real SSH client. `ssh` from OpenSSH works but needs interactive password entry; the samples below use a small Go test client at `/tmp/rcfg-sim-client/client` built on first use (see [Build the SSH test client](#build-the-ssh-test-client)).

---

## Table of contents

### Generator (`rcfg-sim-gen`)
- [G1 — smoke: generate 10 configs on localhost](#g1)
- [G2 — smoke: generate 100 configs, default distribution](#g2)
- [G3 — determinism: same seed, byte-identical output](#g3)
- [G4 — distribution override](#g4)
- [G5 — projected 50k run (do not actually run unless you have 20 GB free)](#g5)
- [G6 — invalid distribution errors cleanly](#g6)

### Server (`rcfg-sim`)
- [S1 — start server on 10 ports; verify listening](#s1)
- [S2 — host-key auto-generation on first run](#s2)
- [S3 — `show version` with hostname substitution](#s3)
- [S4 — `show running-config` streams full mmap'd config](#s4)
- [S5 — `show startup-config` returns the same bytes as running-config](#s5)
- [S6 — abbreviated commands (`sh ver`, `sh run`, `term len 0`)](#s6)
- [S7 — enable mode (correct and wrong password)](#s7)
- [S8 — unknown command returns Cisco-style error](#s8)
- [S9 — ambiguous command returns "Ambiguous command"](#s9)
- [S10 — `exit` from enable returns to unprivileged prompt](#s10)
- [S11 — `exit` / `quit` / `logout` / `end` from unprivileged closes session](#s11)
- [S12 — wrong SSH password is rejected](#s12)
- [S13 — 20 concurrent sessions against different ports](#s13)
- [S14 — graceful SIGTERM shutdown unmaps cleanly](#s14)
- [S15 — `show inventory` renders canned output with device serial](#s15)
- [S16 — `show version` and `show inventory` agree on chassis serial](#s16)
- [S17 — `end` in enable mode drops to user-exec (does not close session)](#s17)
- [S18 — `terminal pager 0` is a silent ack](#s18)

### Metrics (`/metrics`, `/healthz`)
- [M1 — `/healthz` returns 200 OK](#m1)
- [M2 — pre-traffic scrape shows all 8 families + zero-count label sets](#m2)
- [M3 — `rcfgsim_sessions_total` moves by result](#m3)
- [M4 — `rcfgsim_auth_attempts_total{ok|fail}` tracks both outcomes](#m4)
- [M5 — `rcfgsim_command_duration_seconds` only uses canonical Cmd\* labels](#m5)
- [M6 — `rcfgsim_bytes_sent_total` grows with config streaming](#m6)
- [M7 — `rcfgsim_faults_injected_total` is pre-registered at zero](#m7)
- [M8 — label cardinality stays bounded under load](#m8)

### Fault injection (`--fault-rate`, `--fault-types`)
- [F1 — `auth_fail` at rate 1.0 rejects every login](#f1)
- [F2 — `auth_fail` at rate 0.5 rejects ~50%](#f2)
- [F3 — `disconnect_mid` truncates `show running-config` + RSTs the TCP conn](#f3)
- [F4 — `slow_response` stretches command latency 10-50×](#f4)
- [F5 — `malformed` corrupts config output (truncate / inject / bit-flip)](#f5)
- [F6 — zero rate with all types configured: no faults fire](#f6)
- [F7 — hot path with fault-flags present but rate=0 matches no-flag baseline](#f7)

### End-to-end
- [E1 — generate + serve + SSH in, verify hostname matches manifest](#e1)

---

<a name="build-the-ssh-test-client"></a>
## Build the SSH test client (one-time)

OpenSSH requires interactive password entry, which is awkward in non-interactive
samples. These samples use a small Go SSH client that takes `--pass` on the CLI.

```bash
mkdir -p /tmp/rcfg-sim-client && cd /tmp/rcfg-sim-client
cat > client.go <<'EOF'
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

func main() {
	host := flag.String("host", "127.0.0.1", "host")
	port := flag.Int("port", 12000, "port")
	user := flag.String("user", "admin", "user")
	pass := flag.String("pass", "admin", "password")
	enablePass := flag.String("enable", "enable123", "enable password")
	cmdsStr := flag.String("cmds", "terminal length 0|show version|exit", "pipe-separated commands")
	doEnable := flag.Bool("enable-mode", false, "enter enable mode first")
	readTimeout := flag.Duration("read", 800*time.Millisecond, "pause after each command for response")
	flag.Parse()

	cfg := &ssh.ClientConfig{
		User: *user, Auth: []ssh.AuthMethod{ssh.Password(*pass)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second,
	}
	c, err := ssh.Dial("tcp", fmt.Sprintf("%s:%d", *host, *port), cfg)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer c.Close()
	sess, err := c.NewSession()
	if err != nil {
		log.Fatalf("session: %v", err)
	}
	defer sess.Close()
	_ = sess.RequestPty("xterm", 80, 24, ssh.TerminalModes{ssh.ECHO: 0})
	stdin, _ := sess.StdinPipe()
	stdout, _ := sess.StdoutPipe()
	if err := sess.Shell(); err != nil {
		log.Fatalf("shell: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		r := bufio.NewReader(stdout)
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				os.Stdout.Write(buf[:n])
			}
			if err != nil {
				if err != io.EOF {
					fmt.Fprintf(os.Stderr, "read err: %v\n", err)
				}
				return
			}
		}
	}()

	time.Sleep(250 * time.Millisecond)
	if *doEnable {
		fmt.Fprintln(stdin, "enable")
		time.Sleep(150 * time.Millisecond)
		fmt.Fprintln(stdin, *enablePass)
		time.Sleep(250 * time.Millisecond)
	}
	for _, cmd := range strings.Split(*cmdsStr, "|") {
		if cmd = strings.TrimSpace(cmd); cmd == "" {
			continue
		}
		fmt.Fprintln(stdin, cmd)
		time.Sleep(*readTimeout)
	}
	_ = stdin.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	_ = sess.Wait()
}
EOF
go mod init smoketest 2>/dev/null
go get golang.org/x/crypto/ssh@v0.24.0 >/dev/null 2>&1
go build -o client client.go
cd /opt/rcfg-sim
```

Verify: `/tmp/rcfg-sim-client/client --help` should print usage.

---

## Generator samples

<a name="g1"></a>
### G1 — smoke: generate 10 configs on localhost

Produces 10 device configs mapped to 127.0.0.1 ports 12000–12009. Fast (<1s).

```bash
rm -rf /tmp/rcfg-sim-g1 && mkdir -p /tmp/rcfg-sim-g1
./bin/rcfg-sim-gen \
  --count 10 --seed 7 \
  --ip-base 127.0.0.1 --ip-count 1 --devices-per-ip 10 --port-start 12000 \
  --output-dir /tmp/rcfg-sim-g1/configs \
  --manifest   /tmp/rcfg-sim-g1/manifest.csv
head -3 /tmp/rcfg-sim-g1/manifest.csv
ls -lh /tmp/rcfg-sim-g1/configs | head -5
```

Expect: generator summary with realised distribution, 10 `.cfg` files, 11-row manifest (header + 10).

<a name="g2"></a>
### G2 — smoke: generate 100 configs, default distribution

Confirms the standard 40/40/15/5 mix. Useful baseline for template inspection.

```bash
rm -rf /tmp/rcfg-sim-g2 && mkdir -p /tmp/rcfg-sim-g2
./bin/rcfg-sim-gen \
  --count 100 --seed 42 \
  --output-dir /tmp/rcfg-sim-g2/configs \
  --manifest   /tmp/rcfg-sim-g2/manifest.csv
# one sample from each bucket
for b in small medium large huge; do
  f=$(awk -F, -v b=$b '$10==b{print $9; exit}' /tmp/rcfg-sim-g2/manifest.csv)
  printf "%-6s: %s\n" "$b" "$f"
done
```

Expect: 40 small, 40 medium, 15 large, 5 huge. Δ=+0.00 pp on every bucket.

<a name="g3"></a>
### G3 — determinism: same seed, byte-identical output

The generator uses the seed to drive both bucket shuffling and per-device random data. Two independent runs with identical seeds must produce identical config bytes.

```bash
rm -rf /tmp/rcfg-sim-g3a /tmp/rcfg-sim-g3b
for d in g3a g3b; do
  mkdir -p /tmp/rcfg-sim-$d
  ./bin/rcfg-sim-gen --count 20 --seed 99 \
    --output-dir /tmp/rcfg-sim-$d/configs \
    --manifest /tmp/rcfg-sim-$d/manifest.csv > /dev/null
done
# Hash every config file from each run.
( cd /tmp/rcfg-sim-g3a/configs && find . -name '*.cfg' | sort | xargs sha256sum | sha256sum ) > /tmp/g3a.sum
( cd /tmp/rcfg-sim-g3b/configs && find . -name '*.cfg' | sort | xargs sha256sum | sha256sum ) > /tmp/g3b.sum
diff /tmp/g3a.sum /tmp/g3b.sum && echo "DETERMINISTIC: identical config bytes across runs"
```

Expect: `DETERMINISTIC: ...` printed.

<a name="g4"></a>
### G4 — distribution override

Skew entirely to small to verify distribution parsing and stratified counts.

```bash
rm -rf /tmp/rcfg-sim-g4 && mkdir -p /tmp/rcfg-sim-g4
./bin/rcfg-sim-gen --count 50 --seed 1 \
  --distribution "small:100,medium:0,large:0,huge:0" \
  --output-dir /tmp/rcfg-sim-g4/configs \
  --manifest   /tmp/rcfg-sim-g4/manifest.csv \
  | tail -6
awk -F, 'NR>1{c[$10]++} END{for (b in c) print b, c[b]}' /tmp/rcfg-sim-g4/manifest.csv
```

Expect: `small 50` only; other buckets absent.

<a name="g5"></a>
### G5 — projected 50k run (warning: ~20 GB disk)

Full production-scale generation. Parallelised (worker pool sized to `runtime.NumCPU()`); target is roughly 8 minutes on the 12-vCPU host. Skip on small disks.

```bash
df -h /opt/rcfg-sim  # need ~20 GB free
rm -rf /opt/rcfg-sim/configs /opt/rcfg-sim/manifest.csv
time ./bin/rcfg-sim-gen --count 50000 --seed 42 \
  --output-dir /opt/rcfg-sim/configs \
  --manifest   /opt/rcfg-sim/manifest.csv
wc -l /opt/rcfg-sim/manifest.csv   # want 50001 (50000 + header)
du -sh /opt/rcfg-sim/configs       # want ~19 GB
```

Expect: runtime ≈ 8 min on 12 cores, distribution 20000/20000/7500/2500.

<a name="g6"></a>
### G6 — invalid distribution errors cleanly

Confirms sanity checking on user input.

```bash
./bin/rcfg-sim-gen --count 10 --distribution "small:50,medium:40"       ; echo "exit=$?"
./bin/rcfg-sim-gen --count 10 --distribution "tiny:100"                 ; echo "exit=$?"
./bin/rcfg-sim-gen --count 10 --distribution "small:40,medium:40,large:15,huge:5"
```

Expect: first two exit 1 with a descriptive message ("weights must sum to 100" / "unknown bucket"); third runs.

---

## Server samples

All server samples assume you have a generator manifest ready. The simplest source is `/tmp/rcfg-sim-g1/` from [G1](#g1) — it binds 10 devices on 127.0.0.1:12000–12009. Re-run G1 before starting if in doubt.

<a name="s1"></a>
### S1 — start server on 10 ports; verify listening

Launches the server in the background, confirms all 10 ports are bound, then inspects the startup JSON log line.

```bash
mkdir -p /tmp/rcfg-sim-s1/keys
./bin/rcfg-sim \
  --listen-ip 127.0.0.1 --port-start 12000 --port-count 10 \
  --manifest /tmp/rcfg-sim-g1/manifest.csv \
  --host-key /tmp/rcfg-sim-s1/keys/host_rsa \
  --response-delay-ms-min 0 --response-delay-ms-max 0 \
  --max-concurrent-sessions 50 \
  > /tmp/rcfg-sim-s1/server.log 2>&1 &
echo "server pid=$!"
sleep 1
ss -ltn 2>/dev/null | awk '$4 ~ /^127.0.0.1:120[0-9][0-9]$/' | sort  # 10 rows
echo ---
head -1 /tmp/rcfg-sim-s1/server.log  # "rcfg-sim ready" JSON
```

Stop with: `pkill -TERM -f "rcfg-sim --listen-ip 127.0.0.1 --port-start 12000"`

Expect: 10 LISTEN rows, JSON log with `"devices_loaded":10`.

<a name="s2"></a>
### S2 — host-key auto-generation on first run

Point at a non-existent host-key path. Server creates a 2048-bit RSA key at mode 0600 and reuses it on subsequent runs.

```bash
rm -rf /tmp/rcfg-sim-s2 && mkdir -p /tmp/rcfg-sim-s2
./bin/rcfg-sim \
  --listen-ip 127.0.0.1 --port-start 12000 --port-count 10 \
  --manifest /tmp/rcfg-sim-g1/manifest.csv \
  --host-key /tmp/rcfg-sim-s2/newly_generated_key \
  > /tmp/rcfg-sim-s2/server.log 2>&1 &
sleep 1
ls -l /tmp/rcfg-sim-s2/newly_generated_key
head -1 /tmp/rcfg-sim-s2/newly_generated_key
pkill -TERM -f "rcfg-sim --listen-ip 127.0.0.1 --port-start 12000"
```

Expect: file at mode `-rw-------`, first line is `-----BEGIN RSA PRIVATE KEY-----`.

<a name="s3"></a>
### S3 — `show version` with hostname substitution

Verifies the canned show-version response is hostname- and serial-number-substituted. The hostname embedded in the response must match the manifest row for the target port.

```bash
# assumes S1 server is running
expected_hostname=$(awk -F, '$3==12000{print $1}' /tmp/rcfg-sim-g1/manifest.csv)
echo "expecting hostname: $expected_hostname"
/tmp/rcfg-sim-client/client -port 12000 -cmds "terminal length 0|show version|exit" \
  2>/dev/null | grep "uptime is"
```

Expect: a line like `rtr-dfw-edge-1000 uptime is 12 weeks, 3 days, 11 hours, 29 minutes` with the same hostname as `$expected_hostname`.

<a name="s4"></a>
### S4 — `show running-config` streams full mmap'd config

Streams the full rendered config for one device. For a "medium" device expect ~190 KB; for "huge" ~3 MB.

```bash
/tmp/rcfg-sim-client/client -port 12000 -enable-mode \
  -cmds "terminal length 0|show running-config|exit" -read 2s \
  2>/dev/null | wc -c
```

Expect: byte count matching the size of the corresponding `device-NNNNN.cfg` file (± a few hundred bytes for prompts and CRLF framing). Cross-check:

```bash
ls -l $(awk -F, '$3==12000{print $9}' /tmp/rcfg-sim-g1/manifest.csv)
```

<a name="s5"></a>
### S5 — `show startup-config` returns the same bytes as running-config

In v1 startup is an alias for running.

```bash
r=$(/tmp/rcfg-sim-client/client -port 12000 -enable-mode -cmds "terminal length 0|show running-config|exit" -read 2s 2>/dev/null | sha256sum)
s=$(/tmp/rcfg-sim-client/client -port 12000 -enable-mode -cmds "terminal length 0|show startup-config|exit" -read 2s 2>/dev/null | sha256sum)
[ "$r" = "$s" ] && echo "match: running == startup" || echo "MISMATCH"
```

Expect: `match: running == startup`.

<a name="s6"></a>
### S6 — abbreviated commands (`sh ver`, `sh run`, `term len 0`)

Cisco allows unique-prefix abbreviation of every token. Verify that the shortest unambiguous forms resolve to the full commands.

```bash
/tmp/rcfg-sim-client/client -port 12000 -enable-mode \
  -cmds "term len 0|sh ver|sh run|exit" -read 1200ms \
  2>/dev/null | grep -E "uptime is|hostname " | head -3
```

Expect: two lines (uptime from show version, hostname from show running-config).

<a name="s7"></a>
### S7 — enable mode (correct and wrong password)

Correct password transitions the prompt from `>` to `#`; wrong password returns `% Access denied` and leaves the session at `>`.

```bash
# correct
/tmp/rcfg-sim-client/client -port 12000 -enable-mode -enable enable123 \
  -cmds "exit" -read 400ms 2>/dev/null | tail -6

# wrong
/tmp/rcfg-sim-client/client -port 12000 -enable-mode -enable WRONG \
  -cmds "exit" -read 400ms 2>/dev/null | tail -6
```

Expect: correct password produces a `#` prompt; wrong password produces `% Access denied` followed by an unchanged `>` prompt.

<a name="s8"></a>
### S8 — unknown command returns Cisco-style error

```bash
/tmp/rcfg-sim-client/client -port 12000 \
  -cmds "configure terminal|write memory|exit" -read 300ms \
  2>/dev/null | grep -E "Invalid input"
```

Expect: two `% Invalid input detected at '^' marker.` lines.

<a name="s9"></a>
### S9 — ambiguous command returns "Ambiguous command"

`show` alone matches `show version`, `show running-config`, and `show startup-config`, which resolve to different Commands.

```bash
/tmp/rcfg-sim-client/client -port 12000 -cmds "show|exit" -read 300ms \
  2>/dev/null | grep -E "Ambiguous command"
```

Expect: one `% Ambiguous command:  ""` line.

<a name="s10"></a>
### S10 — `exit` from enable returns to unprivileged prompt

```bash
/tmp/rcfg-sim-client/client -port 12000 -enable-mode \
  -cmds "exit|show version|exit" -read 500ms \
  2>/dev/null | tail -10
```

Expect: `#` prompt (in enable), then `>` prompt (after first exit), then show-version output, then session closed after second exit.

<a name="s11"></a>
### S11 — `exit` / `quit` / `logout` / `end` from unprivileged closes session

Four independent sessions, one per alias. Each should close cleanly.

```bash
for word in exit quit logout end; do
  /tmp/rcfg-sim-client/client -port 12000 -cmds "$word" -read 300ms \
    2>/dev/null | tail -2
  echo "--- tested: $word ---"
done
```

Expect: each session closes without error. `end` is an exit alias at the
user-exec prompt; see [S17](#s17) for its enable-mode behaviour.

<a name="s12"></a>
### S12 — wrong SSH password is rejected

```bash
/tmp/rcfg-sim-client/client -port 12000 -pass BAD-PASSWORD -cmds "exit" \
  2>&1 | tail -3
```

Expect: `handshake failed: ssh: unable to authenticate`.

<a name="s13"></a>
### S13 — 20 concurrent sessions against different ports

Drives the per-port listener model. Each of 10 ports gets two concurrent clients; the server must service all 20 without dropping or deadlocking.

```bash
pids=()
for i in 1 2; do
  for p in 12000 12001 12002 12003 12004 12005 12006 12007 12008 12009; do
    /tmp/rcfg-sim-client/client -port $p -cmds "show version|exit" -read 300ms \
      2>/dev/null | grep -m1 "^rtr-\|^sw-" &
    pids+=($!)
  done
done
wait "${pids[@]}"
# Expect 20 hostname-banner lines printed, not necessarily in order.
```

Expect: 20 banner lines; every port shows the same hostname for both clients.

<a name="s14"></a>
### S14 — graceful SIGTERM shutdown unmaps cleanly

```bash
# requires S1 server to be running
pid=$(pgrep -f "rcfg-sim --listen-ip 127.0.0.1 --port-start 12000")
kill -TERM "$pid"
# Wait for exit
for _ in 1 2 3 4 5; do sleep 1; kill -0 "$pid" 2>/dev/null || break; done
kill -0 "$pid" 2>/dev/null && echo "STILL RUNNING" || echo "exited cleanly"
tail -3 /tmp/rcfg-sim-s1/server.log
```

Expect: `exited cleanly`, log includes `"rcfg-sim stopped"` and `"all sessions drained cleanly"`.

<a name="s15"></a>
### S15 — `show inventory` renders canned output with device serial

```bash
/tmp/rcfg-sim-client/client -port 12000 -cmds "terminal pager 0|show inventory|exit" -read 400ms \
  2>/dev/null | sed -n '/^NAME/,/^$/p' | head -20
```

Expect: five NAME/PID entries (chassis, motherboard, PSU, Gi0/0/0, Gi0/0/1). Chassis SN populated from the hostname-derived serial; subcomponents suffixed `-MB` / `-PS`. Interface slots have empty SN — matches real Cisco.

<a name="s16"></a>
### S16 — `show version` and `show inventory` agree on chassis serial

The invariant: whatever serial appears as `Processor board ID` in `show version` must be byte-identical to the chassis `SN:` in `show inventory` for the same device. A mismatch would trigger false "inventory drift" alerts in rConfig-style collectors.

```bash
out=$(/tmp/rcfg-sim-client/client -port 12000 -cmds "terminal pager 0|show version|show inventory|exit" -read 600ms 2>/dev/null)
v=$(echo "$out" | grep -oE "Processor board ID [A-Z0-9]+" | awk '{print $4}')
i=$(echo "$out" | grep -oE "VID: V07 , SN: [A-Z0-9]+" | head -1 | awk '{print $NF}')
printf "show version   Processor board ID : %s\n" "$v"
printf "show inventory chassis SN          : %s\n" "$i"
[ "$v" = "$i" ] && echo "SERIAL AGREEMENT OK" || echo "MISMATCH"
```

Expect: `SERIAL AGREEMENT OK` with both serials matching (e.g. `FOC5FA84785`).

<a name="s17"></a>
### S17 — `end` in enable mode drops to user-exec (does not close session)

`end` is an alias for `exit`: from `#` it returns to `>`; from `>` it closes the session. This test takes one session into enable, uses `end` to drop back, then runs `show version` at `>` to prove the session is still alive.

```bash
/tmp/rcfg-sim-client/client -port 12000 -enable-mode \
  -cmds "end|show version|exit" -read 500ms \
  2>/dev/null | grep -E "^rtr-|uptime is" | head -4
```

Expect: four lines — `#` prompt after enable, `>` prompt after `end`, `uptime is …` line from `show version`, `>` prompt after final `exit`.

<a name="s18"></a>
### S18 — `terminal pager 0` is a silent ack

```bash
/tmp/rcfg-sim-client/client -port 12000 -cmds "terminal pager 0|exit" -read 300ms \
  2>/dev/null | grep -vE "^\s*$"
```

Expect: only banner, prompt, echoed `terminal pager 0`, next prompt, echoed `exit`. No `% Invalid input`, no output between the two prompts after `terminal pager 0`.

---

## Metrics samples

All metrics samples assume [S1](#s1) is running (server bound to 127.0.0.1:12000–12009) with `--metrics-addr 127.0.0.1:9100`. To include that flag, re-launch with:

```bash
pkill -TERM -f "rcfg-sim --listen-ip 127.0.0.1 --port-start 12000" 2>/dev/null
mkdir -p /tmp/rcfg-sim-s1/keys
./bin/rcfg-sim \
  --listen-ip 127.0.0.1 --port-start 12000 --port-count 10 \
  --manifest /tmp/rcfg-sim-g1/manifest.csv \
  --host-key /tmp/rcfg-sim-s1/keys/host_rsa \
  --response-delay-ms-min 0 --response-delay-ms-max 0 \
  --metrics-addr 127.0.0.1:9100 \
  > /tmp/rcfg-sim-s1/server.log 2>&1 &
sleep 1
```

<a name="m1"></a>
### M1 — `/healthz` returns 200 OK

```bash
curl -sv http://127.0.0.1:9100/healthz 2>&1 | grep -E "^< HTTP|^ok"
```

Expect: `< HTTP/1.1 200 OK` and a single `ok` line in the body.

<a name="m2"></a>
### M2 — pre-traffic scrape shows all 8 families + zero-count label sets

Every bounded label combination is pre-registered at zero so Prometheus can alert on absence instead of waiting for traffic.

```bash
curl -s http://127.0.0.1:9100/metrics | grep -E "^# HELP rcfgsim_"
echo "---"
curl -s http://127.0.0.1:9100/metrics | grep -E "^rcfgsim_(faults_injected_total|sessions_total|auth_attempts_total) " | head -12
```

Expect: 8 `# HELP rcfgsim_*` lines (active_sessions, auth_attempts_total, bytes_sent_total, command_duration_seconds, faults_injected_total, handshake_duration_seconds, session_duration_seconds, sessions_total). 4 fault-type entries at 0, 4 session-result entries (ok=0, auth_fail=0, disconnect=0, error=0), 2 auth-result entries (ok=0, fail=0).

<a name="m3"></a>
### M3 — `rcfgsim_sessions_total` moves by result

Drive three sessions — two OK, one with bad password — then inspect counts.

```bash
/tmp/rcfg-sim-client/client -port 12000 -cmds "show version|exit" -read 500ms 2>/dev/null > /dev/null
/tmp/rcfg-sim-client/client -port 12001 -cmds "show version|exit" -read 500ms 2>/dev/null > /dev/null
/tmp/rcfg-sim-client/client -port 12002 -pass BAD -cmds "exit" 2>/dev/null > /dev/null
sleep 0.5
curl -s http://127.0.0.1:9100/metrics | grep "^rcfgsim_sessions_total{"
```

Expect: `result="ok"` ≥ 2, `result="auth_fail"` ≥ 1, `result="disconnect"` and `result="error"` at 0 (or 0-to-small if you've had prior failed sessions).

<a name="m4"></a>
### M4 — `rcfgsim_auth_attempts_total{ok|fail}` tracks both outcomes

The server increments the counter inside the SSH password callback, so both handshake-succeeding and handshake-failing paths are observed.

```bash
curl -s http://127.0.0.1:9100/metrics | grep "^rcfgsim_auth_attempts_total{"
```

Expect: both `ok` and `fail` labels present with non-zero counts (given the [M3](#m3) drive above).

<a name="m5"></a>
### M5 — `rcfgsim_command_duration_seconds` only uses canonical `Cmd*` labels

Bounded cardinality means user input like `sh ver` never leaks into a label — the server resolves to the `Command` enum first, then stringifies to `CmdShowVersion` for the label.

```bash
/tmp/rcfg-sim-client/client -port 12000 -cmds "sh ver|sh run|sh inv|configure terminal|show|exit" -read 500ms 2>/dev/null > /dev/null
sleep 0.3
curl -s http://127.0.0.1:9100/metrics | grep "^rcfgsim_command_duration_seconds_count{"
```

Expect: every label value starts with `Cmd`. No `command="show"` or `command="sh ver"` anywhere. CmdAmbiguous (from bare `show`), CmdUnknown (from `configure terminal`), and CmdShowVersion / CmdShowRunningConfig / CmdShowInventory / CmdExit all incremented.

<a name="m6"></a>
### M6 — `rcfgsim_bytes_sent_total` grows with config streaming

```bash
before=$(curl -s http://127.0.0.1:9100/metrics | awk '/^rcfgsim_bytes_sent_total /{print $2}')
/tmp/rcfg-sim-client/client -port 12000 -enable-mode -cmds "terminal length 0|show running-config|exit" -read 2s 2>/dev/null > /dev/null
sleep 0.3
after=$(curl -s http://127.0.0.1:9100/metrics | awk '/^rcfgsim_bytes_sent_total /{print $2}')
echo "before=$before after=$after delta=$((${after%.*} - ${before%.*}))"
```

Expect: delta ≥ 150000 (roughly the size of a medium config) — show running-config streams the whole mmap'd file.

<a name="m7"></a>
### M7 — `rcfgsim_faults_injected_total` is pre-registered at zero

Fault counters are pre-registered at zero so Prometheus dashboards and alerting rules can be built and tested before any fault activates in production. Verifying the series exist empty means absence of data on a fault type is a real signal, not a missing-metric artifact.

```bash
curl -s http://127.0.0.1:9100/metrics | grep "^rcfgsim_faults_injected_total{"
```

Expect: four rows, all `0`: `auth_fail`, `disconnect_mid`, `slow_response`, `malformed`.

<a name="m8"></a>
### M8 — label cardinality stays bounded under load

Drive a burst of traffic and confirm the scraped series count doesn't grow beyond the closed label set.

```bash
for p in 12000 12001 12002 12003 12004 12005 12006 12007 12008 12009; do
  for cmd in "show version" "show inventory" "sh run" "configure terminal" "en"; do
    /tmp/rcfg-sim-client/client -port $p -cmds "$cmd|exit" -read 150ms 2>/dev/null > /dev/null &
  done
done
wait
sleep 0.5
curl -s http://127.0.0.1:9100/metrics | \
  grep -E "^rcfgsim_[a-z_]+(\{|_count|_sum| )" | \
  grep -vE "_bucket\{" | \
  awk '{print $1}' | sort -u | wc -l
```

Expect: a small fixed number (typically 25 unique series for rcfgsim_\* families, ~70 including all histogram `_bucket`/`_sum`/`_count` lines). Crucially, the number should not grow with traffic.

---

## Fault injection samples

All samples assume the G1 manifest exists at `/tmp/rcfg-sim-g1/manifest.csv` and the test client has been built per [the one-time step](#build-the-ssh-test-client). Each sample stops the previous server (if any) and relaunches with its own flags.

Kill the server between samples with:
```bash
pkill -TERM -f "rcfg-sim --listen-ip 127.0.0.1 --port-start 12000"; sleep 1
```

<a name="f1"></a>
### F1 — `auth_fail` at rate 1.0 rejects every login

```bash
pkill -TERM -f "rcfg-sim --listen-ip 127.0.0.1 --port-start 12000" 2>/dev/null; sleep 1
mkdir -p /tmp/rcfg-sim-f/keys
./bin/rcfg-sim \
  --listen-ip 127.0.0.1 --port-start 12000 --port-count 10 \
  --manifest /tmp/rcfg-sim-g1/manifest.csv \
  --host-key /tmp/rcfg-sim-f/keys/host \
  --fault-rate 1.0 --fault-types "auth_fail" \
  --metrics-addr 127.0.0.1:9100 \
  > /tmp/rcfg-sim-f/server.log 2>&1 &
sleep 1

for i in 1 2 3 4 5; do
  /tmp/rcfg-sim-client/client -port 12000 -pass admin -cmds "exit" 2>&1 | tail -1
done

curl -s http://127.0.0.1:9100/metrics | \
  grep -E "rcfgsim_(faults_injected_total\{type=\"auth_fail\"\}|sessions_total\{result=\"auth_fail\"\}|auth_attempts_total\{result=\"(ok|fail)\"\})"
```

Expect: 5 handshake failures on correct password, `faults_injected{auth_fail}` ≥ 5, `auth_attempts{fail}` ≥ 5, `auth_attempts{ok}` = 0, `sessions{auth_fail}` ≥ 5.

<a name="f2"></a>
### F2 — `auth_fail` at rate 0.5 rejects ~50%

```bash
pkill -TERM -f "rcfg-sim --listen-ip 127.0.0.1 --port-start 12000" 2>/dev/null; sleep 1
./bin/rcfg-sim \
  --listen-ip 127.0.0.1 --port-start 12000 --port-count 10 \
  --manifest /tmp/rcfg-sim-g1/manifest.csv \
  --host-key /tmp/rcfg-sim-f/keys/host \
  --fault-rate 0.5 --fault-types "auth_fail" \
  --metrics-addr 127.0.0.1:9100 \
  > /tmp/rcfg-sim-f/server.log 2>&1 &
sleep 1

fails=0; oks=0
for i in $(seq 1 20); do
  if /tmp/rcfg-sim-client/client -port 12000 -cmds "exit" 2>&1 | grep -q "handshake failed"; then
    fails=$((fails+1))
  else
    oks=$((oks+1))
  fi
done
echo "fails=$fails / 20 (target ~10 ± noise)"
echo "---"
curl -s http://127.0.0.1:9100/metrics | grep "rcfgsim_faults_injected_total{type=\"auth_fail\""
```

Expect: fails between 6 and 14 (3σ window around rate=0.5). `faults_injected{auth_fail}` matches the observed fail count.

<a name="f3"></a>
### F3 — `disconnect_mid` truncates `show running-config` + RSTs the TCP conn

```bash
pkill -TERM -f "rcfg-sim --listen-ip 127.0.0.1 --port-start 12000" 2>/dev/null; sleep 1
./bin/rcfg-sim \
  --listen-ip 127.0.0.1 --port-start 12000 --port-count 10 \
  --manifest /tmp/rcfg-sim-g1/manifest.csv \
  --host-key /tmp/rcfg-sim-f/keys/host \
  --fault-rate 1.0 --fault-types "disconnect_mid" \
  --metrics-addr 127.0.0.1:9100 \
  > /tmp/rcfg-sim-f/server.log 2>&1 &
sleep 1

bytes=$(/tmp/rcfg-sim-client/client -port 12000 -cmds "terminal length 0|show running-config|exit" -read 2s 2>/dev/null | wc -c)
size_on_disk=$(wc -c < $(awk -F, '$3==12000{print $9}' /tmp/rcfg-sim-g1/manifest.csv))
echo "received: $bytes bytes;  on-disk: $size_on_disk bytes"
echo "ratio:    $(awk -v r=$bytes -v d=$size_on_disk 'BEGIN{printf "%.2f\n", r/d}')"
echo "---"
curl -s http://127.0.0.1:9100/metrics | grep -E "rcfgsim_(faults_injected_total\{type=\"disconnect_mid\"\}|sessions_total\{result=\"disconnect\"\})"
```

Expect: `received < on-disk` (received is 20-40% of on-disk for that bucket, plus prompts/echo). `faults_injected{disconnect_mid}` ≥ 1, `sessions{disconnect}` ≥ 1.

<a name="f4"></a>
### F4 — `slow_response` stretches command latency 10-50×

```bash
pkill -TERM -f "rcfg-sim --listen-ip 127.0.0.1 --port-start 12000" 2>/dev/null; sleep 1
./bin/rcfg-sim \
  --listen-ip 127.0.0.1 --port-start 12000 --port-count 10 \
  --manifest /tmp/rcfg-sim-g1/manifest.csv \
  --host-key /tmp/rcfg-sim-f/keys/host \
  --response-delay-ms-min 0 --response-delay-ms-max 50 \
  --fault-rate 1.0 --fault-types "slow_response" \
  --metrics-addr 127.0.0.1:9100 \
  > /tmp/rcfg-sim-f/server.log 2>&1 &
sleep 1

t0=$(date +%s%N)
/tmp/rcfg-sim-client/client -port 12000 -cmds "show version|exit" -read 5s 2>/dev/null > /dev/null
t1=$(date +%s%N)
echo "elapsed: $(( (t1 - t0) / 1000000 )) ms (expect ≥ 500 ms: base-50ms × multiplier-10-50×)"
echo "---"
curl -s http://127.0.0.1:9100/metrics | grep "rcfgsim_faults_injected_total{type=\"slow_response\""
```

Expect: elapsed ≥ 500 ms even though max delay is 50 ms. `faults_injected{slow_response}` ≥ 1 (one per command; show version + exit ≈ 2).

<a name="f5"></a>
### F5 — `malformed` corrupts config output (truncate / inject / bit-flip)

Three modes chosen uniformly at random per fault. Run several times to hit all three.

```bash
pkill -TERM -f "rcfg-sim --listen-ip 127.0.0.1 --port-start 12000" 2>/dev/null; sleep 1
./bin/rcfg-sim \
  --listen-ip 127.0.0.1 --port-start 12000 --port-count 10 \
  --manifest /tmp/rcfg-sim-g1/manifest.csv \
  --host-key /tmp/rcfg-sim-f/keys/host \
  --fault-rate 1.0 --fault-types "malformed" \
  --metrics-addr 127.0.0.1:9100 \
  > /tmp/rcfg-sim-f/server.log 2>&1 &
sleep 1

for run in 1 2 3 4 5; do
  out=$(/tmp/rcfg-sim-client/client -port 12000 -cmds "terminal length 0|show running-config|exit" -read 2s 2>/dev/null)
  size=$(printf '%s' "$out" | wc -c)
  has_marker=$(printf '%s' "$out" | grep -c MALFORMED-FAULT-INJECTION-MARKER || true)
  echo "run $run: bytes=$size  inject-marker=$has_marker"
done
echo "---"
curl -s http://127.0.0.1:9100/metrics | grep "rcfgsim_faults_injected_total{type=\"malformed\""
```

Expect: across 5 runs, a mix of bytes values (short for truncate; marker=1 for inject; same size as baseline but no marker for bit-flip). `faults_injected{malformed}` = 5.

<a name="f6"></a>
### F6 — zero rate with all types configured: no faults fire

```bash
pkill -TERM -f "rcfg-sim --listen-ip 127.0.0.1 --port-start 12000" 2>/dev/null; sleep 1
./bin/rcfg-sim \
  --listen-ip 127.0.0.1 --port-start 12000 --port-count 10 \
  --manifest /tmp/rcfg-sim-g1/manifest.csv \
  --host-key /tmp/rcfg-sim-f/keys/host \
  --fault-rate 0.0 --fault-types "auth_fail,disconnect_mid,slow_response,malformed" \
  --metrics-addr 127.0.0.1:9100 \
  > /tmp/rcfg-sim-f/server.log 2>&1 &
sleep 1

for i in $(seq 1 20); do
  /tmp/rcfg-sim-client/client -port 12000 -cmds "show version|exit" -read 500ms 2>/dev/null > /dev/null
done
curl -s http://127.0.0.1:9100/metrics | grep "^rcfgsim_faults_injected_total{"
echo "---"
curl -s http://127.0.0.1:9100/metrics | grep "^rcfgsim_sessions_total{"
```

Expect: all four `faults_injected_total{type=…}` rows at 0. `sessions_total{ok}` = 20, all other session results at 0. Proves the rate=0 short-circuit.

<a name="f7"></a>
### F7 — hot path with fault-flags present but rate=0 matches no-flag baseline

Benchmark the dispatch inner loop under three configurations. NoConfig and ZeroRate must be within ~2% (spec target); HighRate quantifies the overhead of actually rolling.

```bash
go test -run XXX -bench BenchmarkDispatchHotPath -benchtime 2s -count 5 ./internal/sshsrv/
```

Expect output similar to (ns/op numbers vary by host):

```
BenchmarkDispatchHotPath/NoConfig-2     ~637 ns/op    1636 B/op    3 allocs/op
BenchmarkDispatchHotPath/ZeroRate-2     ~639 ns/op    1636 B/op    3 allocs/op
BenchmarkDispatchHotPath/HighRate-2     ~650 ns/op    1636 B/op    3 allocs/op
```

Delta between NoConfig and ZeroRate should be well under 2%.

---

## End-to-end

<a name="e1"></a>
### E1 — generate + serve + SSH in, verify hostname matches manifest

Full round trip. The hostname in `show version` output must match the manifest row for the target IP:port.

```bash
# Clean up any stale run
pkill -TERM -f "rcfg-sim --listen-ip 127.0.0.1 --port-start 19000" 2>/dev/null
rm -rf /tmp/rcfg-sim-e1 && mkdir -p /tmp/rcfg-sim-e1/keys

# Generate a 5-device set on an obscure port range
./bin/rcfg-sim-gen --count 5 --seed 123 \
  --ip-base 127.0.0.1 --ip-count 1 --devices-per-ip 5 --port-start 19000 \
  --output-dir /tmp/rcfg-sim-e1/configs \
  --manifest /tmp/rcfg-sim-e1/manifest.csv > /dev/null

# Start server
./bin/rcfg-sim \
  --listen-ip 127.0.0.1 --port-start 19000 --port-count 5 \
  --manifest /tmp/rcfg-sim-e1/manifest.csv \
  --host-key /tmp/rcfg-sim-e1/keys/host \
  --response-delay-ms-min 0 --response-delay-ms-max 0 \
  > /tmp/rcfg-sim-e1/server.log 2>&1 &
sleep 1

# For each port, compare server-reported hostname to manifest-recorded hostname
for port in 19000 19001 19002 19003 19004; do
  expected=$(awk -F, -v p=$port '$3==p{print $1}' /tmp/rcfg-sim-e1/manifest.csv)
  observed=$(/tmp/rcfg-sim-client/client -port $port -cmds "exit" -read 300ms \
    2>/dev/null | grep -oE '^(rtr|sw)-[a-z0-9-]+' | head -1)
  printf "port %s  manifest=%-25s observed=%-25s  %s\n" \
    "$port" "$expected" "$observed" \
    "$([ "$expected" = "$observed" ] && echo OK || echo FAIL)"
done

pkill -TERM -f "rcfg-sim --listen-ip 127.0.0.1 --port-start 19000"
```

Expect: five `OK` lines.
