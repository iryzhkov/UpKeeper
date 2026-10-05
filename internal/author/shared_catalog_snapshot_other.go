//go:build !linux

package author

import (
	"context"
	"errors"
)

func acquireCatalogSnapshot(context.Context, CatalogSnapshotOptions, map[string]snapshotSpec, map[string]snapshotSpec) (*CatalogSnapshot, error) {
	return nil, errors.New("catalog snapshot held acquisition unsupported on this platform")
}
