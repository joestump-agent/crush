.PHONY: test lint check tck

# Thin wrapper over the Taskfile targets so `make test` and `make lint` work
# from a clean checkout without task installed. The commands mirror the
# Taskfile's test and lint tasks exactly; CI runs the same commands.
# `make tck` runs the a2a Technology Compatibility Kit against a locally
# served harness; see scripts/run_tck.sh for the checkout it expects.
#
# @joestump-agent 09/05/2026 - Added so the repo exposes the uniform
# make test / make lint entry points.
#
# @joestump-agent 10/06/2026 - Added the tck target wrapping
# scripts/run_tck.sh for #363.

TCK_PATH ?= $(HOME)/src/a2a-tck

test:
	go test -race -failfast ./...

lint:
	./scripts/check_log_capitalization.sh
	GOEXPERIMENT= golangci-lint run --path-mode=abs --config=".golangci.yml" --timeout=5m

check: test lint

tck:
	./scripts/run_tck.sh $(TCK_PATH)
