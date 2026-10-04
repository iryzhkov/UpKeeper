#!/usr/bin/env bash
set -euo pipefail
: "${RUNNER_TEMP:?}" "${GITHUB_ENV:?}" "${GITHUB_PATH:?}"
fixture_root=$(mktemp -d "$RUNNER_TEMP/upkeeper-policy-tests.XXXXXX")
steward_commit=d2327563e90e9b66adaf80c5a7a03746fce16763
generator_commit=88d9b534a6648e2615037dc3a655e6a8d0e9bebc
git clone --quiet --no-checkout https://github.com/iryzhkov/t3-steward.git "$fixture_root/steward"
git -C "$fixture_root/steward" checkout --quiet --detach "$steward_commit"
test "$(git -C "$fixture_root/steward" rev-parse HEAD)" = "$steward_commit"
mkdir "$fixture_root/bin"
(cd "$fixture_root/steward" && CGO_ENABLED=0 go build -o "$fixture_root/bin/t3-steward" ./cmd/t3-steward)
git clone --quiet --no-checkout https://github.com/iryzhkov/omarchy-setup.git "$fixture_root/setup"
git -C "$fixture_root/setup" checkout --quiet --detach "$generator_commit"
test "$(git -C "$fixture_root/setup" rev-parse HEAD)" = "$generator_commit"
printf '%s\n' "$fixture_root/bin" >> "$GITHUB_PATH"
printf 'M14_GENERATOR_REPOSITORY=%s\nM14_GENERATOR_COMMIT=%s\n' "$fixture_root/setup" "$generator_commit" >> "$GITHUB_ENV"
