// Package cli is the command surface of UpKeeper.
//
// The arguments are parsed by hand rather than with the standard flag package,
// because the contract this replaces was written with Python's argparse and
// includes shapes flag cannot express: repeated values after one flag
// (--hosts a b c), a required mutually exclusive group, and exit code 2 for a
// malformed command line. Operators and unit files depend on all three.
package cli

import (
	"fmt"
	"strconv"
	"strings"
)

// usageError is a malformed command line. It exits 2, like argparse did.
type usageError struct{ message string }

func (e *usageError) Error() string { return e.message }

func usagef(format string, arguments ...any) *usageError {
	return &usageError{message: fmt.Sprintf(format, arguments...)}
}

// arguments is the parsed command line.
type arguments struct {
	command string
	asJSON  bool

	// doctor and status
	limit        *string
	probeTimeout int

	// push
	remotes  []string
	noCommit bool
	noPush   bool

	// pull and rollback
	hosts           []string
	all             bool
	selfOnly        bool
	skipUnreachable bool
	dryRun          bool
	pullTimeout     int
	to              string

	// shared
	components []string

	// self-pull and scheduled-pull
	convergeOnly bool

	// fleet-plan
	previousRelease string

	// project
	projectAction       string
	projectName         string
	projectRepository   string
	projectDefaultRef   string
	projectSetupProfile string
	projectWorkers      []string
}

// Commands are the subcommands `upkeeper` accepts. The two names beginning
// with underscores are the machine-facing halves a controller invokes on a
// target; they are deliberately undocumented in help output.
var Commands = []string{
	"doctor", "status", "push", "pull", "rollback", "fleet-plan", "project",
	"self-pull", "scheduled-pull", "install-self-timer", "install-pull-timer",
	"version",
}

const agentCommand = "__agent"
const probeCommand = "__probe"

// isCommand reports whether a token names a subcommand. The machine-facing
// halves are deliberately excluded: they have no help to print and a
// controller never asks for it.
func isCommand(token string) bool {
	for _, command := range Commands {
		if command == token {
			return true
		}
	}
	return false
}

func parse(argv []string, componentChoices []string) (*arguments, error) {
	if len(argv) == 0 {
		return nil, usagef("the following arguments are required: command")
	}
	if isHelpFlag(argv[0]) {
		return nil, &helpRequest{}
	}
	parsed := &arguments{command: argv[0], probeTimeout: 20, pullTimeout: 1800}
	rest := argv[1:]
	if containsHelpFlag(rest) && isCommand(parsed.command) {
		return nil, &helpRequest{command: parsed.command}
	}
	switch parsed.command {
	case "doctor", "status":
		return parsed, parseProbeCommand(parsed, rest)
	case "push":
		return parsed, parsePush(parsed, rest, componentChoices)
	case "pull", "rollback":
		return parsed, parsePull(parsed, rest, componentChoices)
	case "fleet-plan":
		return parsed, parseFleetPlan(parsed, rest)
	case "project":
		return parsed, parseProject(parsed, rest)
	case "self-pull", "scheduled-pull":
		return parsed, parseScheduled(parsed, rest)
	case "install-self-timer", "install-pull-timer", agentCommand, probeCommand, "version":
		if len(rest) > 0 {
			return nil, usagef("unrecognized arguments: %s", strings.Join(rest, " "))
		}
		return parsed, nil
	default:
		return nil, usagef(
			"argument command: invalid choice: %q (choose from %s)",
			parsed.command, strings.Join(quoteAll(Commands), ", "))
	}
}

func parseProbeCommand(parsed *arguments, argv []string) error {
	for index := 0; index < len(argv); index++ {
		switch argv[index] {
		case "--json":
			parsed.asJSON = true
		case "--limit":
			value, next, err := takeValue(argv, index, "--limit")
			if err != nil {
				return err
			}
			parsed.limit = &value
			index = next
		case "--timeout":
			value, next, err := takeValue(argv, index, "--timeout")
			if err != nil {
				return err
			}
			number, err := strconv.Atoi(value)
			if err != nil {
				return usagef("argument --timeout: invalid int value: %q", value)
			}
			// The bound was expressed as a choice set, so a value outside it
			// has always been a usage error rather than a refusal.
			if number < 5 || number > 60 {
				return usagef(
					"argument --timeout: invalid choice: %d (choose from 5 through 60)", number)
			}
			parsed.probeTimeout = number
			index = next
		default:
			return unrecognized(argv[index])
		}
	}
	return nil
}

