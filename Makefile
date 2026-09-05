.PHONY: test lint check

# Thin wrapper over the Taskfile targets so `make test` and `make lint` work
# from a clean checkout without task installed. The commands mirror the
# Taskfile's test and lint tasks exactly; CI runs the same commands.
#
# @joestump-agent 09/05/2026 - Added so the repo exposes the uniform
# make test / make lint entry points.

test:
	go test -race -failfast ./...

lint:
	./scripts/check_log_capitalization.sh
	GOEXPERIMENT= golangci-lint run --path-mode=abs --config=".golangci.yml" --timeout=5m

check: test lint
