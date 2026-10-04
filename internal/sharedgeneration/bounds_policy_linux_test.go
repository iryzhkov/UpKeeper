//go:build linux

package sharedgeneration

import (
	"bytes"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

func TestV2InspectionIdentityBounds(t *testing.T) {
	for _, kind := range []string{"policy-over256K", "policy-exact1M", "policy-over1M", "instruction-over256K-0600", "instruction-exact16M", "instruction-over16M", "mixed-exact64M", "mixed-over64M"} {
		t.Run(kind, func(t *testing.T) {
			s := mustOpen(t)
			mutate := func(r *Receipt, data map[string][]byte) {
				instructions := []string{}
				for _, f := range r.Files {
					if f.Kind == "instruction" {
						instructions = append(instructions, f.Path)
					}
				}
				switch {
				case strings.HasPrefix(kind, "policy-"):
					size := (256 << 10) + 1
					if kind == "policy-exact1M" {
						size = policyLimit
					}
					if kind == "policy-over1M" {
						size = policyLimit + 1
					}
					base, err := canonjson.MarshalFile(map[string]any{"x": ""})
					if err != nil {
						t.Fatal(err)
					}
					raw, err := canonjson.MarshalFile(map[string]any{"x": strings.Repeat("x", size-len(base))})
					if err != nil || len(raw) != size {
						t.Fatal("policy size", err, len(raw))
					}
					data["inputs/route-policy.json"] = raw
				case strings.HasPrefix(kind, "instruction-"):
					size := (256 << 10) + 1
					if kind == "instruction-exact16M" {
						size = fileLimit
					}
					if kind == "instruction-over16M" {
						size = fileLimit + 1
					}
					data[instructions[0]] = bytes.Repeat([]byte("x"), size)
					for n := range r.Files {
						if r.Files[n].Path == instructions[0] {
							r.Files[n].Mode = 0600
						}
					}
				default:
					for j := 0; j < 4; j++ {
						data[instructions[j]] = bytes.Repeat([]byte("x"), fileLimit)
					}
					var total int
					for _, raw := range data {
						total += len(raw)
					}
					shrink := total - aggregateLimit
					data[instructions[3]] = data[instructions[3]][:fileLimit-shrink]
					if kind == "mixed-over64M" {
						data[instructions[3]] = append(data[instructions[3]], 'x')
					}
					for n := range r.Files {
						if r.Files[n].Path == instructions[0] {
							r.Files[n].Mode = 0600
						}
					}
				}
			}
			i := installSyntheticV2Payload(t, s, mutate)
			// selectID and Inspect both execute the actual generation boundary;
			// these fixtures deliberately bypass preparation to exercise offline
			// independently stored payloads.
			refused := strings.Contains(kind, "over1M") || strings.Contains(kind, "over16M") || strings.Contains(kind, "over64M")
			staged := mustOpen(t)
			transaction, err := staged.begin()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := transaction.validate(); err != nil {
				transaction.close()
				t.Fatal(err)
			}
			receipt, payload := syntheticV2Payload(t, mutate)
			_, stageError := transaction.stageGeneration(receipt, payload, func() error { return nil })
			transaction.close()
			if refused {
				if stageError == nil {
					t.Fatal("staging overflow accepted")
				}
			} else {
				if stageError != nil {
					t.Fatal("staging boundary refused", stageError)
				}
				if _, err := staged.Inspect(); err != nil {
					t.Fatal("staged inspection", err)
				}
			}
			t.Logf("actual internal staging boundary: refused=%v", refused)
			_, err = s.SelectPrior(i.GenerationID)
			if refused {
				if err == nil {
					t.Fatal("overflow accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Inspect(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
