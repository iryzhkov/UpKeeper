package inventory_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/inventory"
)

// repositoryRoot is the checkout this test runs from, which carries the real
// inventory and desired state the fleet uses.
func repositoryRoot() string { return filepath.Join("..", "..") }

func TestTheFleetsOwnSourcesOfTruthLoad(t *testing.T) {
	config, err := inventory.Load(repositoryRoot())
	if err != nil {
		t.Fatalf("the repository's own inventory did not load: %v", err)
	}
	for _, name := range []string{"laptop", "omarchy-pc", "normandy", "homelab"} {
		if _, present := config.Hosts[name]; !present {
			t.Errorf("host %s is missing", name)
		}
	}
	laptop := config.Hosts["laptop"]
	if !laptop.SelfManaged {
		t.Error("the laptop must stay self-managed; a controller may not reach into it")
	}
	if laptop.SetupCheckout == nil || *laptop.SetupCheckout != "Work/omarchy-setup" {
		t.Errorf("the laptop's setup checkout override was lost: %v", laptop.SetupCheckout)
	}
	if config.Hosts["omarchy-pc"].SSHAlias == nil {
		t.Error("the ssh alias was lost")
	}
	if len(config.Components()) == 0 {
		t.Error("the desired component set is empty")
	}
	// The fleet grows, so this checks the order rather than a host count: the
	// names are every inventory host, sorted, and the same on every call.
	names := config.HostNames()
	if len(names) != len(config.Hosts) || !slices.IsSorted(names) {
		t.Errorf("host names are not every host in sorted order: %v", names)
	}
	if again := config.HostNames(); !slices.Equal(names, again) {
		t.Errorf("host order is not deterministic: %v then %v", names, again)
	}
}

func TestMalformedInventoriesAreRefused(t *testing.T) {
	valid := `{"all":{"vars":{"fleet_inventory_schema_version":1},"hosts":{"normandy":{
		"ansible_host":"192.168.70.234","ansible_user":"igor",
		"canonical_hostname":"omarchy-normandy","architecture":"x86_64",
		"os_id":"omarchy","os_version":"4.0.1",
		"machine_id_sha256":"` + strings.Repeat("a", 64) + `",
		"ssh_host_ed25519_sha256":"SHA256:KtRGckedXhKRdOs7roVyrtfX1SHSObd2c8+jtjc/nNE",
		"stable_address_verified":true}}}}`
	if _, err := loadInventoryText(t, valid); err != nil {
		t.Fatalf("the valid fixture was refused: %v", err)
	}

	cases := []struct {
		name     string
		document string
		expected string
	}{
		{
			"a future schema version",
			strings.Replace(valid, `"fleet_inventory_schema_version":1`,
				`"fleet_inventory_schema_version":2`, 1),
			"unsupported inventory schema_version",
		},
		{
			"no hosts at all",
			`{"all":{"vars":{"fleet_inventory_schema_version":1},"hosts":{}}}`,
			"inventory all.hosts must be a non-empty object",
		},
		{
			"a truncated machine identity",
			strings.Replace(valid, strings.Repeat("a", 64), "abc", 1),
			"normandy.machine_id_sha256 is invalid",
		},
		{
			"an unusable host key fingerprint",
			strings.Replace(valid, "SHA256:KtRGckedXhKRdOs7roVyrtfX1SHSObd2c8+jtjc/nNE",
				"MD5:whatever", 1),
			"normandy.ssh_host_ed25519_sha256 is invalid",
		},
		{
			"a login name that could become a shell fragment",
			strings.Replace(valid, `"ansible_user":"igor"`, `"ansible_user":"igor;rm"`, 1),
			"normandy.ansible_user is invalid",
		},
		{
			"an unverified stable address flag that is not a boolean",
			strings.Replace(valid, `"stable_address_verified":true`,
				`"stable_address_verified":"yes"`, 1),
			"normandy.stable_address_verified must be boolean",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := loadInventoryText(t, testCase.document)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if err.Error() != testCase.expected {
				t.Errorf("produced %q, expected %q", err.Error(), testCase.expected)
			}
		})
	}
}

func TestManifestRefusals(t *testing.T) {
	valid := `{"schema_version":1,"deployment_enabled":false,"components":{"huyang":{
		"source":{"canonical":"https://github.com/iryzhkov/huyang.git","allowed_mirrors":[]},
		"pin":null,"paths":{"install_root":".local/lib/dev-fleet/huyang"},
		"architectures":["x86_64"],"dependencies":[],"activation_order":10}}}`
	if _, err := loadManifestText(t, valid); err != nil {
		t.Fatalf("the valid fixture was refused: %v", err)
	}
	cases := []struct {
		name     string
		document string
		expected string
	}{
		{
			"an unpinned component while deployment is enabled",
			strings.Replace(valid, `"deployment_enabled":false`,
				`"deployment_enabled":true`, 1),
			"huyang has no immutable pin while deployment is enabled",
		},
		{
			"a source the fleet may not fetch from",
			strings.Replace(valid, "https://github.com/iryzhkov/huyang.git",
				"https://elsewhere.example/huyang.git", 1),
			"source URL is not allowed: 'https://elsewhere.example/huyang.git'",
		},
		{
			"a path that escapes the allowed home roots",
			strings.Replace(valid, ".local/lib/dev-fleet/huyang", ".ssh/authorized_keys", 1),
			"path must stay below .config, .local: '.ssh/authorized_keys'",
		},
		{
			"an unsupported architecture",
			strings.Replace(valid, `["x86_64"]`, `["riscv64"]`, 1),
			"huyang.architectures contains an unsupported value",
		},
		{
			"a dependency on a component that does not exist",
			strings.Replace(valid, `"dependencies":[]`, `"dependencies":["nope"]`, 1),
			"huyang.dependencies contains an unknown component",
		},
		{
			"a negative activation order",
			strings.Replace(valid, `"activation_order":10`, `"activation_order":-1`, 1),
			"huyang.activation_order must be a non-negative integer",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := loadManifestText(t, testCase.document)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if err.Error() != testCase.expected {
				t.Errorf("produced %q, expected %q", err.Error(), testCase.expected)
			}
		})
	}
}

func TestDependencyCycleIsRefused(t *testing.T) {
	document := `{"schema_version":1,"deployment_enabled":false,"components":{
		"alpha":{"source":{"canonical":"https://github.com/iryzhkov/a.git","allowed_mirrors":[]},
			"pin":null,"paths":{"p":".local/a"},"architectures":["x86_64"],
			"dependencies":["beta"],"activation_order":1},
		"beta":{"source":{"canonical":"https://github.com/iryzhkov/b.git","allowed_mirrors":[]},
			"pin":null,"paths":{"p":".local/b"},"architectures":["x86_64"],
			"dependencies":["alpha"],"activation_order":2}}}`
	_, err := loadManifestText(t, document)
	if err == nil || err.Error() != "component dependency graph contains a cycle" {
		t.Fatalf("produced %v", err)
	}
}

func loadInventoryText(t *testing.T, document string) (map[string]inventory.HostSpec, error) {
	t.Helper()
	return inventory.LoadInventory(writeTemporary(t, "hosts.yml", document))
}

func loadManifestText(t *testing.T, document string) (map[string]any, error) {
	t.Helper()
	return inventory.LoadManifest(writeTemporary(t, "components.yml", document))
}

func writeTemporary(t *testing.T, name, document string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}
