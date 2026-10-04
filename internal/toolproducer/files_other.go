//go:build !linux

package toolproducer

type physical struct{}

func newPhysical() (*physical, error) { return nil, refuse("platform-unsupported") }
func (*physical) close()              {}
func (*physical) root(string) error   { return refuse("platform-unsupported") }
func (*physical) file(string, int64) ([]byte, int, error) {
	return nil, 0, refuse("platform-unsupported")
}
func (*physical) check() error { return refuse("platform-unsupported") }
