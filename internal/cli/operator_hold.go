package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/inventory"
	"github.com/iryzhkov/UpKeeper/internal/operatorhold"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/pytime"
	"github.com/iryzhkov/UpKeeper/internal/redact"
	"github.com/iryzhkov/UpKeeper/internal/safety"
	"github.com/iryzhkov/UpKeeper/internal/transport"
)

// holdClock is the time holds are set and judged at; a variable so that a
// test can stand on either side of a hold's end.
var holdClock = time.Now

// holdIsLocal and holdInvoke reach a held host. They are variables so that a
// test can play the far end without SSH: a host that is this machine is held
// in this process's own state, and every other host is asked to record the
// hold in its own state through `upkeeper __hold`.
var holdIsLocal = transport.IsLocal

var holdInvoke = func(spec inventory.HostSpec, input string) transport.Outcome {
	return transport.InvokeCommand(spec, []string{holdAgentCommand}, input, 60*time.Second, procx.Run)
}

// holdRequest is what `hold set` and `hold clear --hosts` send a held host.
// The controller's clock and user are carried, so the two records agree.
type holdRequest struct {
	SchemaVersion int     `json:"schema_version"`
	Action        string  `json:"action"`
	Host          string  `json:"host"`
	Reason        string  `json:"reason"`
	User          string  `json:"user"`
	At            string  `json:"at"`
	Until         *string `json:"until"`
}

// runOperatorHold sets or clears an operator hold on each named host.
//
// The hold is recorded in this host's own state first, so that every pull
// this host dispatches honours it at once, and then in each held host's own
// state, so that the host's own self-pull timer and every other controller's
// pull honour it too. A host that cannot be reached keeps only this host's
// copy; the run says so and exits 20, and running the command again delivers
// it. A failure to record here is fatal, because nothing else would hold.
func runOperatorHold(parsed *arguments, streams Streams) (int, error) {
	root, err := ConfigRoot()
	if err != nil {
		return exitcode.Preflight, err
	}
	config, err := inventory.Load(root)
	if err != nil {
		return exitcode.Preflight, err
	}
	hosts, err := operatorHoldHosts(parsed.holdHosts, config)
	if err != nil {
		return holdRefusal(parsed, streams, err.Error())
	}
	now := holdClock().UTC().Truncate(time.Second)
	var until time.Time
	if parsed.holdUntil != "" {
		if until, err = parseHoldUntil(parsed.holdUntil, now); err != nil {
			return holdRefusal(parsed, streams, err.Error())
		}
	}
	directory, err := operatorhold.DefaultDirectory()
	if err != nil {
		return exitcode.Preflight, err
	}
	user := operatorhold.UserName()
	entries := []any{}
	code := exitcode.Success
	for _, name := range hosts {
		entry := map[string]any{"host": name}
		request := holdRequest{
			SchemaVersion: operatorhold.SchemaVersion, Action: parsed.holdAction, Host: name,
			Reason: parsed.holdReason, User: user, At: pytime.ISO8601Z(now),
		}
		if parsed.holdAction == "set" {
			hold, err := operatorhold.Set(directory, operatorhold.Hold{
				Host: name, Reason: parsed.holdReason, SetBy: user, SetAt: now, Until: until,
			})
			if err != nil {
				return exitcode.Preflight, err
			}
			entry["hold"] = hold.Document(now)
			if !until.IsZero() {
				value := pytime.ISO8601Z(until)
				request.Until = &value
			}
		} else {
			_, cleared, err := operatorhold.Clear(directory, name, parsed.holdReason, user, now)
			if err != nil {
				return exitcode.Preflight, err
			}
			entry["cleared_here"] = cleared
		}
		entry["recorded"] = operatorhold.RecordPath(directory, name)
		if holdIsLocal(config.Hosts[name]) {
			entry["delivery"] = "local"
		} else if result, err := deliverHold(config.Hosts[name], request); err != nil {
			entry["delivery"] = "failed"
			entry["error"] = redact.Text(err.Error())
			code = exitcode.Partial
		} else {
			entry["delivery"] = "delivered"
			entry["remote_result"] = result
		}
		entries = append(entries, entry)
	}
	document := map[string]any{
		"schema_version": exitcode.SchemaVersion,
		"command":        "hold",
		"action":         parsed.holdAction,
		"reason":         parsed.holdReason,
		"user":           user,
		"at":             pytime.ISO8601Z(now),
		"directory":      directory,
		"hosts":          entries,
		"exit_code":      code,
	}
	if parsed.asJSON {
		return code, printJSON(streams.Out, document)
	}
	printOperatorHoldChange(streams.Out, parsed.holdAction, entries)
	return code, nil
}

