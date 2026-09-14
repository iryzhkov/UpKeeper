// Package runid mints the opaque, sortable identifiers that tie a command to
// its durable records.
package runid

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/pytime"
	"github.com/iryzhkov/UpKeeper/internal/safety"
)

// New returns a run identifier of the form 20260914T090700Z-0a1b2c3d.
func New() (string, error) {
	return At(time.Now(), "")
}

// At returns a run identifier for a fixed instant, and with fixed entropy when
// entropy is not empty. Tests use it to get reproducible records.
func At(now time.Time, entropy string) (string, error) {
	suffix := entropy
	if suffix == "" {
		var random [4]byte
		if _, err := cryptorand.Read(random[:]); err != nil {
			return "", err
		}
		suffix = hex.EncodeToString(random[:])
	}
	return safety.ValidateRunID(pytime.RunIDStamp(now) + "-" + suffix)
}
