.PHONY: test lint syntax check-mode redaction gate

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
