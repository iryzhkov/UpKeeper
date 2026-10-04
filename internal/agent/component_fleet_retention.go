package agent

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
)

// Decoded retention contains only validated data. Guard authority is never
// accepted from JSON: callers pass expected live bytes separately.
type fleetRetainedDocument struct {
	destination string
	existed     bool
	payload     []byte
}

func fleetValidateRetainedEntry(entry map[string]any) (fleetRetainedDocument, error) {
	var result fleetRetainedDocument
	for key := range entry {
		if key != "destination" && key != "existed" && key != "content" && key != "encoding" {
			return result, errors.New("unsupported retained fleet document field")
		}
	}
	destination, ok := entry["destination"].(string)
	if !ok || !fleetOwnedDestinations[destination] {
		return result, errors.New("retained fleet record names an unexpected destination")
	}
	existed, ok := entry["existed"].(bool)
	if !ok {
		return result, errors.New("retained fleet existence must be a boolean")
	}
	content, present := entry["content"]
	if !present {
		return result, errors.New("retained fleet record has no content field")
	}
	result.destination, result.existed = destination, existed
	if !existed && content == nil {
		if _, encoded := entry["encoding"]; encoded {
			return result, errors.New("absent retained content cannot have an encoding")
		}
		return result, nil
	}
	payload, err := fleetRetainedBytes(entry)
	if err != nil {
		return result, err
	}
	if !existed {
		// Historical absent entries used an empty string. Canonical encoded empty
		// content is also unambiguous, but nonempty bytes contradict absence.
		if len(payload) != 0 {
			return result, errors.New("absent retained document has content")
		}
		return result, nil
	}
	if len(payload) > fleetCoordinatorMaxBytes {
		return result, errors.New("retained fleet content exceeds bound")
	}
	result.payload = payload
	return result, nil
}

func fleetDecodeRetainedRecord(record []byte) ([]fleetRetainedDocument, error) {
	prior, err := canonjson.DecodeObject(record)
	if err != nil {
		return nil, err
	}
	if !workerSameFields(prior, []string{"documents"}) {
		return nil, errors.New("unsupported retained fleet record fields")
	}
	documents, ok := prior["documents"].([]any)
	if !ok || len(documents) == 0 || len(documents) > len(fleetOwnedDestinations) {
		return nil, errors.New("retained fleet record names invalid documents")
	}
	decoded := make([]fleetRetainedDocument, 0, len(documents))
	seen := make(map[string]bool)
	for _, item := range documents {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("retained fleet record is malformed")
		}
		document, err := fleetValidateRetainedEntry(entry)
		if err != nil {
			return nil, err
		}
		if seen[document.destination] {
			return nil, errors.New("duplicate retained fleet destination")
		}
		seen[document.destination] = true
		decoded = append(decoded, document)
	}
	return decoded, nil
}

func fleetReadRetainedRecord(home, runID string) ([]fleetRetainedDocument, error) {
	record, err := os.ReadFile(filepath.Join(home, fleet.StateDirectory, runID, "prior.json"))
	if err != nil {
		return nil, err
	}
	return fleetDecodeRetainedRecord(record)
}

// Validate a retry's original record before any ownership receipt is prepared.
func fleetValidateReusedRetention(c *Context, runID string) error {
	_, err := os.Lstat(c.Path(fleet.StateDirectory, runID, "prior.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = fleetReadRetainedRecord(c.Home, runID)
	return err
}