func parsePush(parsed *arguments, argv []string, choices []string) error {
	for index := 0; index < len(argv); index++ {
		token := argv[index]
		switch {
		case token == "--json":
			parsed.asJSON = true
		case token == "--no-commit":
			parsed.noCommit = true
		case token == "--no-push":
			parsed.noPush = true
		case token == "--components":
			values, next, err := takeValues(argv, index, "--components")
			if err != nil {
				return err
			}
			if err := checkChoices("--components", values, choices); err != nil {
				return err
			}
			parsed.components = values
			index = next
		case strings.HasPrefix(token, "-"):
			return unrecognized(token)
		default:
			parsed.remotes = append(parsed.remotes, token)
		}
	}
	return nil
}

func parsePull(parsed *arguments, argv []string, choices []string) error {
	group := []string{}
	for index := 0; index < len(argv); index++ {
		token := argv[index]
		switch token {
		case "--json":
			parsed.asJSON = true
		case "--dry-run":
			parsed.dryRun = true
		case "--skip-unreachable":
			parsed.skipUnreachable = true
		case "--all":
			parsed.all = true
			group = append(group, "--all")
		case "--self":
			if parsed.command != "pull" {
				return unrecognized(token)
			}
			parsed.selfOnly = true
			group = append(group, "--self")
		case "--hosts":
			values, next, err := takeValues(argv, index, "--hosts")
			if err != nil {
				return err
			}
			parsed.hosts = values
			group = append(group, "--hosts")
			index = next
		case "--components":
			values, next, err := takeValues(argv, index, "--components")
			if err != nil {
				return err
			}
			if err := checkChoices("--components", values, choices); err != nil {
				return err
			}
			parsed.components = values
			index = next
		case "--timeout":
			value, next, err := takeValue(argv, index, "--timeout")
			if err != nil {
				return err
			}
			number, err := strconv.Atoi(value)
			if err != nil {
				return usagef("argument --timeout: invalid int value: %q", value)
			}
			parsed.pullTimeout = number
			index = next
		case "--to":
			if parsed.command != "rollback" {
				return unrecognized(token)
			}
			value, next, err := takeValue(argv, index, "--to")
			if err != nil {
				return err
			}
			parsed.to = value
			index = next
		default:
			return unrecognized(token)
		}
	}
	if parsed.command == "rollback" && parsed.to == "" {
		return usagef("the following arguments are required: --to")
	}
	if len(group) == 0 {
		if parsed.command == "pull" {
			return usagef("one of the arguments --hosts --all --self is required")
		}
		return usagef("one of the arguments --hosts --all is required")
	}
	if len(group) > 1 {
		return usagef("argument %s: not allowed with argument %s", group[1], group[0])
	}
	if group[0] == "--hosts" && len(parsed.hosts) == 0 {
		return usagef("argument --hosts: expected at least one argument")
	}
	return nil
}

// parseFleetPlan reads the options of a command that only reports. It takes no
// host selector on purpose: a plan describes the fleet, and describing half of
// it would hide the authorization it is meant to expose.
func parseFleetPlan(parsed *arguments, argv []string) error {
	for index := 0; index < len(argv); index++ {
		switch argv[index] {
		case "--json":
			parsed.asJSON = true
		case "--previous":
			value, next, err := takeValue(argv, index, "--previous")
			if err != nil {
				return err
			}
			parsed.previousRelease = value
			index = next
		default:
			return unrecognized(argv[index])
		}
	}
	return nil
}

// projectSetupProfileDefault is the setup profile a project gets when none is
// named. Every project the fleet carries today uses it; a project that needs
// another one says so with --setup-profile.
const projectSetupProfileDefault = "git-status"

