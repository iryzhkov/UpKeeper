.PHONY: test lint syntax check-mode redaction gate
.PHONY: go-build go-test go-race go-vet go-format go-cross go-gate

test:
	uv run pytest

lint:
	uv run ruff check src tests scripts
	uv run python scripts/run-captured ansible-lint --offline playbooks

syntax:
	uv run python scripts/run-captured ansible-playbook playbooks/status.yml --syntax-check

check-mode:
	uv run python scripts/run-captured ansible-playbook -i tests/fixtures/localhost_inventory.yml playbooks/status.yml --check

redaction:
	uv run pytest tests/test_redaction.py

gate: lint test syntax check-mode redaction

# The Go binary is the production path. The Python gate above stays as the
# reference implementation and the rollback path until the canary has passed.

GO_LDFLAGS = -X github.com/iryzhkov/UpKeeper/internal/buildinfo.Commit=$(shell git rev-parse HEAD)

# A release artifact is built without cgo so that one binary runs on every host
# the fleet has. A dynamically linked build picks up the glibc of the machine
# that produced it, and Debian 12 on homelab is older than Arch on the others.
GO_RELEASE_ENV = CGO_ENABLED=0

go-build:
	$(GO_RELEASE_ENV) go build -ldflags "$(GO_LDFLAGS)" -o bin/upkeeper ./cmd/upkeeper

go-test:
	go test ./...

go-race:
	go test -race ./...

go-vet:
	go vet ./...

go-format:
	test -z "$$(gofmt -l . )"

# Both fleet architectures are built from one source commit so that a release
# can never ship an artifact nobody checked.
go-cross:
	$(GO_RELEASE_ENV) GOOS=linux GOARCH=amd64 go build -ldflags "$(GO_LDFLAGS)" -o bin/upkeeper-linux-amd64 ./cmd/upkeeper
	$(GO_RELEASE_ENV) GOOS=linux GOARCH=arm64 go build -ldflags "$(GO_LDFLAGS)" -o bin/upkeeper-linux-arm64 ./cmd/upkeeper

go-gate: go-format go-vet go-test go-race go-cross
