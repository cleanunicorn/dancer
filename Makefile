# dancer — agent orchestration over Slack
#
#   make help          list targets
#
# Variables you may override:  make service-install BIN=/opt/dancer/dancer USER_=bot

BIN     ?= /usr/local/bin/dancer
CONFIG  ?= $(or $(DANCER_CONFIG),$(HOME)/.config/dancer/config.toml)
USER_   ?= $(shell id -un)
GROUP_  ?= $(shell id -gn)
HOME_   ?= $(shell getent passwd $(USER_) | cut -d: -f6)
UNIT    ?= /etc/systemd/system/dancer.service
GO      ?= go
# BIN and the unit live under root-owned prefixes. Set SUDO= (empty) to install
# into a prefix you already own, e.g. `make install BIN=$$HOME/bin/dancer SUDO=`.
SUDO    ?= sudo

# auto-update: a root-owned deploy clone, polled by a systemd timer
REPO       ?= $(shell git remote get-url origin)
BRANCH     ?= main
SRC        ?= /opt/dancer/src
INTERVAL   ?= 5min
UPDATER    ?= /usr/local/lib/dancer/dancer-update.sh
UPDATE_UNIT ?= /etc/systemd/system/dancer-update.service
UPDATE_TIMER ?= /etc/systemd/system/dancer-update.timer
GO_BIN     ?= $(shell command -v $(GO))
DEPLOY_ENV ?= /etc/dancer/deploy.env
STATE      ?= /var/lib/dancer/deployed.sha

.DEFAULT_GOAL := help
.PHONY: help build run run-terminal setup doctor test test-race test-live e2e restart-drill auto-resume-drill lint fmt tidy clean \
        install uninstall service-install service-uninstall service-restart service-status service-logs \
        update-install update-uninstall update-now update-status update-logs deploy-env

## ---- build -----------------------------------------------------------------

build: ## Build bin/dancer
	$(GO) build -o bin/dancer ./cmd/dancer

clean: ## Remove build output
	rm -rf bin

## ---- run locally -------------------------------------------------------------

run: build ## Run with the configured transports (Slack) — CONFIG=$(CONFIG)
	bin/dancer run -config $(CONFIG)

run-terminal: build ## Run with the terminal transport instead of Slack
	bin/dancer run -config $(CONFIG) -terminal

setup: build ## Interactive first-time wizard (writes CONFIG, then runs doctor)
	bin/dancer setup -config $(CONFIG)

doctor: build ## Check config, claude login, docker, ssh hosts, Slack tokens
	bin/dancer doctor -config $(CONFIG)

## ---- test -------------------------------------------------------------------

test: ## Unit tests (docker/ssh tests use a real daemon / throwaway sshd; skipped if absent)
	$(GO) test ./...

test-race: ## Unit tests with the race detector
	$(GO) test -race -count=1 ./...

test-live: ## Also drive the real claude CLI (haiku, a few cents)
	DANCER_LIVE=1 $(GO) test -count=1 ./...

restart-drill: build ## SIGTERM mid-tool-call: drain, notify, restart, resume (needs logged-in claude)
	python3 scripts/restart-drill.py bin/dancer

auto-resume-drill: build ## SIGTERM mid-tool-call, then check the task finishes itself (needs logged-in claude)
	python3 scripts/auto-resume-drill.py bin/dancer

e2e: build ## Whole binary through the terminal transport (needs logged-in claude)
	python3 scripts/e2e.py bin/dancer

lint: ## gofmt check + go vet
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	$(GO) vet ./...

fmt: ## gofmt the tree
	gofmt -w .

tidy: ## go mod tidy
	$(GO) mod tidy

## ---- install as a service -----------------------------------------------------

install: build ## Copy the binary to BIN=$(BIN)   (SUDO= to skip sudo)
	$(SUDO) install -d $(dir $(BIN))
	$(SUDO) install -m 0755 bin/dancer $(BIN)

uninstall: ## Remove the binary
	-$(SUDO) rm -f $(BIN)

service-install: install deploy-env ## Install + enable + start the systemd unit for USER_=$(USER_)
	sed -e 's|__USER__|$(USER_)|g' -e 's|__GROUP__|$(GROUP_)|g' -e 's|__HOME__|$(HOME_)|g' -e 's|__BIN__|$(BIN)|g' \
		deploy/dancer.service | sudo tee $(UNIT) > /dev/null
	sudo systemctl daemon-reload
	sudo systemctl enable --now dancer
	@$(MAKE) --no-print-directory service-status