// parseProject reads `project add NAME --repository URL --default-ref REF
// --workers a,b [--setup-profile P] [--json]` and `project remove NAME
// [--json]`. The workers are one comma-separated value, the shape the fleet
// contract and the issue that asked for this command both use.
func parseProject(parsed *arguments, argv []string) error {
	if len(argv) == 0 {
		return usagef("the following arguments are required: action (add or remove)")
	}
	parsed.projectAction = argv[0]
	if parsed.projectAction != "add" && parsed.projectAction != "remove" {
		return usagef("argument action: invalid choice: %q (choose from \"add\", \"remove\")",
			parsed.projectAction)
	}
	if len(argv) < 2 || strings.HasPrefix(argv[1], "-") {
		return usagef("the following arguments are required: NAME")
	}
	parsed.projectName = argv[1]
	parsed.projectSetupProfile = projectSetupProfileDefault
	for index := 2; index < len(argv); index++ {
		token := argv[index]
		if token == "--json" {
			parsed.asJSON = true
			continue
		}
		if parsed.projectAction != "add" {
			return unrecognized(token)
		}
		switch token {
		case "--repository", "--default-ref", "--setup-profile", "--workers":
			value, next, err := takeValue(argv, index, token)
			if err != nil {
				return err
			}
			index = next
			switch token {
			case "--repository":
				parsed.projectRepository = value
			case "--default-ref":
				parsed.projectDefaultRef = value
			case "--setup-profile":
				parsed.projectSetupProfile = value
			case "--workers":
				parsed.projectWorkers = splitWorkers(value)
				if len(parsed.projectWorkers) == 0 {
					return usagef("argument --workers: expected at least one worker")
				}
			}
		default:
			return unrecognized(token)
		}
	}
	if parsed.projectAction == "add" {
		missing := []string{}
		for _, required := range []struct{ flag, value string }{
			{"--repository", parsed.projectRepository},
			{"--default-ref", parsed.projectDefaultRef},
		} {
			if required.value == "" {
				missing = append(missing, required.flag)
			}
		}
		if len(parsed.projectWorkers) == 0 {
			missing = append(missing, "--workers")
		}
		if len(missing) > 0 {
			return usagef("the following arguments are required: %s", strings.Join(missing, ", "))
		}
	}
	return nil
}

// splitWorkers reads a comma-separated worker list, ignoring empty entries so
// that a trailing comma is not a worker called "".
func splitWorkers(value string) []string {
	workers := []string{}
	for _, worker := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(worker); trimmed != "" {
			workers = append(workers, trimmed)
		}
	}
	return workers
}

func parseScheduled(parsed *arguments, argv []string) error {
	for _, token := range argv {
		switch token {
		case "--dry-run":
			parsed.dryRun = true
		case "--converge":
			parsed.convergeOnly = true
		default:
			return unrecognized(token)
		}
	}
	return nil
}

// takeValue reads the single value that follows a flag.
func takeValue(argv []string, index int, flag string) (string, int, error) {
	if index+1 >= len(argv) || strings.HasPrefix(argv[index+1], "--") {
		return "", index, usagef("argument %s: expected one argument", flag)
	}
	return argv[index+1], index + 1, nil
}

// takeValues reads every value that follows a flag until the next flag, which
// is how argparse's nargs="+" behaves.
func takeValues(argv []string, index int, flag string) ([]string, int, error) {
	values := []string{}
	position := index + 1
	for position < len(argv) && !strings.HasPrefix(argv[position], "--") {
		values = append(values, argv[position])
		position++
	}
	if len(values) == 0 {
		return nil, index, usagef("argument %s: expected at least one argument", flag)
	}
	return values, position - 1, nil
}

func checkChoices(flag string, values, choices []string) error {
	allowed := make(map[string]bool, len(choices))
	for _, choice := range choices {
		allowed[choice] = true
	}
	for _, value := range values {
		if !allowed[value] {
			return usagef("argument %s: invalid choice: %q (choose from %s)",
				flag, value, strings.Join(quoteAll(choices), ", "))
		}
	}
	return nil
}

func unrecognized(token string) error {
	return usagef("unrecognized arguments: %s", token)
}

func quoteAll(values []string) []string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, strconv.Quote(value))
	}
	return quoted
}
