.PHONY: sync test lint syntax check-mode redaction gate
.PHONY: go-build go-test go-race go-vet go-format go-cross go-gate

# Every Python target needs the locked development environment, which a fresh
# checkout does not have: `uv run pytest` fails with "Failed to spawn: pytest"
# until the extras are installed. uv sync is idempotent and quick when nothing
# changed, so the targets depend on it directly rather than on a marker file.
sync:
	uv sync --all-extras

test: sync
	uv run pytest

lint: sync
	uv run ruff check src tests scripts
	uv run python scripts/run-captured ansible-lint --offline playbooks

syntax: sync
	uv run python scripts/run-captured ansible-playbook playbooks/status.yml --syntax-check

check-mode: sync
	uv run python scripts/run-captured ansible-playbook -i tests/fixtures/localhost_inventory.yml playbooks/status.yml --check

redaction: sync
	uv run pytest tests/test_redaction.py

gate: lint test syntax check-mode redaction

# The Go binary is the production path. The Python gate above stays as the
# reference implementation and the rollback path until the canary has passed.

# A build at a tag names that tag; anything else keeps the development version.
# A released artifact that calls itself 0.0.0-dev is not evidence of a release.
GO_VERSION = $(shell git describe --tags --exact-match 2>/dev/null || echo 0.0.0-dev)

GO_LDFLAGS = \
	-X github.com/iryzhkov/UpKeeper/internal/buildinfo.Commit=$(shell git rev-parse HEAD) \
	-X github.com/iryzhkov/UpKeeper/internal/buildinfo.Version=$(GO_VERSION)

# A release artifact is built without cgo so that one binary runs on every host
# the fleet has. A dynamically linked build picks up the glibc of the machine
# that produced it, and Debian 12 on homelab is older than Arch on the others.
GO_RELEASE_ENV = CGO_ENABLED=0

# -trimpath keeps the absolute path of the checkout out of the binary. Without
# it two clean checkouts of the same commit produce different bytes, so the
# checksum a release publishes cannot be reproduced by anyone who did not build
# in the same directory, and the checksum stops being evidence.
GO_RELEASE_FLAGS = -trimpath

go-build:
	$(GO_RELEASE_ENV) go build $(GO_RELEASE_FLAGS) -ldflags "$(GO_LDFLAGS)" -o bin/upkeeper ./cmd/upkeeper

go-test:
	go test ./...

# The full race gate, run as three exhaustive groups so that no one race binary
# carries the whole author package into the default ten-minute alarm. The script
# derives the groups from the real inventory and refuses any gap or overlap.
go-race:
	python3 scripts/go-race-shards.py --group all

go-vet:
	go vet ./...

go-format:
	test -z "$$(gofmt -l . )"

# Both fleet architectures are built from one source commit so that a release
# can never ship an artifact nobody checked.
go-cross:
	$(GO_RELEASE_ENV) GOOS=linux GOARCH=amd64 go build $(GO_RELEASE_FLAGS) -ldflags "$(GO_LDFLAGS)" -o bin/upkeeper-linux-amd64 ./cmd/upkeeper
	$(GO_RELEASE_ENV) GOOS=linux GOARCH=arm64 go build $(GO_RELEASE_FLAGS) -ldflags "$(GO_LDFLAGS)" -o bin/upkeeper-linux-arm64 ./cmd/upkeeper

go-gate: go-format go-vet go-test go-race go-cross