// operatorHoldHosts checks each name against the inventory: a hold on a name
// no host has would hold nothing and look like it held something.
func operatorHoldHosts(names []string, config *inventory.Config) ([]string, error) {
	known := config.KnownHosts()
	seen := map[string]bool{}
	hosts := make([]string, 0, len(names))
	for _, raw := range names {
		name, err := safety.ValidateHostName(raw, known)
		if err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, failure.Validation("host list contains duplicates")
		}
		seen[name] = true
		hosts = append(hosts, name)
	}
	return hosts, nil
}

// parseHoldUntil reads --until as an RFC 3339 time or as a duration from now
// ("36h"). Either way the end must lie in the future.
func parseHoldUntil(value string, now time.Time) (time.Time, error) {
	until, err := time.Parse(time.RFC3339, value)
	if err != nil {
		duration, durationErr := time.ParseDuration(value)
		if durationErr != nil {
			return time.Time{}, failure.Validation(
				"argument --until: expected an RFC 3339 time such as 2026-10-09T18:00:00Z or a duration such as 36h, got %q", value)
		}
		until = now.Add(duration)
	}
	until = until.UTC().Truncate(time.Second)
	if !until.After(now) {
		return time.Time{}, failure.Validation("argument --until: %s is not in the future", pytime.ISO8601Z(until))
	}
	return until, nil
}

// deliverHold asks a remote host to record the request in its own state.
func deliverHold(spec inventory.HostSpec, request holdRequest) (string, error) {
	input, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	outcome := holdInvoke(spec, string(input))
	switch {
	case outcome.Err != nil:
		return "", fmt.Errorf("cannot reach %s: %v", spec.Name, outcome.Err)
	case outcome.TimedOut:
		return "", fmt.Errorf("%s did not answer in time", spec.Name)
	case outcome.MissingBinary():
		return "", fmt.Errorf("%s: %s", spec.Name, transport.MissingBinaryDiagnosis)
	case outcome.Code == exitcode.Usage:
		return "", fmt.Errorf("the upkeeper on %s predates operator holds; update its binary, or stop "+
			"its upkeeper-self.timer by hand until it can be held", spec.Name)
	case outcome.Code != 0:
		return "", fmt.Errorf("%s refused the hold (exit %d): %s", spec.Name, outcome.Code,
			firstLineOf(transport.Tail(strings.TrimSpace(outcome.Stdout+"\n"+outcome.Stderr), 400)))
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(outcome.Stdout), &answer); err != nil {
		return "", fmt.Errorf("%s answered with something other than a hold receipt", spec.Name)
	}
	result, _ := answer["result"].(string)
	if result == "" {
		return "", fmt.Errorf("%s answered without a result", spec.Name)
	}
	return result, nil
}

// runHoldAgent is the held host's half of a hold change: one JSON request on
// standard input, one JSON receipt on standard output.
func runHoldAgent(streams Streams) (int, error) {
	raw, err := io.ReadAll(io.LimitReader(streams.In, 64<<10))
	if err != nil {
		return exitcode.Preflight, err
	}
	var request holdRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return holdAgentRefusal(streams, "the hold request is not a hold request: "+err.Error())
	}
	if request.SchemaVersion != operatorhold.SchemaVersion {
		return holdAgentRefusal(streams, fmt.Sprintf("unsupported hold request schema %d", request.SchemaVersion))
	}
	at, err := pytime.ParseISO8601Z(request.At)
	if err != nil {
		return holdAgentRefusal(streams, err.Error())
	}
	directory, err := operatorhold.DefaultDirectory()
	if err != nil {
		return holdAgentRefusal(streams, err.Error())
	}
	receipt := map[string]any{
		"schema_version": operatorhold.SchemaVersion,
		"host":           request.Host,
		"path":           operatorhold.RecordPath(directory, request.Host),
	}
	switch request.Action {
	case "set":
		hold := operatorhold.Hold{Host: request.Host, Reason: request.Reason, SetBy: request.User, SetAt: at}
		if request.Until != nil {
			if hold.Until, err = pytime.ParseISO8601Z(*request.Until); err != nil {
				return holdAgentRefusal(streams, err.Error())
			}
		}
		if _, err := operatorhold.Set(directory, hold); err != nil {
			return holdAgentRefusal(streams, err.Error())
		}
		receipt["result"] = "set"
	case "clear":
		_, cleared, err := operatorhold.Clear(directory, request.Host, request.Reason, request.User, at)
		if err != nil {
			return holdAgentRefusal(streams, err.Error())
		}
		receipt["result"] = "absent"
		if cleared {
			receipt["result"] = "cleared"
		}
	default:
		return holdAgentRefusal(streams, fmt.Sprintf("unknown hold action %q", request.Action))
	}
	encoded, err := canonjson.Compact(receipt)
	if err != nil {
		return exitcode.Preflight, err
	}
	fmt.Fprintln(streams.Out, string(encoded))
	return exitcode.Success, nil
}

