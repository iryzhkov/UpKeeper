package agent

import (
	"bytes"
	"encoding/json"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

// decodeStruct fills a typed value from a decoded generic document. Unknown
// fields are ignored deliberately: a newer controller may send a field this
// agent does not know, and refusing the whole payload would make a mixed-
// version fleet undeployable.
func decodeStruct(document map[string]any, target any) error {
	encoded, err := canonjson.Compact(document)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	return decoder.Decode(target)
}

// DecodeInto is decodeStruct for component adapters that want a typed view of
// their own manifest entry.
func DecodeInto(document map[string]any, target any) error {
	return decodeStruct(document, target)
}
