#!/usr/bin/env bash
set -euo pipefail
: "${RUNNER_TEMP:?}" "${GITHUB_ENV:?}" "${GITHUB_PATH:?}"
export GIT_NO_REPLACE_OBJECTS=1
fixture_root=$(mktemp -d "$RUNNER_TEMP/upkeeper-policy-tests.XXXXXX")
steward_commit=d2327563e90e9b66adaf80c5a7a03746fce16763
generator_commit=88d9b534a6648e2615037dc3a655e6a8d0e9bebc
shared_generator_commit=b32c0b6f48e12b7d74a60f8a010f84bab73d299f
shared_generator_tree=fc0e7ad2bad7a7df96030f71696125b5a7d5bd52
git clone --quiet --no-checkout https://github.com/iryzhkov/t3-steward.git "$fixture_root/steward"
git -C "$fixture_root/steward" checkout --quiet --detach "$steward_commit"
test "$(git -C "$fixture_root/steward" rev-parse HEAD)" = "$steward_commit"
mkdir "$fixture_root/bin"
(cd "$fixture_root/steward" && CGO_ENABLED=0 go build -o "$fixture_root/bin/t3-steward" ./cmd/t3-steward)
test -x "$fixture_root/bin/t3-steward"
git clone --quiet --no-checkout https://github.com/iryzhkov/omarchy-setup.git "$fixture_root/setup"
git -C "$fixture_root/setup" checkout --quiet --detach "$generator_commit"
test "$(git -C "$fixture_root/setup" rev-parse HEAD)" = "$generator_commit"
git clone --quiet --no-checkout https://github.com/iryzhkov/omarchy-setup.git "$fixture_root/shared-setup"
git -C "$fixture_root/shared-setup" checkout --quiet --detach "$shared_generator_commit"
test "$(git -C "$fixture_root/shared-setup" rev-parse HEAD)" = "$shared_generator_commit"
test "$(git -C "$fixture_root/shared-setup" rev-parse 'HEAD^{tree}')" = "$shared_generator_tree"
printf '%s\n' "$fixture_root/bin" >> "$GITHUB_PATH"
printf 'M14_GENERATOR_REPOSITORY=%s\nM14_GENERATOR_COMMIT=%s\nM14_SHARED_GENERATOR_REPOSITORY=%s\nM14_SHARED_GENERATOR_COMMIT=%s\n' \
  "$fixture_root/setup" "$generator_commit" "$fixture_root/shared-setup" "$shared_generator_commit" >> "$GITHUB_ENV"
