//go:build !linux

package toolcatalog

func Inspect(o Options) (map[string]any, error) {
	// Refuse before opening any input.
	e := refusal("unsupported-platform")
	return Failure(e, ""), e
}
