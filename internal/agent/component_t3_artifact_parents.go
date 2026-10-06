package agent

import (
	"errors"
	"path/filepath"
)

func (tx *t3Transaction) parentsUnchanged() error {
	for _, p := range []string{tx.prefix, filepath.Dir(tx.packagePath), filepath.Dir(tx.link), filepath.Dir(tx.state)} {
		real, e := filepath.EvalSymlinks(p)
		if e != nil || real != p {
			return errors.New("T3 package, launcher or state parent ownership changed")
		}
	}
	return nil
}
