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

.DEFAULT_GOAL := help
.PHONY: help build run run-terminal setup doctor test test-race test-live e2e lint fmt tidy clean \
        install uninstall service-install service-uninstall service-restart service-status service-logs

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

install: build ## Copy the binary to BIN=$(BIN)
	install -d $(dir $(BIN))
	install -m 0755 bin/dancer $(BIN)

uninstall: ## Remove the binary
	-sudo rm -f $(BIN)

service-install: install ## Install + enable + start the systemd unit for USER_=$(USER_)
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

## ---- help -------------------------------------------------------------------

help: ## Show this help
	@awk 'BEGIN{FS=":.*## "} /^## ----/{sub(/^## ---- /,""); sub(/ -+$$/,""); printf "\n%s\n",$$0; next} /^[a-zA-Z_-]+:.*## /{printf "  %-20s %s\n",$$1,$$2}' $(MAKEFILE_LIST)
