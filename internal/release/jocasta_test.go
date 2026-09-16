package release

import (
	"strings"
	"testing"
)

func TestJocastaClosedContract(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{"kind": "github-release", "repository": "iryzhkov/jocasta", "version": "0.1.0", "commit": strings.Repeat("a", 40), "url": "https://jocasta.example.test", "assets": map[string]any{"linux_amd64": strings.Repeat("a", 64), "linux_arm64": strings.Repeat("b", 64)}, "hosts": map[string]any{"laptop": "secretref:jocasta/laptop"}}
	}
	if _, err := DecodeJocasta(base()); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(map[string]any){
		func(m map[string]any) { m["token"] = "secret" },
		func(m map[string]any) { m["url"] = "http://localhost" },
		func(m map[string]any) { m["url"] = "https://user:secret@example.test" },
		func(m map[string]any) { m["hosts"] = map[string]any{"laptop": "secretref:jocasta/normandy"} },
		func(m map[string]any) { m["version"] = "0.1.0-dirty" },
		func(m map[string]any) { m["assets"] = map[string]any{"linux_amd64": strings.Repeat("a", 64)} },
	} {
		m := base()
		change(m)
		if _, err := DecodeJocasta(m); err == nil {
			t.Fatal("accepted invalid contract")
		}
	}
}
