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
	"regexp"
	"strconv"
	"strings"

	"github.com/iryzhkov/UpKeeper/internal/toolcatalog"
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
	catalog toolcatalog.Options
	asJSON  bool

	// doctor and status
	limit        *string
	probeTimeout int

	// push
	remotes             []string
	noCommit            bool
	noPush              bool
	authored            bool
	expectHead          string
	expectReleaseSHA256 string
	// publishDivergedInstructions is the capture gate's override. It is a
	// separate flag rather than a mode of some general --force because the
	// operator has to be able to say this one thing and only this one thing.
	publishDivergedInstructions bool

	// pull and rollback
	hosts           []string
	all             bool
	selfOnly        bool
	skipUnreachable bool
	dryRun          bool
	pullTimeout     int
	to              string
	// reconcileT3 is pull's explicit resume of one retained controlled T3
	// transaction left at files-restored. It is its own flag rather than a
	// mode of a general --force: it names one transaction on one host.
	reconcileT3 string

	// shared
	components []string

	// self-pull and scheduled-pull
	convergeOnly bool
	// afterUpdate is the hidden self-pull option a freshly installed binary
	// is re-executed with: the commit whose program it was installed for.
	afterUpdate string
	// updateTransaction names the switch that installed it, when there was one.
	updateTransaction string

	// __agent: the program the controller requires this binary to be.
	agentProgram string

	// self
	selfAction       string
	selfExpectSHA256 string

	// fleet-plan
	previousRelease string

	// hold
	holdAction string
	holdHost   string
	holdReason string

	// project
	projectAction                string
	projectName                  string
	projectType                  string
	projectRepository            string
	projectDefaultRef            string
	projectSetupProfile          string
	projectWorkers               []string
	projectTypeSet               bool
	projectRepositorySet         bool
	projectDefaultRefSet         bool
	projectSetupProfileSet       bool
	projectWorkersSet            bool
	projectExpectedReleaseSHA256 string

	// provider
	providerAction   string
	providerInstance string
	providerPool     string
	providerModels   []string
	providerHosts    []string

	// author
	authorSteward         string
	authorSetupCommit     string
	authorSetupRepository string
}

// Commands are the subcommands `upkeeper` accepts. The two names beginning
// with underscores are the machine-facing halves a controller invokes on a
// target; they are deliberately undocumented in help output.
var Commands = []string{
	"doctor", "status", "push", "pull", "rollback", "fleet-plan", "project", "provider",
	"author", "self-pull", "scheduled-pull", "install-self-timer", "install-pull-timer",
	"hold", "self", "version", "environment",
}

const agentCommand = "__agent"
const probeCommand = "__probe"

// agentProgramFlag is the program fence on the agent's command line, spelled
// as converge.AgentProgramFlag. An agent from before the fence rejects it as
// an unrecognized argument, which is the point: it must not do fenced work.
const agentProgramFlag = "--expect-program"

