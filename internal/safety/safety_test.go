package safety_test

import (
	"reflect"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/safety"
)

// The refusal messages are part of the operator-facing contract, so they are
// compared literally against what the Python implementation produced.
func TestRefusalMessagesMatchPython(t *testing.T) {
	knownHost := map[string]bool{"x": true}
	cases := []struct {
		name     string
		call     func() error
		expected string
	}{
		{
			"component name with an upper-case letter",
			func() error { _, err := safety.ValidateComponentName("Bad", nil); return err },
			"invalid component name: 'Bad'",
		},
		{
			"host that is not in the inventory",
			func() error { _, err := safety.ValidateHostName("omarchy-pc", knownHost); return err },
			"unknown host: omarchy-pc",
		},
		{
			"selector with an empty entry",
			func() error { return selectorError("a,,b", map[string]bool{"a": true, "b": true}) },
			"host selector contains an empty entry",
		},
		{
			"selector naming a host twice",
			func() error { return selectorError("a,a", map[string]bool{"a": true}) },
			"host selector contains duplicates",
		},
		{
			"padded selector",
			func() error { return selectorError(" a", map[string]bool{"a": true}) },
			"host selector may not be empty or padded",
		},
		{
			"short commit",
			func() error { _, err := safety.ValidateGitCommit("abc"); return err },
			"git refs must be full lowercase 40-character commits",
		},
		{
			"path traversal",
			func() error { _, err := safety.ValidateHomeRelativePath(".local/../x", nil); return err },
			"path traversal is forbidden: '.local/../x'",
		},
		{
			"path outside the allowed roots",
			func() error { _, err := safety.ValidateHomeRelativePath("Work/x", nil); return err },
			"path must stay below .config, .local: 'Work/x'",
		},
		{
			"path with a trailing separator",
			func() error { _, err := safety.ValidateHomeRelativePath(".local/bin/", nil); return err },
			"path must be a normalized home-relative path: '.local/bin/'",
		},
		{
			"absolute path",
			func() error { _, err := safety.ValidateHomeRelativePath("/etc/passwd", nil); return err },
			"path must be a normalized home-relative path: '/etc/passwd'",
		},
		{
			"source on a host the fleet does not trust",
			func() error { _, err := safety.ValidateSourceURL("https://evil.com/x.git"); return err },
			"source URL is not allowed: 'https://evil.com/x.git'",
		},
		{
			"address with an underscore",
			func() error { _, err := safety.ValidateAddress("bad_host"); return err },
			"invalid host address: 'bad_host'",
		},
		{
			"run identifier with a short suffix",
			func() error { _, err := safety.ValidateRunID("20260914T090700Z-zz"); return err },
			"invalid run ID: '20260914T090700Z-zz'",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.call()
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if err.Error() != testCase.expected {
				t.Errorf("produced %q, expected %q", err.Error(), testCase.expected)
			}
		})
	}
}

func TestAcceptedValues(t *testing.T) {
	if _, err := safety.ValidateSourceURL("git@github.com:iryzhkov/t3-steward.git"); err != nil {
		t.Errorf("scp-style GitHub source was refused: %v", err)
	}
	if _, err := safety.ValidateSourceURL("https://git.ryzhkov.dev/igor/x.git"); err != nil {
		t.Errorf("mirror source was refused: %v", err)
	}
	if _, err := safety.ValidateAddress("192.168.90.212"); err != nil {
		t.Errorf("literal address was refused: %v", err)
	}
	if _, err := safety.ValidateRunID("20260914T090700Z-0a1b2c3d"); err != nil {
		t.Errorf("well-formed run identifier was refused: %v", err)
	}
}

func TestSelectorWithoutAValueMeansEveryHost(t *testing.T) {
	selected, err := safety.ParseHostSelector(nil, map[string]bool{"b": true, "a": true})
	if err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
	if !reflect.DeepEqual(selected, []string{"a", "b"}) {
		t.Errorf("produced %v, expected every host in sorted order", selected)
	}
}

func TestUnixUserNames(t *testing.T) {
	for _, accepted := range []string{"igor", "iryzhkov", "a-b_c9"} {
		if !safety.ValidateUnixUser(accepted) {
			t.Errorf("%q should be accepted", accepted)
		}
	}
	for _, refused := range []string{"", "has space", "root;rm", "________"} {
		if safety.ValidateUnixUser(refused) {
			t.Errorf("%q should be refused", refused)
		}
	}
}

func selectorError(raw string, known map[string]bool) error {
	_, err := safety.ParseHostSelector(&raw, known)
	return err
}
