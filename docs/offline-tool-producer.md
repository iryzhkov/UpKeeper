# Offline tool producer

`toolproducer.Compile(Inputs) (Result, error)` is an offline library. It takes explicit absolute physical paths and hash pins, checks the accepted closed canonical version 1 schemas, finite pilot vocabulary and selection, and identity-v1 Git commit/tree/blob/mode correspondence. No CLI or active writer calls it.

The producer closure is exactly ten sorted owning files: go.mod, go.sum, canonjson.go, catalog.go, both inspector platform files and the four toolproducer implementation files. The immutable producer commit is supplied by the caller and is never embedded in its own source. Testdata pins are shape examples; tests create real disposable Git repositories and regenerate all source/descriptor/manifest/input pins.

Linux acquisition uses nofollow held inode metadata before data-open, approved modes, single-link regular files, bounded reads and final physical binding/content checks. Other platforms refuse. Optional incompatible tools and unused artifacts/sources are pruned from catalog only. Optional missing artifacts retain declarations; present corruption and required missing/incompatible tools refuse.

Only after unchanged toolcatalog.Inspect verifies the private scratch catalog does Compile return catalog and separate producer receipt bytes and SHA256 pins. Scratch is removed before return. Any failure returns an empty Result. Git origin configuration is a trusted local identity statement, not cryptographic authentication. Per-file observations do not establish an atomic filesystem snapshot.

Catalog and receipt remain candidate-inert declarations. They establish neither binary build lineage nor authorization, installation, enforcement or readiness. Huyang is the sole repository route, agent99 is excluded, and descriptors contain no executable commands. Stage B, host capture, PATH discovery and active integration are outside this library.

Run the unit/race producer and unchanged toolcatalog tests, full Go gate, then Python/YAML gates with distinct pinned original-88, d232 consumer and current-b32 prerequisites. Physical tests inspect the actual existing /dev/null device inode without data-open, sudo or device creation. Unchanged opt-in/CAP_MKNOD skips are gaps; new semantics may not skip.
