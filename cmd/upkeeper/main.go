// Command upkeeper is the whole of UpKeeper: a single self-contained binary
// that observes the fleet, captures a release, publishes it and converges hosts
// onto it.
//
// The binary is the product boundary. It needs Git, OpenSSH, systemd and
// ordinary operating system facilities on a host, and it diagnoses their
// absence before it mutates anything, but it needs no language runtime, no
// package manager and no configuration management system of its own.
package main

import (
	"os"

	"github.com/iryzhkov/UpKeeper/internal/cli"
)

func main() {
	program := "upkeeper"
	if len(os.Args) > 0 {
		program = os.Args[0]
	}
	os.Exit(cli.MainSpooled(program, os.Args[1:], cli.Streams{
		In: os.Stdin, Out: os.Stdout, Err: os.Stderr,
	}))
}
