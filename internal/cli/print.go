package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/redact"
	"github.com/iryzhkov/UpKeeper/internal/safety"
)

// printJSON writes a report the way every machine consumer expects it:
// redacted, sorted, indented by two spaces and newline terminated.
func printJSON(out io.Writer, report map[string]any) error {
	encoded, err := canonjson.Marshal(redact.Value(report))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, string(encoded))
	return err
}

func printDoctor(out io.Writer, report map[string]any) {
	fmt.Fprintf(out, "run %s controller=%s doctor=%s exit=%s\n",
		text(report["run_id"]), text(report["controller"]),
		text(report["result"]), scalar(report["exit_code"]))
	for _, item := range list(report["hosts"]) {
		host, _ := item.(map[string]any)
		if !flag(host["reachable"]) {
			fmt.Fprintf(out, "%s: offline (%s)\n", text(host["name"]), errorText(host))
			continue
		}
		failed := stringList(host["failed_checks"])
		suffix := ""
		if len(failed) > 0 {
			suffix = " failed=" + join(failed, ",")
		}
		fmt.Fprintf(out, "%s: %s%s\n", text(host["name"]), text(host["result"]), suffix)
		for _, item := range list(host["blockers"]) {
			blocker, _ := item.(map[string]any)
			fmt.Fprintf(out, "  blocker: %s needs %s; %s\n",
				text(blocker["component"]), text(blocker["name"]), text(blocker["install"]))
		}
		for _, notice := range stringList(host["notices"]) {
			fmt.Fprintf(out, "  notice: %s\n", notice)
		}
	}
}

func printStatus(out io.Writer, report map[string]any) {
	fmt.Fprintf(out, "run %s controller=%s status=%s exit=%s\n",
		text(report["run_id"]), text(report["controller"]),
		text(report["result"]), scalar(report["exit_code"]))
	for _, item := range list(report["hosts"]) {
		host, _ := item.(map[string]any)
		if !flag(host["reachable"]) {
			fmt.Fprintf(out, "%s: offline (%s)\n", text(host["name"]), errorText(host))
			continue
		}
		summaries := []string{}
		reasons := []string{}
		for _, entry := range list(host["components"]) {
			component, _ := entry.(map[string]any)
			summaries = append(summaries,
				text(component["name"])+"="+text(component["state"]))
			if reason := text(component["reason"]); reason != "" {
				reasons = append(reasons, "  "+text(component["name"])+": "+reason)
			}
		}
		fmt.Fprintf(out, "%s: %s [%s]\n",
			text(host["name"]), text(host["result"]), join(summaries, ", "))
		for _, reason := range reasons {
			fmt.Fprintln(out, reason)
		}
		if line := binaryLine(host); line != "" {
			fmt.Fprintln(out, line)
		}
	}
}

// binaryLine names the UpKeeper binary a host reported running, or "" for a
// host whose binary predates the report.
func binaryLine(host map[string]any) string {
	evidence, _ := host["upkeeper"].(map[string]any)
	binary, reported := evidence["binary"].(map[string]any)
	if !reported {
		return ""
	}
	generation := shorten(text(binary["generation"]), 12)
	if generation == "" {
		generation = "unmanaged"
	}
	return fmt.Sprintf("  upkeeper %s commit %s generation %s",
		text(binary["version"]), shorten(text(binary["commit"]), 12), generation)
}

// printSelfStatus renders `self status` for a person.
func printSelfStatus(out io.Writer, status map[string]any) {
	running, _ := status["running"].(map[string]any)
	fmt.Fprintf(out, "running upkeeper %s (commit %s)\n",
		text(running["version"]), text(running["commit"]))
	binary, _ := status["binary"].(map[string]any)
	switch {
	case flag(binary["managed"]):
		fmt.Fprintf(out, "%s runs the current generation\n", text(binary["path"]))
	case flag(binary["missing"]):
		fmt.Fprintf(out, "%s does not exist\n", text(binary["path"]))
	case text(binary["link"]) != "":
		fmt.Fprintf(out, "%s is not managed: it links to %s\n", text(binary["path"]), text(binary["link"]))
	default:
		fmt.Fprintf(out, "%s is not managed: it is a file\n", text(binary["path"]))
	}
	for _, item := range list(status["generations"]) {
		generation, _ := item.(map[string]any)
		marks := []string{}
		if flag(generation["current"]) {
			marks = append(marks, "current")
		}
		if flag(generation["previous"]) {
			marks = append(marks, "previous")
		}
		if !flag(generation["valid"]) {
			marks = append(marks, "INVALID")
		}
		identity, _ := generation["identity"].(map[string]any)
		description := text(identity["version_line"])
		if description == "" {
			description = text(identity["source"])
		}
		fmt.Fprintf(out, "  %s %s [%s]\n", text(generation["name"]), description, join(marks, ","))
	}
}

func printPush(out io.Writer, report map[string]any) {
	for _, line := range stringList(report["summary"]) {
		fmt.Fprintf(out, "  %s\n", line)
	}
	if committed := text(report["committed"]); committed != "" {
		fmt.Fprintf(out, "committed %s\n", shorten(committed, 12))
	} else if _, present := report["head"]; present {
		fmt.Fprintln(out, "nothing new to commit")
	}
	for _, pushed := range stringList(report["pushed"]) {
		fmt.Fprintf(out, "pushed %s\n", pushed)
	}
}

