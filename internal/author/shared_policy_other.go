//go:build !linux

package author

import "errors"

func acquireSharedSnapshotDirectory(string) (func() error, func(), error) {
	return nil, nil, errors.New("shared policy requires Linux secure acquisition")
}
func acquireSharedRepository(string) (func() error, func(), error) {
	return nil, nil, errors.New("shared policy requires Linux secure acquisition")
}
func acquireSharedFile(string, int64, func()) ([]byte, func() error, func(), error) {
	return nil, nil, nil, errors.New("shared policy requires Linux secure acquisition")
}
func acquireSharedManifest(string) ([]byte, func() error, func(), error) {
	return nil, nil, nil, errors.New("shared policy requires Linux secure acquisition")
}
