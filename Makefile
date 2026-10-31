BIN_DIR       := bin
GOFLAGS       := -trimpath
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS       := -s -w -X main.version=$(VERSION)
BUILD_ENV     := CGO_ENABLED=0

# Install targets (override on command line for unit tests or staging).
PREFIX        ?= /opt/rcfg-sim
SYSTEMD_DIR   ?= /etc/systemd/system
SYSCTL_DIR    ?= /etc/sysctl.d
LIMITS_DIR    ?= /etc/security/limits.d
ETC_DIR       ?= /etc/rcfg-sim
UI_ETC_DIR    ?= /etc/rcfg-sim-ui
POLKIT_DIR    ?= /etc/polkit-1/rules.d
# Group (and, for the UI, user) that may write $(ETC_DIR). The device
# processes run as rcfgsim and get no access to it (docs/UI-ARCHITECTURE.md 7.3).
UI_GROUP      ?= rcfgsim-ui

# Web UI toolchain (docs/UI-ARCHITECTURE.md 11.2). templ: the newest release
# whose go line is no higher than go.mod's. Tailwind: the standalone CLI,
# checksum-pinned per platform, downloaded to $(TOOLS_DIR).
TEMPL_VERSION    := v0.3.1100
TAILWIND_VERSION := v4.3.3
TAILWIND_SHA256_linux-x64   := dc61b3ac6b8c9ca874c0cc4c57b2409791a64c5540404ca5f5367360babc313a
TAILWIND_SHA256_linux-arm64 := 55fd0b241214eff3de1e8ee4f22796662f2d2e7a49bcfca7477cfd0bac398195
TAILWIND_SHA256_macos-x64   := 7922e0953f2110c05976e3bf58f14e643d90427575e766b7d433f5f80cbee7e1
TAILWIND_SHA256_macos-arm64 := cdf646702987a743464dff4d9c60fd4480d1c1e73dd819a9a67f1078815dce9d
TOOLS_DIR        := .tools
UI_DIR           := internal/ui/web
TW_OS   := $(subst Darwin,macos,$(subst Linux,linux,$(shell uname -s)))
TW_ARCH := $(subst aarch64,arm64,$(subst x86_64,x64,$(shell uname -m)))
TW_PLATFORM := $(TW_OS)-$(TW_ARCH)
TAILWIND := $(TOOLS_DIR)/tailwindcss-$(TAILWIND_VERSION)-$(TW_PLATFORM)
TEMPL    := go run github.com/a-h/templ/cmd/templ@$(TEMPL_VERSION)
SHA256   := $(shell command -v sha256sum >/dev/null 2>&1 && echo sha256sum || echo "shasum -a 256")

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
        generate-configs deploy-aliases remove-aliases integration bench \
        ui-gen ui-check install-ui uninstall-ui etc-perms

all: build

build:
	$(BUILD_ENV) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/rcfg-sim ./cmd/rcfg-sim
	$(BUILD_ENV) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/rcfg-sim-gen ./cmd/rcfg-sim-gen
	$(BUILD_ENV) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/rcfg-sim-ui ./cmd/rcfg-sim-ui

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
	$(MAKE) --no-print-directory etc-perms
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
	@echo "  2. sudo sysctl --system                          # apply sysctl drop-in"
	@echo "  3. sudo install -m 0660 -g $(UI_GROUP) $(PREFIX)/deploy/systemd/rcfg-sim-instance.env.sample \\"
	@echo "        $(ETC_DIR)/<LISTEN_IP>.env                 # per instance"
	@echo "  4. sudo systemctl enable --now rcfg-sim@<LISTEN_IP>"
	@echo "  Optional web UI: sudo make install-ui"

# etc-perms sets the $(ETC_DIR) layout from docs/UI-ARCHITECTURE.md 7.3:
# root:$(UI_GROUP) 2770 (setgid, so new files get the group) and env files
# 0660 in that group. systemd reads EnvironmentFile= as root, so the device
# processes (rcfgsim) need, and get, no access. Creates the group if missing;
# installs without the UI just leave it unused. Idempotent.
etc-perms:
	@[ "$$(id -u)" -eq 0 ] || { echo "etc-perms requires root"; exit 1; }
	getent group $(UI_GROUP) >/dev/null || groupadd -r $(UI_GROUP)
	install -d -m 2770 -o root -g $(UI_GROUP) $(ETC_DIR)
	chown root:$(UI_GROUP) $(ETC_DIR)
	chmod 2770 $(ETC_DIR)
	find $(ETC_DIR) -maxdepth 1 -type f -name '*.env' -exec chgrp $(UI_GROUP) {} + -exec chmod 0660 {} +

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

