package cli

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/rollout"
)

func parseRollout(p *arguments, argv []string) error {
	if len(argv) == 0 {
		return usagef("rollout requires check, plan or run")
	}
	p.rolloutAction = argv[0]
	if p.rolloutAction != "check" && p.rolloutAction != "plan" && p.rolloutAction != "run" {
		return usagef("unknown rollout phase: %s", argv[0])
	}
	for i := 1; i < len(argv); i++ {
		switch argv[i] {
		case "--config":
			if p.rolloutConfig != "" {
				return usagef("duplicate --config")
			}
			value, next, err := takeValue(argv, i, "--config")
			if err != nil {
				return err
			}
			p.rolloutConfig = value
			i = next
		case "--yes":
			p.rolloutYes = true
		case "--json":
			p.asJSON = true
		default:
			return unrecognized(argv[i])
		}
	}
	if p.rolloutConfig == "" {
		return usagef("rollout requires --config FILE")
	}
	if p.rolloutYes && p.rolloutAction != "run" {
		return usagef("--yes applies only to rollout run")
	}
	return nil
}
func rolloutHelp(program string) string {
	return "usage: " + program + " rollout check|plan|run --config FILE [--yes] [--json]\n\n" +
		"check and plan are read-only. run prints every step and rollback, requires\n" +
		"--yes, and resumes the configuration-bound journal. Timers remain fenced.\n" +
		"See docs/rollout.md for the configuration and external review gate.\n"
}
func runRollout(p *arguments, streams Streams) (int, error) {
	c, err := rollout.LoadConfig(p.rolloutConfig)
	if err != nil {
		return exitcode.Preflight, err
	}
	steps, err := rollout.Plan(c)
	if err != nil {
		return exitcode.Preflight, err
	}
	if p.rolloutAction == "plan" {
		if p.asJSON {
			return exitcode.Success, printJSON(streams.Out, map[string]any{"command": "rollout", "phase": "plan", "steps": steps})
		}
		for _, s := range steps {
			fmt.Fprintf(streams.Out, "%s: %s\n  rollback: %s\n", s.Action, s.Description, s.Rollback)
		}
		return exitcode.Success, nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	backend := rollout.NewProductionBackend()
	if p.rolloutAction == "check" {
		err = backend.Check(ctx, c)
		if err != nil {
			return exitcode.Preflight, err
		}
		if p.asJSON {
			return exitcode.Success, printJSON(streams.Out, map[string]any{"command": "rollout", "phase": "check", "result": "pass"})
		}
		fmt.Fprintln(streams.Out, "rollout check: preconditions pass")
		return exitcode.Success, nil
	}
	err = rollout.Run(ctx, c, backend, p.rolloutYes, streams.Out)
	if err != nil {
		if !p.rolloutYes {
			fmt.Fprintln(streams.Err, err)
			return exitcode.Usage, nil
		}
		return exitcode.Preflight, err
	}
	if p.asJSON {
		return exitcode.Success, printJSON(streams.Out, map[string]any{"command": "rollout", "phase": "run", "result": "complete", "state": c.StatePath, "timers": "fenced"})
	}
	return exitcode.Success, nil
}
