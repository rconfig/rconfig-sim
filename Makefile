BIN_DIR       := bin
GOFLAGS       := -trimpath
LDFLAGS       := -s -w
BUILD_ENV     := CGO_ENABLED=0

# Install targets (override on command line for unit tests or staging).
PREFIX        ?= /opt/rcfg-sim
SYSTEMD_DIR   ?= /etc/systemd/system
SYSCTL_DIR    ?= /etc/sysctl.d
LIMITS_DIR    ?= /etc/security/limits.d
ETC_DIR       ?= /etc/rcfg-sim

# Defaults for generate-configs; override with `make generate-configs COUNT=100`.
COUNT         ?= 50000
OUTPUT_DIR    ?= $(PREFIX)/configs
MANIFEST      ?= $(PREFIX)/manifest.csv
SEED          ?= 42

# Defaults for deploy-aliases / remove-aliases.
INTERFACE     ?=
BASE_IP       ?= 10.50.0.1
IP_COUNT      ?= 20

.PHONY: all build test vet fmt install uninstall clean \
        generate-configs deploy-aliases remove-aliases integration bench

all: build

build:
	$(BUILD_ENV) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/rcfg-sim ./cmd/rcfg-sim
	$(BUILD_ENV) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/rcfg-sim-gen ./cmd/rcfg-sim-gen

test:
	go test ./...

integration:
	go test -tags integration -count 1 ./...

bench:
	go test -run XXX -bench . -benchtime 2s ./internal/...

vet:
	go vet ./...
	go vet -tags integration ./...

fmt:
	gofmt -s -w .

# install copies binaries, the systemd template unit, the sysctl drop-in and
# the limits.d file into their canonical locations. Does NOT create the
# rcfgsim user, generate any configs, enable units, or apply sysctl —
# operators drive those explicitly so there are no surprises.
install: build
	@[ "$$(id -u)" -eq 0 ] || { echo "install requires root (try: sudo make install)"; exit 1; }
	install -d $(PREFIX)/bin
	@# Skip copies when src and dst resolve to the same file (happens when
	@# the repo is cloned into $(PREFIX) directly; binaries are already in place).
	@if [ "$$(realpath -m $(BIN_DIR)/rcfg-sim)" != "$$(realpath -m $(PREFIX)/bin/rcfg-sim)" ]; then \
		install -m 0755 $(BIN_DIR)/rcfg-sim $(PREFIX)/bin/rcfg-sim; \
	fi
	@if [ "$$(realpath -m $(BIN_DIR)/rcfg-sim-gen)" != "$$(realpath -m $(PREFIX)/bin/rcfg-sim-gen)" ]; then \
		install -m 0755 $(BIN_DIR)/rcfg-sim-gen $(PREFIX)/bin/rcfg-sim-gen; \
	fi
	install -d $(PREFIX)/deploy/systemd
	@if [ "$$(realpath -m deploy/systemd/rcfg-sim-instance.env.sample)" != "$$(realpath -m $(PREFIX)/deploy/systemd/rcfg-sim-instance.env.sample)" ]; then \
		install -m 0644 deploy/systemd/rcfg-sim-instance.env.sample $(PREFIX)/deploy/systemd/rcfg-sim-instance.env.sample; \
	fi
	@if [ "$$(realpath -m deploy/ip-aliases.sh)" != "$$(realpath -m $(PREFIX)/deploy/ip-aliases.sh)" ]; then \
		install -m 0755 deploy/ip-aliases.sh $(PREFIX)/deploy/ip-aliases.sh; \
	fi
	install -m 0644 deploy/systemd/rcfg-sim@.service $(SYSTEMD_DIR)/rcfg-sim@.service
	install -m 0644 deploy/sysctl-rcfg-sim.conf      $(SYSCTL_DIR)/99-rcfg-sim.conf
	install -m 0644 deploy/limits-rcfg-sim.conf      $(LIMITS_DIR)/rcfg-sim.conf
	install -d -m 0755 $(ETC_DIR)
	@# Host-keys directory must be writable by the rcfgsim service user so
	@# first-boot host-key generation succeeds under ProtectSystem=full. 700
	@# because the keys are RSA private material — only the owner should read.
	@# Idempotent: install -d no-ops if the directory already exists with the
	@# right mode. chown is run every time so it self-heals after
	@# accidental ownership changes.
	install -d -m 0700 -o rcfgsim -g rcfgsim $(PREFIX)/host-keys 2>/dev/null || install -d -m 0700 $(PREFIX)/host-keys
	@if id -u rcfgsim >/dev/null 2>&1; then chown rcfgsim:rcfgsim $(PREFIX)/host-keys; fi
	systemctl daemon-reload
	@echo
	@echo "install complete. Next:"
	@echo "  1. sudo useradd -r -s /sbin/nologin rcfgsim      (if not already present)"
	@echo "  2. sudo chown rcfgsim:rcfgsim $(ETC_DIR)"
	@echo "  3. sudo sysctl --system                          # apply sysctl drop-in"
	@echo "  4. cp $(PREFIX)/deploy/systemd/rcfg-sim-instance.env.sample \\"
	@echo "        $(ETC_DIR)/<LISTEN_IP>.env                 # per instance"
	@echo "  5. sudo systemctl enable --now rcfg-sim@<LISTEN_IP>"

