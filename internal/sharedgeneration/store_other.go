//go:build !linux

package sharedgeneration

import "github.com/iryzhkov/UpKeeper/internal/author"

type Store struct{}

func (*Store) StageWithPolicy(author.SharedPolicyOptions) (Inspection, error) {
	return Inspection{}, ErrUnsupported
}

func Open(string) (*Store, error)                       { return nil, ErrUnsupported }
func (*Store) Stage(string, string) (Inspection, error) { return Inspection{}, ErrUnsupported }
func (*Store) Inspect() (Inspection, error)             { return Inspection{}, ErrUnsupported }
func (*Store) SelectPrior(string) (Inspection, error)   { return Inspection{}, ErrUnsupported }
func (*Store) Recover() (Inspection, error)             { return Inspection{}, ErrUnsupported }
