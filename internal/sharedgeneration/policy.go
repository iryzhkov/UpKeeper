package sharedgeneration

import (
	"bytes"
	"errors"
	"unicode/utf8"

	"github.com/iryzhkov/UpKeeper/internal/author"
	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

const policyLimit = 1 << 20

func validPolicy(p *author.SharedPolicy) bool {
	return p != nil && pin(p.InputManifestSHA256, 64) && pin(p.CanonicalPolicySHA256, 64) &&
		p.InputContract == "canonical-release-manifest-v1" && p.PolicyFormat == "route-policy/v1-canonical-json" &&
		p.Derivation == "author-derive-policy-v1" && p.UpstreamRepository == "github.com/iryzhkov/omarchy-setup" &&
		validUpstreamPair(p.UpstreamCommit, p.UpstreamTree) &&
		p.Interpretation == "steward-parser-and-pinned-generator" &&
		p.ConfiguredCatalogValidation == "completed-at-preparation" &&
		p.CurrentAuthorization == "unknown" && p.Readiness == "unassessed"
}

// Offline receipt reading preserves only the two reviewed, indivisible identities.
// New preparation remains bound to the current source in author.
func validUpstreamPair(commit, tree string) bool {
	return commit == "33cb805b5482d511071be7b344140e2ccb8c87da" && tree == "3439bd874573cb88bbad852c5b66771f2ab80eeb" ||
		commit == "b32c0b6f48e12b7d74a60f8a010f84bab73d299f" && tree == "fc0e7ad2bad7a7df96030f71696125b5a7d5bd52"
}

func allowedFileMode(version int, f File) bool {
	if version == 1 {
		return f.Mode == 0644
	}
	switch f.Kind {
	case "instruction":
		return f.Mode == 0600 || f.Mode == 0644 || f.Mode == 0700 || f.Mode == 0755
	case "skill":
		return f.Mode == 0644
	case "policy-input":
		return f.Mode == 0600
	}
	return false
}

// Inspect checks canonical encoding and content closure only. Policy semantics
// and configured catalog validation belong to preparation's owning validators.
func validatePolicyBytes(b []byte) error {
	if len(b) == 0 || len(b) > policyLimit {
		return errors.New("policy input size")
	}
	if !utf8.Valid(b) {
		return errors.New("policy input encoding")
	}
	// Policy semantics and structure limits remain owned by the preparation
	// validator. Inspection checks only duplicate-safe canonical JSON identity.
	v, e := canonjson.DecodeObject(b)
	if e != nil {
		return e
	}
	canonical, e := canonjson.MarshalFile(v)
	if e != nil {
		return e
	}
	if !bytes.Equal(b, canonical) {
		return errors.New("noncanonical policy input")
	}
	return nil
}
