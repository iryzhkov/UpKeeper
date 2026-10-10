package rollout

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestStagedValidatorExactReceiptAndMutationRefusals(t *testing.T) {
	for _, fault := range []string{"positive", "missing", "wrong-digest", "unknown", "trailing", "duplicate", "success42", "mutate-content", "mutate-mode", "same-content-write", "same-content-replace", "unsupported"} {
		t.Run(fault, func(t *testing.T) {
			b, c, s, path := configFixture(t)
			program := "import pathlib,sys,json,hashlib,os; p=pathlib.Path(sys.argv[1]); d=dict(schema_version=1,kind='config-validation',valid=True,file_sha256=hashlib.sha256(p.read_bytes()).hexdigest()); "
			switch fault {
			case "missing":
				program += "del d['file_sha256']; "
			case "wrong-digest":
				program += "d['file_sha256']='0'*64; "
			case "unknown":
				program += "d['extra']=True; "
			case "mutate-content":
				program += "p.write_text('changed'); "
			case "mutate-mode":
				program += "os.chmod(p,0o644); "
			case "same-content-write":
				program += "p.write_bytes(p.read_bytes()); "
			case "same-content-replace":
				program += "q=p.with_suffix('.replace'); q.write_bytes(p.read_bytes()); os.chmod(q,0o600); os.replace(q,p); "
			}
			if fault == "duplicate" {
				program += "print('{\"schema_version\":1,\"schema_version\":1,\"kind\":\"config-validation\",\"valid\":true,\"file_sha256\":\"'+d['file_sha256']+'\"}'); "
			} else {
				program += "print(json.dumps(d)); "
			}
			if fault == "trailing" {
				program += "print('{}'); "
			}
			if fault == "success42" {
				program += "sys.exit(42); "
			}
			c.CoordinatorConfig.ValidateCommand = []string{"python3", "-c", program, "{config}"}
			if fault == "unsupported" {
				c.CoordinatorConfig.ValidateCommand = []string{"/nonexistent/accepted-steward-validator", "config", "validate", "--file", "{config}", "--json"}
			}
			err := b.configPatch(context.Background(), c, s)
			if fault == "positive" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("invalid validator accepted")
			}
			got, readErr := os.ReadFile(path)
			if readErr != nil || digest(got) != c.CoordinatorConfig.BeforeSHA256 {
				t.Fatal("refused staged validation installed live config", readErr)
			}
			calls := b.Runner.(*configShellRunner).calls
			if err := b.configPatch(context.Background(), c, s); err == nil || !strings.Contains(err.Error(), "unfinished") {
				t.Fatal("ambiguous validation retried", err)
			}
			if b.Runner.(*configShellRunner).calls != calls {
				t.Fatal("retry dispatched another validation/install")
			}
		})
	}
}
func TestWalkRejectsEveryMissingOrMalformedRequiredArgvBeforeEffects(t *testing.T) {
	for _, field := range []string{"activity", "haiku", "dropin", "quiet", "validator"} {
		for _, bad := range [][]string{nil, {"", "x"}, {"x", "\x00"}} {
			c := rc122Fixture(t)
			switch field {
			case "activity":
				c.ActivityCommand = bad
			case "haiku":
				c.HaikuCheckCommand = bad
			case "dropin":
				c.DropinTestCommand = bad
			case "quiet":
				c.QuietCommand = bad
			case "validator":
				c.CoordinatorConfig.ValidateCommand = bad
			}
			b := protocolBackend()
			if err := PrepareRollout(context.Background(), c, b, true); err == nil {
				t.Fatal("accepted malformed argv", field, bad)
			}
			if b.checks > 0 || len(b.actions) > 0 {
				t.Fatal("required argv checked after effects", field)
			}
		}
	}
}
