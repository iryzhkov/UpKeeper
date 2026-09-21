# Canonical controlled Codex fixture

`codex-controlled-canonical-x86_64.tar.zst` was generated from Codex commit
`28dc6d3` using the canonical `scripts.codex_package.layout` and
`scripts.codex_package.archive` modules. Its SHA-256 is
`c4a175a28bba90e9f25911389d86b7d10bd945392f9902ffb69d82d8f798cd17`.

The generator created executable shell stubs named `codex`,
`codex-code-mode-host`, `rg`, and `bwrap`, constructed
`PackageInputs`, called `prepare_package_dir`, `build_package_dir` with
version `0.155.1`, variant `codex`, and target
`x86_64-unknown-linux-musl`, called `validate_package_dir`, then called
`write_archive(..., canonical.tar.zst, force=True)`. This intentionally
exercises the producer's directory headers, metadata writer, executable mode
normalization, sorted member order, tar implementation, and zstd writer.