# install-ui installs the web UI controller: the rcfgsim-ui user, binary,
# unit, polkit rule and a sample env file. Not part of `make install`, and
# never enables or starts the unit.
install-ui: build
	@[ "$$(id -u)" -eq 0 ] || { echo "install-ui requires root (try: sudo make install-ui)"; exit 1; }
	getent group $(UI_GROUP) >/dev/null || groupadd -r $(UI_GROUP)
	id -u rcfgsim-ui >/dev/null 2>&1 || useradd -r -g $(UI_GROUP) -s /sbin/nologin -d /var/lib/rcfg-sim-ui rcfgsim-ui
	install -d $(PREFIX)/bin
	install -m 0755 $(BIN_DIR)/rcfg-sim-ui $(PREFIX)/bin/rcfg-sim-ui
	install -m 0644 deploy/systemd/rcfg-sim-ui.service $(SYSTEMD_DIR)/rcfg-sim-ui.service
	install -d -m 0755 $(POLKIT_DIR)
	install -m 0644 deploy/polkit/50-rcfg-sim-ui.rules $(POLKIT_DIR)/50-rcfg-sim-ui.rules
	$(MAKE) --no-print-directory etc-perms
	install -d -m 0750 -o root -g $(UI_GROUP) $(UI_ETC_DIR)
	@[ -e $(UI_ETC_DIR)/rcfg-sim-ui.env ] || install -m 0640 -o root -g $(UI_GROUP) deploy/systemd/rcfg-sim-ui.env.sample $(UI_ETC_DIR)/rcfg-sim-ui.env
	systemctl daemon-reload
	@echo
	@echo "install-ui complete. Next:"
	@echo "  1. Set ADMIN=true (and ADMIN_TOKEN, if used) in each $(ETC_DIR)/<IP>.env, then restart those instances"
	@echo "  2. Edit $(UI_ETC_DIR)/rcfg-sim-ui.env (UI_TOKEN, ADMIN_TOKEN, UI_ADDR)"
	@echo "  3. sudo systemctl enable --now rcfg-sim-ui"
	@echo "  4. ssh -L 8080:127.0.0.1:8080 <this host>, then open http://127.0.0.1:8080/"

uninstall-ui:
	@[ "$$(id -u)" -eq 0 ] || { echo "uninstall-ui requires root"; exit 1; }
	-systemctl disable --now rcfg-sim-ui.service
	rm -f $(SYSTEMD_DIR)/rcfg-sim-ui.service $(POLKIT_DIR)/50-rcfg-sim-ui.rules $(PREFIX)/bin/rcfg-sim-ui
	systemctl daemon-reload
	@echo "uninstall-ui complete. $(UI_ETC_DIR), /var/lib/rcfg-sim-ui and the rcfgsim-ui user were left in place."

# --- Web UI generated files (committed; docs/UI-ARCHITECTURE.md D8) ---

$(TAILWIND):
	@[ -n "$(TAILWIND_SHA256_$(TW_PLATFORM))" ] || { echo "no pinned Tailwind checksum for $(TW_PLATFORM)"; exit 1; }
	mkdir -p $(TOOLS_DIR)
	curl -fsSL -o $@.download https://github.com/tailwindlabs/tailwindcss/releases/download/$(TAILWIND_VERSION)/tailwindcss-$(TW_PLATFORM)
	echo "$(TAILWIND_SHA256_$(TW_PLATFORM))  $@.download" | $(SHA256) -c -
	chmod +x $@.download
	mv $@.download $@

# ui-gen regenerates the templ Go code and the built CSS. Commit both.
ui-gen: $(TAILWIND)
	$(TEMPL) generate -path $(UI_DIR)/views
	$(TAILWIND) -i $(UI_DIR)/static/src/app.css -o $(UI_DIR)/static/app.css --minify

# ui-check regenerates into a temp copy and fails if the committed output
# differs, so CI catches a stale *_templ.go or app.css.
ui-check: $(TAILWIND)
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	cp -R $(UI_DIR) "$$tmp/web"; \
	rm -f "$$tmp"/web/views/*_templ.go "$$tmp/web/static/app.css"; \
	$(TEMPL) generate -path "$$tmp/web/views" >/dev/null && \
	$(TAILWIND) -i "$$tmp/web/static/src/app.css" -o "$$tmp/web/static/app.css" --minify 2>/dev/null && \
	if diff -r $(UI_DIR) "$$tmp/web"; then echo "ui-check: generated files are up to date"; \
	else echo "ui-check: generated files are stale; run 'make ui-gen' and commit the result"; exit 1; fi

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
	rm -rf $(TOOLS_DIR)