# uninstall stops + disables every active rcfg-sim@ instance, then removes
# the binaries, unit, sysctl, and limits files. Leaves /opt/rcfg-sim/configs,
# $(ETC_DIR)/*.env, and the rcfgsim user in place — those hold operator
# state and should survive a reinstall.
uninstall:
	@[ "$$(id -u)" -eq 0 ] || { echo "uninstall requires root"; exit 1; }
	-systemctl list-units --state=active 'rcfg-sim@*' --no-legend --no-pager 2>/dev/null | awk '{print $$1}' | xargs -r systemctl stop
	-systemctl list-unit-files 'rcfg-sim@*' --no-legend --no-pager 2>/dev/null | awk '{print $$1}' | xargs -r systemctl disable
	rm -f $(SYSTEMD_DIR)/rcfg-sim@.service
	rm -f $(SYSCTL_DIR)/99-rcfg-sim.conf
	rm -f $(LIMITS_DIR)/rcfg-sim.conf
	rm -f $(PREFIX)/bin/rcfg-sim
	rm -f $(PREFIX)/bin/rcfg-sim-gen
	systemctl daemon-reload
	@echo "uninstall complete. $(PREFIX)/configs, $(ETC_DIR), and the rcfgsim user were left in place."

# generate-configs renders device configs into $(OUTPUT_DIR) and a manifest
# at $(MANIFEST). Override COUNT/SEED/OUTPUT_DIR/MANIFEST for different runs.
generate-configs: build
	$(BIN_DIR)/rcfg-sim-gen --count $(COUNT) --seed $(SEED) --output-dir $(OUTPUT_DIR) --manifest $(MANIFEST)

# deploy-aliases / remove-aliases wrap deploy/ip-aliases.sh.
# Required overrides:
#   INTERFACE=<iface>  (e.g. ens192)
# Optional overrides:
#   BASE_IP=10.50.0.1
#   IP_COUNT=20
deploy-aliases:
	@[ -n "$(INTERFACE)" ] || { echo "INTERFACE=<iface> required (see 'ip link')"; exit 1; }
	bash deploy/ip-aliases.sh --interface "$(INTERFACE)" --base-ip "$(BASE_IP)" --count $(IP_COUNT)

remove-aliases:
	@[ -n "$(INTERFACE)" ] || { echo "INTERFACE=<iface> required"; exit 1; }
	bash deploy/ip-aliases.sh --interface "$(INTERFACE)" --base-ip "$(BASE_IP)" --count $(IP_COUNT) --remove

clean:
	rm -rf $(BIN_DIR)
