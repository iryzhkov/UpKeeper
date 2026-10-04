package canonjson_test

import (
	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"testing"
)

func TestDecodeRefusesDuplicatesBeforePolicyMapConversion(t *testing.T) {
	for _, raw := range []string{
		`{"route_policy":{"schema":"route-policy/v1","schema":"invented"}}`,
		`{"route_policy":{"roles":[{"name":"execute","name":"read"}]}}`,
		`{"route_policy":null,"route_policy":{}}`,
		`{} {}`,
		`{} trailing`,
	} {
		if _, err := canonjson.DecodeObject([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, err := canonjson.DecodeObject([]byte(`{"a":[],"b":null,"c":{"a":1}}`)); err != nil {
		t.Fatal(err)
	}
}
