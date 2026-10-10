package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/rollout"
)

// Diagnostic argv deliberately ends in HOST EPOCH for the configured adapter
// consumer. It accepts no mutating flag or arbitrary producer command.
func parseRolloutDiagnostic(p *arguments, argv []string) error {
	if len(argv) < 2 {
		return usagef("rollout %s requires --config FILE --json HOST EPOCH", p.rolloutAction)
	}
	p.rolloutHost, p.rolloutEpoch = argv[len(argv)-2], argv[len(argv)-1]
	epoch, err := strconv.ParseInt(p.rolloutEpoch, 10, 64)
	if err != nil || epoch <= 0 || p.rolloutHost == "" || p.rolloutHost[0] == '-' {
		return usagef("diagnostic requires HOST and positive EPOCH")
	}
	for i := 0; i < len(argv)-2; i++ {
		switch argv[i] {
		case "--config":
			if p.rolloutConfig != "" || i+1 >= len(argv)-2 {
				return usagef("diagnostic requires one --config FILE")
			}
			i++
			p.rolloutConfig = argv[i]
		case "--json":
			if p.asJSON {
				return usagef("duplicate --json")
			}
			p.asJSON = true
		default:
			return unrecognized(argv[i])
		}
	}
	if p.rolloutConfig == "" || !p.asJSON {
		return usagef("diagnostic requires --config FILE --json")
	}
	return nil
}
func runRolloutDiagnostic(p *arguments, streams Streams) (int, error) {
	c, err := rollout.LoadConfig(p.rolloutConfig)
	if err != nil {
		return exitcode.Preflight, err
	}
	epoch, _ := strconv.ParseInt(p.rolloutEpoch, 10, 64)
	v, err := rollout.NewProductionBackend().Diagnostic(context.Background(), c, p.rolloutAction, p.rolloutHost, epoch)
	if err != nil {
		return exitcode.Preflight, fmt.Errorf("rollout diagnostic refused: %w", err)
	}
	return exitcode.Success, json.NewEncoder(streams.Out).Encode(v)
}