service-uninstall: ## Stop + disable + remove the unit and the binary
	-sudo systemctl disable --now dancer
	-sudo rm -f $(UNIT)
	sudo systemctl daemon-reload
	@$(MAKE) --no-print-directory uninstall

service-restart: install ## Rebuild, reinstall the binary, restart the service
	sudo systemctl restart dancer
	@$(MAKE) --no-print-directory service-status

service-status: ## Show service status
	systemctl status dancer --no-pager | head -8

service-logs: ## Follow service logs
	journalctl -u dancer -f

## ---- auto-update from git ---------------------------------------------------

deploy-env: ## Record install settings in DEPLOY_ENV=$(DEPLOY_ENV) (the updater re-renders units from it)
	@test -n "$(REPO)" || { echo "no git remote 'origin' here; pass REPO=https://github.com/you/dancer"; exit 1; }
	@printf '%s\n' \
		"# Written by 'make deploy-env'. Plain values only — systemd reads this as an" \
		"# EnvironmentFile and the updater sources it. Re-run make to change it." \
		"DANCER_REPO=$(REPO)" \
		"DANCER_BRANCH=$(BRANCH)" \
		"DANCER_SRC=$(SRC)" \
		"DANCER_BIN=$(BIN)" \
		"DANCER_SERVICE=dancer.service" \
		"DANCER_UPDATE_STATE=$(STATE)" \
		"GO=$(GO_BIN)" \
		"DANCER_USER=$(USER_)" \
		"DANCER_GROUP=$(GROUP_)" \
		"DANCER_HOME=$(HOME_)" \
		"DANCER_INTERVAL=$(INTERVAL)" \
		"DANCER_UPDATER=$(UPDATER)" \
		"DANCER_UNIT=$(UNIT)" \
		"DANCER_UPDATE_UNIT=$(UPDATE_UNIT)" \
		"DANCER_UPDATE_TIMER=$(UPDATE_TIMER)" \
		| $(SUDO) install -D -m 0644 /dev/stdin $(DEPLOY_ENV)
	@echo "wrote $(DEPLOY_ENV)"

update-install: deploy-env ## Poll REPO for new commits every INTERVAL=$(INTERVAL), rebuild, restart (SRC=$(SRC))
	@test -n "$(GO_BIN)" || { echo "go not found on PATH; pass GO_BIN=/path/to/go"; exit 1; }
	@test -n "$(REPO)" || { echo "no git remote 'origin' here; pass REPO=https://github.com/you/dancer"; exit 1; }
	sudo install -d $(dir $(UPDATER))
	sudo install -m 0755 scripts/dancer-update.sh $(UPDATER)
	sed -e 's|__REPO__|$(REPO)|g' -e 's|__BRANCH__|$(BRANCH)|g' -e 's|__SRC__|$(SRC)|g' \
		-e 's|__BIN__|$(BIN)|g' -e 's|__GO__|$(GO_BIN)|g' -e 's|__UPDATER__|$(UPDATER)|g' \
		deploy/dancer-update.service | sudo tee $(UPDATE_UNIT) > /dev/null
	sed -e 's|__INTERVAL__|$(INTERVAL)|g' deploy/dancer-update.timer | sudo tee $(UPDATE_TIMER) > /dev/null
	sudo systemctl daemon-reload
	sudo systemctl enable --now dancer-update.timer
	@$(MAKE) --no-print-directory update-status

update-uninstall: ## Stop the timer and remove the updater (leaves SRC, state and the binary)
	-$(SUDO) systemctl disable --now dancer-update.timer
	-$(SUDO) rm -f $(UPDATE_UNIT) $(UPDATE_TIMER) $(UPDATER) $(DEPLOY_ENV)
	$(SUDO) systemctl daemon-reload

update-now: ## Run one update immediately instead of waiting for the timer
	@sudo systemctl start dancer-update.service; rc=$$?; \
		journalctl -u dancer-update.service -n 30 --no-pager; \
		exit $$rc

update-status: ## Show when the update timer last ran and next fires
	systemctl list-timers dancer-update.timer --no-pager
	@systemctl status dancer-update.service --no-pager | head -6

update-logs: ## Follow updater logs
	journalctl -u dancer-update.service -f

## ---- help -------------------------------------------------------------------

help: ## Show this help
	@awk 'BEGIN{FS=":.*## "} /^## ----/{sub(/^## ---- /,""); sub(/ -+$$/,""); printf "\n%s\n",$$0; next} /^[a-zA-Z_-]+:.*## /{printf "  %-20s %s\n",$$1,$$2}' $(MAKEFILE_LIST)