// parseAgent accepts the agent's only option, the program fence.
func parseAgent(parsed *arguments, argv []string) error {
	for index := 0; index < len(argv); index++ {
		if argv[index] != agentProgramFlag || parsed.agentProgram != "" {
			return usagef("unrecognized arguments: %s", strings.Join(argv[index:], " "))
		}
		value, next, err := takeValue(argv, index, agentProgramFlag)
		if err != nil {
			return err
		}
		if !fullCommit(value) {
			return usagef("argument %s: expected a full lowercase commit", agentProgramFlag)
		}
		parsed.agentProgram = value
		index = next
	}
	return nil
}

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
	if parsed.command == "environment" {
		return parseEnvironment(parsed, rest)
	}
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
	case "provider":
		return parsed, parseProvider(parsed, rest)
	case "author":
		return parsed, parseAuthor(parsed, rest)
	case "self-pull", "scheduled-pull":
		return parsed, parseScheduled(parsed, rest)
	case "hold":
		return parsed, parseHold(parsed, rest)
	case "self":
		return parsed, parseSelf(parsed, rest)
	case agentCommand:
		return parsed, parseAgent(parsed, rest)
	case "install-self-timer", "install-pull-timer", probeCommand, "version":
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
		case token == "--publish-diverged-instructions":
			parsed.publishDivergedInstructions = true
		case token == "--authored":
			parsed.authored = true
		case token == "--expect-head":
			value, next, err := takeValue(argv, index, "--expect-head")
			if err != nil {
				return err
			}
			parsed.expectHead = value
			index = next
		case token == "--expect-release-sha256":
			value, next, err := takeValue(argv, index, "--expect-release-sha256")
			if err != nil {
				return err
			}
			parsed.expectReleaseSHA256 = value
			index = next
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
	if parsed.authored {
		if parsed.expectHead == "" || parsed.expectReleaseSHA256 == "" {
			return usagef("--authored requires --expect-head and --expect-release-sha256")
		}
		if len(parsed.components) != 0 {
			return usagef("--components is not valid with --authored")
		}
	} else if parsed.expectHead != "" || parsed.expectReleaseSHA256 != "" {
		return usagef("--expect-head and --expect-release-sha256 require --authored")
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
		case "--reconcile-t3-transaction":
			if parsed.command != "pull" {
				return unrecognized(token)
			}
			value, next, err := takeValue(argv, index, "--reconcile-t3-transaction")
			if err != nil {
				return err
			}
			parsed.reconcileT3 = value
			index = next
		default:
			return unrecognized(token)
		}
	}
	if parsed.reconcileT3 != "" {
		if !strings.HasPrefix(parsed.reconcileT3, ".upkeeper-t3-") || strings.ContainsRune(parsed.reconcileT3, '/') {
			return usagef("argument --reconcile-t3-transaction: expected a .upkeeper-t3-* transaction name")
		}
		if len(parsed.hosts) != 1 || parsed.all || parsed.selfOnly {
			return usagef("argument --reconcile-t3-transaction: requires --hosts with exactly one host")
		}
		if len(parsed.components) != 1 || parsed.components[0] != "t3" {
			return usagef("argument --reconcile-t3-transaction: requires --components t3")
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
	if parsed.projectAction != "add" && parsed.projectAction != "remove" && parsed.projectAction != "update" {
		return usagef("argument action: invalid choice: %q (choose from \"add\", \"remove\", \"update\")",
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
		if parsed.projectAction == "remove" {
			return unrecognized(token)
		}
		switch token {
		case "--type", "--repository", "--default-ref", "--setup-profile", "--workers", "--expect-release-sha256":
			value, next, err := takeValue(argv, index, token)
			if err != nil {
				return err
			}
			index = next
			switch token {
			case "--type":
				parsed.projectType = value
				parsed.projectTypeSet = true
			case "--repository":
				parsed.projectRepository = value
				parsed.projectRepositorySet = true
			case "--default-ref":
				parsed.projectDefaultRef = value
				parsed.projectDefaultRefSet = true
			case "--setup-profile":
				parsed.projectSetupProfile = value
				parsed.projectSetupProfileSet = true
			case "--workers":
				parsed.projectWorkers = splitWorkers(value)
				parsed.projectWorkersSet = true
				if len(parsed.projectWorkers) == 0 {
					return usagef("argument --workers: expected at least one worker")
				}
			case "--expect-release-sha256":
				parsed.projectExpectedReleaseSHA256 = value
			}
		default:
			return unrecognized(token)
		}
	}
	if parsed.projectAction == "update" {
		if parsed.projectTypeSet {
			return usagef("argument --type is only valid with project add")
		}
		if parsed.projectExpectedReleaseSHA256 == "" {
			return usagef("project update requires --expect-release-sha256")
		}
		if !parsed.projectRepositorySet && !parsed.projectDefaultRefSet && !parsed.projectSetupProfileSet && !parsed.projectWorkersSet {
			return usagef("project update requires at least one field flag")
		}
	}
	if parsed.projectAction == "add" {
		if parsed.projectType == "fresh" && !parsed.projectSetupProfileSet {
			parsed.projectSetupProfile = ""
		}
		if parsed.projectType != "" && parsed.projectType != "git" && parsed.projectType != "fresh" {
			return usagef("argument --type: invalid choice: %q (choose from \"git\", \"fresh\")", parsed.projectType)
		}
		if parsed.projectType == "fresh" && (parsed.projectRepository != "" || parsed.projectDefaultRef != "") {
			return usagef("fresh projects must not use --repository or --default-ref")
		}
		missing := []string{}
		if parsed.projectType != "fresh" {
			for _, required := range []struct{ flag, value string }{
				{"--repository", parsed.projectRepository},
				{"--default-ref", parsed.projectDefaultRef},
			} {
				if required.value == "" {
					missing = append(missing, required.flag)
				}
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

// parseProvider reads `provider add INSTANCE --quota-pool POOL --hosts a,b
// [--models m1,m2] [--json]` and `provider remove INSTANCE [--hosts a,b]
// [--json]`. Hosts and models are one comma-separated value each, the shape
// `project add --workers` already uses.
func parseProvider(parsed *arguments, argv []string) error {
	if len(argv) == 0 {
		return usagef("the following arguments are required: action (add or remove)")
	}
	parsed.providerAction = argv[0]
	if parsed.providerAction != "add" && parsed.providerAction != "remove" {
		return usagef("argument action: invalid choice: %q (choose from \"add\", \"remove\")",
			parsed.providerAction)
	}
	if len(argv) < 2 || strings.HasPrefix(argv[1], "-") {
		return usagef("the following arguments are required: INSTANCE")
	}
	parsed.providerInstance = argv[1]
	for index := 2; index < len(argv); index++ {
		token := argv[index]
		if token == "--json" {
			parsed.asJSON = true
			continue
		}
		if parsed.providerAction != "add" && token != "--hosts" {
			return unrecognized(token)
		}
		switch token {
		case "--quota-pool", "--hosts", "--models":
			value, next, err := takeValue(argv, index, token)
			if err != nil {
				return err
			}
			index = next
			switch token {
			case "--quota-pool":
				parsed.providerPool = value
			case "--hosts":
				parsed.providerHosts = splitWorkers(value)
				if len(parsed.providerHosts) == 0 {
					return usagef("argument --hosts: expected at least one host")
				}
			case "--models":
				parsed.providerModels = splitWorkers(value)
				if len(parsed.providerModels) == 0 {
					return usagef("argument --models: expected at least one model")
				}
			}
		default:
			return unrecognized(token)
		}
	}
	if parsed.providerAction == "add" {
		missing := []string{}
		if parsed.providerPool == "" {
			missing = append(missing, "--quota-pool")
		}
		if len(parsed.providerHosts) == 0 {
			missing = append(missing, "--hosts")
		}
		if len(missing) > 0 {
			return usagef("the following arguments are required: %s", strings.Join(missing, ", "))
		}
	}
	return nil
}

// parseAuthor reads `author --steward VERSION --omarchy-setup COMMIT
// [--omarchy-setup-repo PATH] [--dry-run] [--json]`. The repository has no
// default here because the default is a home-relative path, which the command
// resolves when it runs.
func parseAuthor(parsed *arguments, argv []string) error {
	for index := 0; index < len(argv); index++ {
		token := argv[index]
		switch token {
		case "--json":
			parsed.asJSON = true
		case "--dry-run":
			parsed.dryRun = true
		case "--steward", "--omarchy-setup", "--omarchy-setup-repo":
			value, next, err := takeValue(argv, index, token)
			if err != nil {
				return err
			}
			index = next
			switch token {
			case "--steward":
				parsed.authorSteward = value
			case "--omarchy-setup":
				parsed.authorSetupCommit = value
			case "--omarchy-setup-repo":
				parsed.authorSetupRepository = value
			}
		default:
			return unrecognized(token)
		}
	}
	missing := []string{}
	if parsed.authorSteward == "" {
		missing = append(missing, "--steward")
	}
	if parsed.authorSetupCommit == "" {
		missing = append(missing, "--omarchy-setup")
	}
	if len(missing) > 0 {
		return usagef("the following arguments are required: %s", strings.Join(missing, ", "))
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
	for index := 0; index < len(argv); index++ {
		token := argv[index]
		switch {
		case token == "--dry-run":
			parsed.dryRun = true
		case token == "--converge":
			parsed.convergeOnly = true
		case token == afterUpdateFlag && parsed.command == "self-pull":
			value, next, err := takeValue(argv, index, afterUpdateFlag)
			if err != nil {
				return err
			}
			if !fullCommit(value) {
				return usagef("argument %s: expected a full lowercase commit", afterUpdateFlag)
			}
			parsed.afterUpdate = value
			index = next
		case token == transactionFlag && parsed.command == "self-pull":
			value, next, err := takeValue(argv, index, transactionFlag)
			if err != nil {
				return err
			}
			if !transactionPattern.MatchString(value) {
				return usagef("argument %s: expected 32 lowercase hex digits", transactionFlag)
			}
			parsed.updateTransaction = value
			index = next
		default:
			return unrecognized(token)
		}
	}
	if parsed.updateTransaction != "" && parsed.afterUpdate == "" {
		return usagef("argument %s: only allowed with %s", transactionFlag, afterUpdateFlag)
	}
	// The re-executed binary converges exactly the refreshed checkout; it
	// neither refreshes again nor previews.
	if parsed.afterUpdate != "" && (parsed.dryRun || parsed.convergeOnly) {
		return usagef("argument %s: not allowed with --dry-run or --converge", afterUpdateFlag)
	}
	return nil
}

// afterUpdateFlag is undocumented on purpose: only a self-update passes it,
// spelled as selfupdate.AfterUpdateFlag.
const afterUpdateFlag = "--after-update"

// transactionFlag names the switch the self-update made, spelled as
// selfupdate.TransactionFlag; it is as undocumented as afterUpdateFlag.
const transactionFlag = "--update-transaction"

var transactionPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func fullCommit(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

// parseSelf reads `self install --expect-sha256 SHA [--json]`, `self rollback
// [--json]` and `self status [--json]`. The digest is required on the command
// line: installing a binary is authorized by the digest somebody checked, not
// by the binary's own say-so.
func parseSelf(parsed *arguments, argv []string) error {
	if len(argv) == 0 || strings.HasPrefix(argv[0], "-") {
		return usagef("the following arguments are required: action (install, rollback or status)")
	}
	parsed.selfAction = argv[0]
	if parsed.selfAction != "install" && parsed.selfAction != "rollback" && parsed.selfAction != "status" {
		return usagef("argument action: invalid choice: %q (choose from \"install\", \"rollback\", \"status\")",
			parsed.selfAction)
	}
	rest := argv[1:]
	for index := 0; index < len(rest); index++ {
		token := rest[index]
		switch {
		case token == "--json":
			parsed.asJSON = true
		case token == "--expect-sha256" && parsed.selfAction == "install":
			value, next, err := takeValue(rest, index, "--expect-sha256")
			if err != nil {
				return err
			}
			parsed.selfExpectSHA256 = value
			index = next
		default:
			return unrecognized(token)
		}
	}
	if parsed.selfAction == "install" && parsed.selfExpectSHA256 == "" {
		return usagef("the following arguments are required: --expect-sha256")
	}
	return nil
}

// parseHold reads `hold show [--json]` and `hold clear HOST --reason TEXT
// [--json]`. The reason is required on the command line, not prompted for: a
// cleared hold with no recorded reason is the gap this command exists to close.
func parseHold(parsed *arguments, argv []string) error {
	if len(argv) == 0 || strings.HasPrefix(argv[0], "-") {
		return usagef("the following arguments are required: action (show or clear)")
	}
	parsed.holdAction = argv[0]
	rest := argv[1:]
	switch parsed.holdAction {
	case "show":
		for _, token := range rest {
			if token != "--json" {
				return unrecognized(token)
			}
			parsed.asJSON = true
		}
	case "clear":
		for index := 0; index < len(rest); index++ {
			token := rest[index]
			switch {
			case token == "--json":
				parsed.asJSON = true
			case token == "--reason":
				value, next, err := takeValue(rest, index, "--reason")
				if err != nil {
					return err
				}
				parsed.holdReason = value
				index = next
			case strings.HasPrefix(token, "-") || parsed.holdHost != "":
				return unrecognized(token)
			default:
				parsed.holdHost = token
			}
		}
		if parsed.holdHost == "" {
			return usagef("the following arguments are required: HOST")
		}
		if strings.TrimSpace(parsed.holdReason) == "" {
			return usagef("the following arguments are required: --reason")
		}
	default:
		return usagef("argument action: invalid choice: %q (choose from \"show\", \"clear\")",
			parsed.holdAction)
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
