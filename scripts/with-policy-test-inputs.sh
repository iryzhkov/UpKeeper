#!/usr/bin/env bash
# Run a gate with the same immutable policy fixtures locally and on Actions.
set -euo pipefail
if [[ "${UPKEEPER_POLICY_INPUTS_READY:-}" == 1 ]]; then
  exec "$@"
fi
fixture_temp=$(mktemp -d "${TMPDIR:-/tmp}/upkeeper-ci-inputs.XXXXXX")
trap 'rm -rf "$fixture_temp"' EXIT
export RUNNER_TEMP="$fixture_temp"
export GITHUB_ENV="$fixture_temp/environment"
export GITHUB_PATH="$fixture_temp/path"
bash scripts/prepare-policy-test-inputs.sh
set -a
source "$GITHUB_ENV"
set +a
export PATH="$(cat "$GITHUB_PATH"):$PATH"
export UPKEEPER_POLICY_INPUTS_READY=1
"$@"