func holdAgentRefusal(streams Streams, message string) (int, error) {
	fmt.Fprintf(streams.Err, "upkeeper __hold: %s\n", message)
	return exitcode.Preflight, nil
}

// printOperatorHolds renders the operator holds this host keeps.
func printOperatorHolds(out io.Writer, directory string, holds []operatorhold.Hold, now time.Time) {
	if len(holds) == 0 {
		fmt.Fprintf(out, "no operator holds under %s\n", directory)
		return
	}
	for _, hold := range holds {
		state := "held"
		if !hold.Active(now) {
			state = "expired, no longer enforced"
		}
		until := "until cleared"
		if !hold.Until.IsZero() {
			until = "until " + pytime.ISO8601Z(hold.Until)
		}
		fmt.Fprintf(out, "%s: operator hold (%s) set by %s at %s, %s\n",
			hold.Host, state, hold.SetBy, pytime.ISO8601Z(hold.SetAt), until)
		fmt.Fprintf(out, "  reason: %s\n", hold.Reason)
		fmt.Fprintf(out, "  clear with: %s\n", operatorhold.ClearCommand(hold.Host))
	}
}

// printOperatorHoldChange reports a set or clear for a person, one line per
// host, naming where the hold now lives and what could not be reached.
func printOperatorHoldChange(out io.Writer, action string, entries []any) {
	for _, item := range entries {
		entry, _ := item.(map[string]any)
		name := text(entry["host"])
		verb := "held"
		if action == "clear" {
			verb = "released"
		}
		switch text(entry["delivery"]) {
		case "local":
			fmt.Fprintf(out, "%s: %s (this host; recorded at %s)\n", name, verb, text(entry["recorded"]))
		case "delivered":
			fmt.Fprintf(out, "%s: %s here and on %s (%s)\n", name, verb, name, text(entry["remote_result"]))
		default:
			fmt.Fprintf(out, "%s: %s here only; %s; rerun this command once %s is reachable\n",
				name, verb, text(entry["error"]), name)
		}
	}
}

// pullHoldRefusal refuses an explicit pull to held hosts before anything is
// dispatched, naming every hold and both ways forward.
func pullHoldRefusal(hosts []string, now time.Time) error {
	directory, err := operatorhold.DefaultDirectory()
	if err != nil {
		return err
	}
	held := []string{}
	for _, name := range hosts {
		if hold, active := operatorhold.Check(directory, name, now); active {
			held = append(held, hold.Describe())
		}
	}
	if len(held) == 0 {
		return nil
	}
	return failure.Pull("%s; nothing was dispatched. Pass --override-hold to pull through the hold "+
		"(it is logged), or release it with: upkeeper hold clear --hosts HOST --reason TEXT",
		strings.Join(held, "; "))
}

// selfPullHeld reports this host's operator hold, which stops a self-pull
// from refreshing the checkout or updating its binary. A hold that cannot be
// read holds, as operatorhold.Check does.
func selfPullHeld(host string, now time.Time) (operatorhold.Hold, bool) {
	directory, err := operatorhold.DefaultDirectory()
	if err != nil {
		return operatorhold.Hold{Host: host, SetBy: "unknown", Reason: err.Error()}, true
	}
	return operatorhold.Check(directory, host, now)
}