// printFleetPlan renders the plan for a person.
//
// Desired and observed are printed as separate lines, and a host with no
// observation says so rather than printing an empty set: collapsing those two
// answers is how an unroutable host became unexplainable.
func printFleetPlan(out io.Writer, report map[string]any) {
	fmt.Fprintf(out, "coordinator %s on %s\n",
		text(report["coordinator_id"]), text(report["coordinator_host"]))
	hosts, _ := report["hosts"].(map[string]any)
	for _, name := range safety.SortedKeys(hosts) {
		host, _ := hosts[name].(map[string]any)
		fmt.Fprintf(out, "%s: profile=%s slots=%s worker=%s\n",
			name, text(host["profile"]), scalar(host["executor_slots"]),
			shorten(text(host["worker_sha256"]), 12))
		fmt.Fprintf(out, "  desired  %s\n", modelSummary(host["desired_models"]))
		observed, present := host["observed_models"]
		if !present || observed == nil {
			fmt.Fprintln(out, "  observed not observed")
			continue
		}
		fmt.Fprintf(out, "  observed %s\n", modelSummary(observed))
	}
	artifact, _ := report["coordinator_projection"].(map[string]any)
	fmt.Fprintf(out, "coordinator projection %s (%s)\n",
		shorten(text(artifact["sha256"]), 12), text(artifact["purpose"]))
	plan, _ := report["enrollment_plan"].(map[string]any)
	if revision := plan["expected_revision"]; revision != nil {
		fmt.Fprintf(out, "last observed coordinator revision %s\n", scalar(revision))
	}
	for _, change := range stringList(plan["authorization_changes"]) {
		fmt.Fprintf(out, "  %s\n", change)
	}
	if advice := text(plan["fence"]); advice != "" {
		fmt.Fprintf(out, "%s\n", advice)
	}
	fmt.Fprintln(out, "not enrolled; applying this is an explicit Steward operation")
}

// modelSummary renders one host's allowlists as instance=model,model.
func modelSummary(value any) string {
	models, _ := value.(map[string]any)
	if len(models) == 0 {
		return "none"
	}
	summaries := []string{}
	for _, instance := range safety.SortedKeys(models) {
		summaries = append(summaries,
			instance+"="+join(stringList(models[instance]), ","))
	}
	return join(summaries, " ")
}

func printPull(out io.Writer, report map[string]any) {
	mode := ""
	if flag(report["dry_run"]) {
		mode = " (dry run)"
	}
	fmt.Fprintf(out, "run %s controller=%s exit=%s%s\n",
		text(report["run_id"]), text(report["controller"]),
		scalar(report["exit_code"]), mode)
	for _, item := range list(report["hosts"]) {
		host, _ := item.(map[string]any)
		if flag(host["self_managed"]) {
			fmt.Fprintf(out, "%s: self-managed (local timer; no controller SSH)\n",
				text(host["host"]))
			continue
		}
		if !flag(host["reachable"]) {
			fmt.Fprintf(out, "%s: offline (%s)\n", text(host["host"]), errorText(host))
			if stderr := text(host["stderr"]); strings.TrimSpace(stderr) != "" {
				fmt.Fprintf(out, "  %s\n", lastLine(stderr))
			}
			continue
		}
		fmt.Fprintf(out, "%s: %s\n", text(host["host"]), text(host["result"]))
		if retirement, ok := host["legacy_updater"].(map[string]any); ok {
			fmt.Fprintf(out, "  legacy updater: %s\n", text(retirement["state"]))
			for _, action := range stringList(retirement["actions"]) {
				fmt.Fprintf(out, "    - %s\n", action)
			}
			if message := text(retirement["error"]); message != "" {
				fmt.Fprintf(out, "    ! %s\n", message)
			}
		}
		for _, entry := range list(host["components"]) {
			component, _ := entry.(map[string]any)
			fmt.Fprintf(out, "  %s: %s\n", text(component["name"]), text(component["state"]))
			for _, action := range stringList(component["actions"]) {
				fmt.Fprintf(out, "    - %s\n", action)
			}
			if message := text(component["error"]); message != "" {
				fmt.Fprintf(out, "    ! %s\n", message)
			}
		}
		if harness, ok := host["harness"].(map[string]any); ok {
			missing := []string{}
			for _, name := range sortedKeys(harness) {
				if registered, ok := harness[name].(bool); !ok || !registered {
					missing = append(missing, name)
				}
			}
			if len(missing) > 0 {
				fmt.Fprintf(out, "  harness without huyang registration: %s\n",
					join(missing, ", "))
			}
		}
	}
}

func errorText(host map[string]any) string {
	if message := text(host["error"]); message != "" {
		return message
	}
	return "unreachable"
}

func lastLine(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	return lines[len(lines)-1]
}

func join(values []string, separator string) string {
	return strings.Join(values, separator)
}

func shorten(value string, size int) string {
	if len(value) <= size {
		return value
	}
	return value[:size]
}
