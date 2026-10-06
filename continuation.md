# M2 continuation

Branch: ci/local-parity-m2. Base: 1ecf8482465d3fdede1466ce9c51ff2e4a9a78c4.

Read plan sections 1/6/9/10 and decisions. M2 implementation applied: shared CI Make targets, local fixture wrapper and timing runner, exact Go toolchain, tag CI authorization, pinned provenance action, clean-clone reproducibility helper, workflow tests. No internal Go source changed. User requires local commits only; no push/tag/release/convergence.

Focused tests: 51 passed; lint passed. Initial committed gate found an awk dollar escape and the pinned external Steward fixture's Go 1.25 requirement; corrected escape and selected exact Go 1.25.7 for all gates/builds.

Preliminary full ci-local: all Go/race gates passed; Python failed 34 tests because worker populated tracked fixture files as 0600 (expected 0644). Refreshing clean tracked files via Git under umask 022 restores checkout permissions without changing source or weakening tests. Added local failure dependency/matrix parity, pinned gofmt through the selected GOROOT, and corrected the pre-existing quoted-tilde shellcheck warning in release notes.

Next: commit final source, refresh clean Git checkout permissions, foreground umask-022 ci-local on clean committed source with complete verification.log, reproducibility and scratch noncanonical manifest refusal, measure proposed fast subset, final handoff. Huyang prepared revision was applied with provisional diagnostics; actual CLI validation remains required.
