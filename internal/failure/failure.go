// Package failure classifies the errors the CLI is allowed to turn into a
// preflight exit.
//
// The Python CLI caught exactly ValidationError, ValueError, CaptureError,
// PushError and PullError and returned exit code 30; anything else escaped and
// the process died with exit code 1. That split is a real part of the contract,
// because an unexpected failure must not be mistaken for a clean refusal, so
// the Go port keeps it by marking the expected errors explicitly.
package failure

import (
	"errors"
	"fmt"
)

// Kind names the Python exception an error corresponds to.
type Kind string

const (
	KindValidation Kind = "validation"
	KindValue      Kind = "value"
	KindCapture    Kind = "capture"
	KindPush       Kind = "push"
	KindPull       Kind = "pull"
	KindLock       Kind = "lock-contention"
	KindProbe      Kind = "probe"
)

// Error is an expected refusal: the CLI reports it and exits 30.
type Error struct {
	Kind    Kind
	Message string
	Cause   error
}

func (e *Error) Error() string { return e.Message }

func (e *Error) Unwrap() error { return e.Cause }

// Expected marks this error as one the CLI may report rather than crash on.
func (e *Error) Expected() bool { return true }

func newf(kind Kind, format string, arguments ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, arguments...)}
}

// Validation reports an input that failed a security boundary.
func Validation(format string, arguments ...any) *Error {
	return newf(KindValidation, format, arguments...)
}

// Value reports a value that is structurally wrong but not a boundary breach.
func Value(format string, arguments ...any) *Error {
	return newf(KindValue, format, arguments...)
}

// Capture reports controller state that is not publishable as a release.
func Capture(format string, arguments ...any) *Error {
	return newf(KindCapture, format, arguments...)
}

// Push reports a release that could not be committed or pushed.
func Push(format string, arguments ...any) *Error {
	return newf(KindPush, format, arguments...)
}

// Pull reports a convergence that could not start.
func Pull(format string, arguments ...any) *Error {
	return newf(KindPull, format, arguments...)
}

// LockContention reports a target that already holds a deployment lock.
func LockContention(format string, arguments ...any) *Error {
	return newf(KindLock, format, arguments...)
}

// Probe reports a target that could not return a valid probe result.
func Probe(format string, arguments ...any) *Error {
	return newf(KindProbe, format, arguments...)
}

// Wrap attaches a cause while keeping the reported message.
func Wrap(err error, kind Kind, format string, arguments ...any) *Error {
	wrapped := newf(kind, format, arguments...)
	wrapped.Cause = err
	return wrapped
}

// IsExpected reports whether the CLI may render err as a preflight refusal.
func IsExpected(err error) bool {
	var expected interface{ Expected() bool }
	return errors.As(err, &expected) && expected.Expected()
}

// Is reports whether err, or anything it wraps, carries the given kind.
func Is(err error, kind Kind) bool {
	var typed *Error
	for errors.As(err, &typed) {
		if typed.Kind == kind {
			return true
		}
		if typed.Cause == nil {
			return false
		}
		err = typed.Cause
	}
	return false
}
