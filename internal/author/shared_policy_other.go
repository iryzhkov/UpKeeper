//go:build !linux

package author

import "errors"

func acquireSharedRepository(string) (func() error, func(), error) {
	return nil, nil, errors.New("shared policy requires Linux secure acquisition")
}
func acquireSharedManifest(string) ([]byte, func() error, func(), error) {
	return nil, nil, nil, errors.New("shared policy requires Linux secure acquisition")
}
