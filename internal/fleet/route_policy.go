package fleet

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// RoutePolicyDestination is a standalone, credential-free, one-author document.
const RoutePolicyDestination = ".config/t3-steward/route-policy.yaml"
const RoutePolicyMaxBytes = 1 << 20

// parseRoutePolicy deliberately delegates the closed schema to Steward. JSON is
// a YAML subset; canonical JSON preserves role/candidate/list order while making
// map order immaterial. We do not maintain a second policy schema in UpKeeper.
func (d *Document) parseRoutePolicy(component map[string]any) error {
	value, present := component["route_policy"]
	if !present {
		return nil
	}
	if _, ok := value.(map[string]any); !ok {
		return failure.Value("route_policy must be a route-policy/v1 object, not null or a file reference")
	}
	payload, err := canonjson.MarshalFile(value)
	if err != nil {
		return err
	}
	if err := CheckRoutePolicy(payload, nil, false); err != nil {
		return err
	}
	d.routePolicy = payload
	return nil
}

// RoutePolicyBytes returns the immutable rendered bytes, or nil for legacy intent.
func (d *Document) RoutePolicyBytes() []byte {
	return bytes.Clone(d.routePolicy)
}

// CheckRoutePolicy validates exactly these bytes in private temporary storage.
// show is the authoritative schema parser; validate additionally checks configured
// authorization, never readiness, quota or advertisements. Runner may isolate
// transport in tests; production always executes the installed Steward CLI.
func CheckRoutePolicy(payload []byte, runner procx.Runner, catalog bool) error {
	if payload == nil {
		return nil
	}
	if len(payload) > RoutePolicyMaxBytes {
		return failure.Value("route_policy exceeds 1 MiB; reduce the policy")
	}
	file, err := os.CreateTemp("", "upkeeper-route-policy-*.yaml")
	if err != nil {
		return err
	}
	path := file.Name()
	defer os.Remove(path)
	if _, err := file.Write(payload); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return CheckRoutePolicyFile(path, runner, catalog)
}

// CheckRoutePolicyFile is also used on staged bytes before the destination rename.
// The CLI reads its normal coordinator config (the agent runner supplies HOME/XDG).
func CheckRoutePolicyFile(path string, runner procx.Runner, catalog bool) error {
	if runner == nil {
		runner = procx.Run
	}
	action := "show"
	if catalog {
		action = "validate"
	}
	result := runner(procx.Command{Argv: []string{"t3-steward", "policy", action, "--file", path, "--json"}, Timeout: 30 * time.Second})
	if result.OK() {
		return nil
	}
	detail := strings.TrimSpace(result.Stderr + "\n" + result.Stdout)
	if result.Err != nil {
		detail += ": " + result.Err.Error()
	}
	if len(detail) > 4000 {
		detail = detail[len(detail)-4000:]
	}
	return failure.Value("route_policy %s refused (exit %d): %s; use t3-steward rc107 or newer for schema parsing; configured sole-wildcard model catalogs require commit ced8e299561920ad1b46059f354316467f10f655 or a release containing that fix (rc107 lacks it); run t3-steward policy validate --file PATH against a coordinator reporting configured model authorizations", action, result.Code, detail)
}

// ValidateRoutePolicy checks catalog authorization at the publication boundary.
func (d *Document) ValidateRoutePolicy() error {
	if err := CheckRoutePolicy(d.routePolicy, nil, true); err != nil {
		return fmt.Errorf("fleet route policy: %w", err)
	}
	return nil
}
