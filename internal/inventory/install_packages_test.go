package inventory_test

import (
	"strings"
	"testing"
)

func installPackagesInventory(value string) string {
	return `{"all":{"vars":{"fleet_inventory_schema_version":1},"hosts":{"agent-a":{
		"ansible_host":"192.168.130.2","ansible_user":"igor",
		"canonical_hostname":"agent-a","architecture":"x86_64",
		"os_id":"debian","os_version":"13",
		"machine_id_sha256":"` + strings.Repeat("a", 64) + `",
		"ssh_host_ed25519_sha256":"SHA256:fIi2ZWW/6A/zIm+RE3DbBte2nd7dD7bj2m2HdJjT5NA",
		"stable_address_verified":true` + value + `}}}}`
}

// Installing OS packages is a host's explicit opt-in, off unless it is set.
func TestInstallingOSPackagesIsAnExplicitBooleanOptIn(t *testing.T) {
	hosts, err := loadInventoryText(t, installPackagesInventory(""))
	if err != nil || hosts["agent-a"].InstallOSPackages {
		t.Fatalf("default = %v (%v)", hosts["agent-a"].InstallOSPackages, err)
	}
	hosts, err = loadInventoryText(t, installPackagesInventory(`,"install_os_packages":true`))
	if err != nil || !hosts["agent-a"].InstallOSPackages {
		t.Fatalf("opted in = %v (%v)", hosts["agent-a"].InstallOSPackages, err)
	}
	if _, err = loadInventoryText(t, installPackagesInventory(`,"install_os_packages":"yes"`)); err == nil ||
		!strings.Contains(err.Error(), "install_os_packages must be boolean") {
		t.Fatalf("a non-boolean opt-in produced %v", err)
	}
}
