// Scratch-only candidate inspection; never authors or activates a release.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/iryzhkov/UpKeeper/internal/author"
	"os"
)

func main() {
	repository := flag.String("repository", "", "local UpKeeper Git repository (required)")
	commit := flag.String("commit", "", "exact full input source commit (required)")
	flag.Parse()
	if *repository == "" || *commit == "" || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	prepared, err := author.PrepareShared(*repository, *commit)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer prepared.Close()
	if err := json.NewEncoder(os.Stdout).Encode(prepared); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
