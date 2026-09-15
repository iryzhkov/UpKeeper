package cli

import (
	"time"

	"github.com/iryzhkov/UpKeeper/internal/converge"
	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/inventory"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
	"github.com/iryzhkov/UpKeeper/internal/safety"
)

func runReleaseStatus(parsed *arguments, streams Streams) (int, error) {
	root, err := ConfigRoot()
	if err != nil {
		return exitcode.Preflight, err
	}
	config, err := inventory.Load(root)
	if err != nil {
		return exitcode.Preflight, err
	}
	value, err := release.Load(root)
	if err != nil {
		return exitcode.Preflight, err
	}
	hosts, err := safety.ParseHostSelector(parsed.limit, config.KnownHosts())
	if err != nil {
		return exitcode.Preflight, err
	}
	if len(hosts) == 0 {
		hosts = config.HostNames()
	}
	doc, _, err := converge.Fleet(config, value, converge.Options{
		Hosts: hosts, DryRun: true, ObserveOnly: true, ManifestCommit: manifestCommit(root), Timeout: time.Duration(parsed.probeTimeout) * time.Second, Process: procx.Run,
	})
	if err != nil {
		return exitcode.Preflight, err
	}
	code := 0
	result := "converged"
	for _, raw := range doc["hosts"].([]any) {
		host := raw.(map[string]any)
		host["name"] = host["host"]
		if host["observe_only"] != true {
			host["result"] = "unavailable"
			host["error"] = "release observation unavailable; target needs a compatible UpKeeper binary"
			code = 20
			result = "partial"
			continue
		}
		switch host["result"] {
		case "failed":
			code = 20
			result = "partial"
		case "drift":
			if code == 0 {
				code = 10
				result = "drift"
			}
		}
	}
	doc["command"] = "status"
	doc["result"] = result
	doc["exit_code"] = code
	if parsed.asJSON {
		err = printJSON(streams.Out, doc)
	} else {
		printStatus(streams.Out, doc)
	}
	return code, err
}
