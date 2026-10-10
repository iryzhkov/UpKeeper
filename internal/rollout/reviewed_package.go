package rollout

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
)

// reviewedPackage contains only preparation evidence. Runtime receipts never
// become part of its digest; every frozen field remains an acceptance fence.
type reviewedPackage struct {
	Head, Release, ConfigDigest string
	TimerPrior                  map[string]string
	Evidence                    map[string]json.RawMessage
}

func acceptedPackage(c Config, s *State) (*reviewedPackage, error) {
	if s.PackageDigest == "" {
		return nil, errors.New("missing reviewed package digest")
	}
	raw, err := os.ReadFile(c.StatePath + ".review-package.json")
	if err != nil {
		return nil, err
	}
	if digest(raw) != s.PackageDigest {
		return nil, errors.New("accepted package bytes changed")
	}
	review, err := os.ReadFile(c.ReviewFile)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.ReplaceAll(string(review), "\r\n", "\n"), "\n")
	packageReferences := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "PACKAGE ") {
			packageReferences++
		}
	}
	if len(lines) == 0 || lines[0] != "VERDICT: ACCEPT" || packageReferences != 1 || !contains(lines, "PACKAGE "+s.PackageDigest) || !strings.Contains(string(review), s.ExpectHead) || !strings.Contains(string(review), s.ReleaseSHA256) {
		return nil, errors.New("review does not bind exact package and publication fences")
	}
	var pkg reviewedPackage
	if err = decodeDiagnostic(raw, &pkg); err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if len(fields) != 5 {
		return nil, errors.New("reviewed package field inventory differs")
	}
	for _, key := range []string{"Head", "Release", "ConfigDigest", "TimerPrior", "Evidence"} {
		if fields[key] == nil {
			return nil, fmt.Errorf("missing exact reviewed package field %s", key)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&pkg); err != nil {
		return nil, err
	}
	if pkg.Head == "" || pkg.Release == "" || pkg.Head != s.ExpectHead || pkg.Release != s.ReleaseSHA256 || pkg.ConfigDigest != c.Digest() || s.ConfigDigest != pkg.ConfigDigest {
		return nil, errors.New("accepted package binding mismatch")
	}
	if pkg.TimerPrior == nil || !reflect.DeepEqual(pkg.TimerPrior, s.TimerPrior) {
		return nil, errors.New("accepted timer fence baseline changed")
	}
	for _, h := range c.TimerHosts {
		if pkg.TimerPrior[h] == "" {
			return nil, fmt.Errorf("missing reviewed timer baseline %s", h)
		}
	}
	for _, key := range []string{"author-started", "release.diff", "release-before", "release-after", "fleet-plan", "preview-full", "preview-scoped", "preview-rollback", "assets", "rc118-findings"} {
		if len(pkg.Evidence[key]) == 0 {
			return nil, fmt.Errorf("missing reviewed evidence %s", key)
		}
	}
	for key, frozen := range pkg.Evidence {
		// Marshal compaction tolerates the journal encoder's whitespace only.
		var a, z bytes.Buffer
		if err := json.Compact(&a, frozen); err != nil {
			return nil, err
		}
		if err := json.Compact(&z, s.Extra[key]); err != nil || !bytes.Equal(a.Bytes(), z.Bytes()) {
			return nil, fmt.Errorf("immutable reviewed evidence changed: %s", key)
		}
	}
	if _, err := packageComponents(c, &pkg); err != nil {
		return nil, err
	}
	return &pkg, nil
}

func acceptedAssets(c Config, s *State, v *assetEvidence) error {
	pkg, err := acceptedPackage(c, s)
	if err != nil {
		return err
	}
	return decodeDiagnostic(pkg.Evidence["assets"], v)
}

// This envelope comes from converge.Run, including the dirty manifest's parent
// commit. Inventory equality alone cannot authorize effects for another release.
func fullPreviewBinding(p preview, head, release string) error {
	canonicalHex := func(value string, length int) bool {
		if len(value) != length {
			return false
		}
		for _, ch := range value {
			if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
				return false
			}
		}
		return true
	}
	if p.SchemaVersion == nil || *p.SchemaVersion != 1 || p.Command != "pull" ||
		p.DryRun == nil || !*p.DryRun || p.ExitCode == nil || *p.ExitCode != 0 {
		return errors.New("reviewed full preview producer schema differs")
	}
	if !canonicalHex(p.ReleaseSHA256, 64) || !canonicalHex(release, 64) ||
		p.ReleaseSHA256 != release {
		return errors.New("reviewed full preview release digest differs")
	}
	if !canonicalHex(p.DesiredManifestCommit, 40) || !canonicalHex(head, 40) ||
		p.DesiredManifestCommit != head {
		return errors.New("reviewed full preview preparation parent differs")
	}
	return nil
}

// The release declares the component universe; the reviewed full preview binds
// that complete universe to each intended host, including self-managed laptop.
func packageComponents(c Config, pkg *reviewedPackage) (map[string]map[string]bool, error) {
	var release struct{ Components map[string]json.RawMessage }
	if err := decodeDiagnostic(pkg.Evidence["release-after"], &release); err != nil {
		return nil, err
	}
	if len(release.Components) == 0 {
		return nil, errors.New("reviewed release component inventory absent")
	}
	var fp preview
	if err := decodeDiagnostic(pkg.Evidence["preview-full"], &fp); err != nil {
		return nil, err
	}
	if err := fullPreviewBinding(fp, pkg.Head, pkg.Release); err != nil {
		return nil, err
	}
	expected := map[string]map[string]bool{}
	for _, h := range fp.Hosts {
		host := h.Host
		if h.Host != "" && h.Name != "" && h.Host != h.Name {
			return nil, errors.New("conflicting reviewed full preview host references")
		}
		if host == "" {
			host = h.Name
		}
		if host == "" || expected[host] != nil {
			return nil, errors.New("reviewed full preview duplicate/missing host")
		}
		if _, ok := c.Targets[host]; !ok {
			return nil, fmt.Errorf("reviewed full preview unknown host %s", host)
		}
		names := map[string]bool{}
		for _, component := range h.Components {
			if _, ok := release.Components[component.Name]; !ok || component.Name == "" || names[component.Name] {
				return nil, fmt.Errorf("reviewed full preview unknown/duplicate component %s/%s", host, component.Name)
			}
			names[component.Name] = true
		}
		if len(names) != len(release.Components) {
			return nil, fmt.Errorf("reviewed full preview incomplete components %s", host)
		}
		expected[host] = names
	}
	for host := range c.Targets {
		if expected[host] == nil {
			return nil, fmt.Errorf("reviewed full preview omitted %s", host)
		}
	}
	return expected, nil
}
