# Recovery and off-homelab copy

Forgejo at `git.ryzhkov.dev` is the primary collaboration remote and must be named `gitea`.
It runs on `homelab`, so it cannot be the only recovery copy.

The Phase 1 recovery design uses a private, read-only operational mirror at
`git@github.com:iryzhkov/dev-fleet.git`, named `github` locally. GitHub is not a deployment
source unless it is explicitly listed and its repository identity is verified. Normal work pushes
to `gitea` first; the mirror receives the same reviewed commit after that succeeds.

A recovery drill is:

```bash
git clone --origin github git@github.com:iryzhkov/dev-fleet.git dev-fleet-recovery
cd dev-fleet-recovery
git fsck --full
git verify-commit HEAD  # required once signing is enabled; until then record that it is unsigned
git remote add gitea git@git.ryzhkov.dev:igor/dev-fleet.git
git remote -v
```

The drill must run outside homelab and compare the recovered commit to the last known mirrored
commit. It may not silently promote the mirror to primary or deploy an unreviewed mirror-only
commit. No scheduled mirror job is installed in Phase 1; that would add credentials and an
unattended service outside this session's scope.

If GitHub mirroring cannot be created safely, create an encrypted git bundle in a later explicit
operation. Its age private key must not live only in the house, and restore testing is part of the
design rather than merely producing a bundle.
