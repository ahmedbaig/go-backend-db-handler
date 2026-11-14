APP  := db-handler
CMD  := ./cmd/db-handler
PORT := 8090

include make/go.mk

.PHONY: gen setup

gen: ## introspect the databases in ./var/db-handler-db.json and regenerate models
	CONFIG_DIR=$(CONFIG_DIR) go run ./cmd/db-gen

setup: ## copy sample configs into ./var
	mkdir -p var
	for f in configs/*.json.sample; do b=$$(basename $$f .sample); [ -f var/$$b ] || cp $$f var/$$b; done
