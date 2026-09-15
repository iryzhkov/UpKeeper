# Release test fixtures

`paired-v1.json` is the exact public `desired/release.json` from UpKeeper commit
`4cf0a20`. It contains the paired component set before optional fleet intent.
Its SHA-256 is `4af4e0281126f3523f256106ce85d5adf6d2e29ef76ae7141d584780fc81fc5a`.

Keep these bytes frozen: the canonical JSON tests retain independently established
Python digest vectors for this document. Release and CLI tests explicitly add
fleet intent or construct legacy components from this baseline. New captures must
not silently change those test inputs. A separate release test validates the
current published manifest and checks its canonical bytes and file identity.
