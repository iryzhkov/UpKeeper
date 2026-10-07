.PHONY: sync test lint syntax check-mode redaction gate
.PHONY: go-build go-test go-race go-vet go-format go-cross go-gate

# A toolchain directive is a minimum when a newer Go is installed. Force the
# exact compiler for local gates and released bytes, deriving it from go.mod.
export GOTOOLCHAIN := $(shell awk '/^toolchain / {print $$2}' go.mod)

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

# The Go binary is the production path and binary rollback is `upkeeper self
# rollback`. The Python gate above still checks src/dev_fleet until that package
# is removed; it is no longer a rollback path.

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
# Go's automatic VCS/module stamping differs between linked worktrees and
# ordinary clones. The explicit ldflags above already carry commit and version.
GO_RELEASE_FLAGS = -trimpath -buildvcs=false

go-build:
	$(GO_RELEASE_ENV) go build $(GO_RELEASE_FLAGS) -ldflags "$(GO_LDFLAGS)" -o bin/upkeeper ./cmd/upkeeper

go-test:
	go test ./...

# The full race gate, run as four exhaustive groups so that no one race binary
# carries the whole author package into the default ten-minute alarm. The script
# derives the groups from the real inventory and refuses any gap or overlap.
go-race:
	python3 scripts/go-race-shards.py --group all

go-vet:
	go vet ./...

go-format:
	test -z "$$("$$(go env GOROOT)/bin/gofmt" -l .)"

# Both fleet architectures are built from one source commit so that a release
# can never ship an artifact nobody checked.
go-cross:
	$(GO_RELEASE_ENV) GOOS=linux GOARCH=amd64 go build $(GO_RELEASE_FLAGS) -ldflags "$(GO_LDFLAGS)" -o bin/upkeeper-linux-amd64 ./cmd/upkeeper
	$(GO_RELEASE_ENV) GOOS=linux GOARCH=arm64 go build $(GO_RELEASE_FLAGS) -ldflags "$(GO_LDFLAGS)" -o bin/upkeeper-linux-arm64 ./cmd/upkeeper

go-gate: go-format go-vet go-test go-race go-cross

.PHONY: ci-manifest ci-manifest-check ci-go-checks ci-go-checks-run
.PHONY: ci-race ci-race-run ci-python ci-go-gate ci-local go-reproducible
GROUP ?= all
CI_UV = .local-runs/ci-tools/bin/uv

# Each CI job and the local aggregate enter the same targets. The wrapper
# prepares immutable policy fixtures and scopes their environment to this run.
ci-manifest:
	bash scripts/with-policy-test-inputs.sh $(MAKE) ci-manifest-check

ci-manifest-check:
	go test -count=1 -run '^TestCurrentPublishedManifestValidatesAndIsCanonical$$' ./internal/release

ci-go-checks:
	bash scripts/with-policy-test-inputs.sh $(MAKE) ci-go-checks-run

ci-go-checks-run:
	$(MAKE) go-format
	$(MAKE) go-vet
	$(MAKE) go-test
	$(MAKE) go-cross
	sha256sum bin/upkeeper-linux-amd64 bin/upkeeper-linux-arm64
	$(MAKE) go-build
	./bin/upkeeper version

ci-race:
	bash scripts/with-policy-test-inputs.sh $(MAKE) ci-race-run GROUP="$(GROUP)"

ci-race-run:
	python3 scripts/go-race-shards.py --group "$(GROUP)"

# Bootstrap uv into an isolated environment: works on externally managed
# Python installations and uses the same uv/Python versions as hosted CI.
ci-python:
	python3 -m venv .local-runs/ci-tools
	.local-runs/ci-tools/bin/python -m pip install uv==0.12.12
	$(CI_UV) sync --locked --all-extras --python 3.11
	$(CI_UV) run --no-sync ansible-galaxy collection install -r requirements.yml
	$(CI_UV) run --no-sync ruff check src tests scripts
	$(CI_UV) run --no-sync yamllint requirements.yml inventory desired playbooks tests/fixtures .forgejo .github
	$(CI_UV) run --no-sync python scripts/run-captured ansible-lint --offline playbooks
	$(CI_UV) run --no-sync pytest
	$(CI_UV) run --no-sync pytest tests/test_redaction.py
	$(CI_UV) run --no-sync python scripts/run-captured ansible-playbook playbooks/status.yml --syntax-check
	$(CI_UV) run --no-sync python scripts/run-captured ansible-playbook -i tests/fixtures/localhost_inventory.yml playbooks/status.yml --check

# GitHub supplies dependency outcomes; local orchestration supplies them only
# after all three Go jobs pass. Missing/skipped/failing outcomes fail closed.
ci-go-gate:
	@echo "manifest-gate result: $$MANIFEST_RESULT"
	@echo "go-checks result: $$CHECKS_RESULT"
	@echo "go-race result: $$RACE_RESULT"
	test "$$MANIFEST_RESULT" = success
	test "$$CHECKS_RESULT" = success
	test "$$RACE_RESULT" = success

ci-local:
	python3 scripts/ci-local.py

# Two independent clean Git clones and empty build caches, not a repeat build
# in the same directory. The helper refuses a dirty tracked source tree.
go-reproducible:
	python3 scripts/check-reproducible.py
