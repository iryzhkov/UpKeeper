//go:build !linux

package sharedgeneration

type Store struct{}

func Open(string) (*Store, error)                       { return nil, ErrUnsupported }
func (*Store) Stage(string, string) (Inspection, error) { return Inspection{}, ErrUnsupported }
func (*Store) Inspect() (Inspection, error)             { return Inspection{}, ErrUnsupported }
func (*Store) SelectPrior(string) (Inspection, error)   { return Inspection{}, ErrUnsupported }
func (*Store) Recover() (Inspection, error)             { return Inspection{}, ErrUnsupported }
