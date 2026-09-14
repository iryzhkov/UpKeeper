package cli

import (
	"strings"

	"github.com/iryzhkov/UpKeeper/internal/release"
)

// helpRequest is -h or --help. argparse answered it by printing the help text
// on stdout and exiting 0, and people and scripts rely on both halves of that:
// a help flag that writes to stderr and exits 2 looks exactly like a mistake.
// It travels as an error only because that is how the parser reports "stop
// here", and Main turns it back into a successful print.
type helpRequest struct{ command string }

func (h *helpRequest) Error() string { return "help requested" }

// isHelpFlag reports whether a token asks for help.
func isHelpFlag(token string) bool { return token == "-h" || token == "--help" }

// containsHelpFlag reports whether a subcommand's own arguments ask for help.
// argparse accepted -h anywhere in the arguments of the parser that owned it,
// even after other options, so `upkeeper pull --all --help` is a help request
// and not a convergence.
func containsHelpFlag(argv []string) bool {
	for _, token := range argv {
		if isHelpFlag(token) {
			return true
		}
	}
	return false
}

// commandSummaries are the one-line descriptions listed under the top-level
// help. The first five are the wording the Python parser used, kept so that
// the text an operator has read before does not change under them.
var commandSummaries = [][2]string{
	{"doctor", "report whether each host can be managed at all"},
	{"status", "report what each host is running now"},
	{"push", "capture this machine as the release manifest, commit it and push the branch"},
	{"pull", "converge hosts on an exact release manifest"},
	{"rollback", "converge hosts on an exact release manifest"},
	{"fleet-plan", "report the Steward fleet intent and what enrolling it would change"},
	{"self-pull", "refresh the gated release and converge this machine only"},
	{"scheduled-pull", "converge the fleet on the published release, for the timer"},
	{"install-self-timer", "install the self-convergence systemd user timer"},
	{"install-pull-timer", "install the controller systemd user timer"},
	{"version", "print the exact source commit this binary was built from"},
}

const helpOption = "  -h, --help            show this help message and exit"

// helpText renders the help for a command, or for the program when command is
// empty. The layout follows the parser this replaced: a usage line, an
// optional description, then the argument sections.
func helpText(program, command string) string {
	switch command {
	case "":
		return topHelp(program)
	case "doctor", "status":
		return probeHelp(program, command)
	case "push":
		return pushHelp(program)
	case "pull":
		return pullHelp(program)
	case "rollback":
		return rollbackHelp(program)
	case "fleet-plan":
		return fleetPlanHelp(program)
	case "self-pull", "scheduled-pull":
		return scheduledHelp(program, command)
	default:
		return plainHelp(program, command)
	}
}

func topHelp(program string) string {
	var text strings.Builder
	text.WriteString("usage: " + program + " [-h] {" + strings.Join(Commands, ",") + "} ...\n\n")
	text.WriteString("Developer fleet discovery (doctor, status) and UpKeeper releases (push, pull)\n\n")
	text.WriteString("positional arguments:\n")
	text.WriteString("  {" + strings.Join(Commands, ",") + "}\n")
	for _, summary := range commandSummaries {
		text.WriteString(describe("    "+summary[0], summary[1]))
	}
	text.WriteString("\noptions:\n" + helpOption + "\n")
	return text.String()
}

func probeHelp(program, command string) string {
	return "usage: " + program + " " + command +
		" [-h] [--json] [--limit HOST[,HOST...]] [--timeout SECONDS]\n\noptions:\n" +
		helpOption + "\n" +
		describe("  --json", "") +
		describe("  --limit HOST[,HOST...]",
			"exact canonical inventory names; no patterns or arbitrary expressions") +
		describe("  --timeout SECONDS", "5 through 60, per host")
}

func pushHelp(program string) string {
	return "usage: " + program + " push [-h] [--components COMPONENT [COMPONENT ...]]" +
		" [--no-commit] [--no-push] [--json] [REMOTE ...]\n\npositional arguments:\n" +
		describe("  REMOTE", "git remotes to push to (default: all)") +
		"\noptions:\n" + helpOption + "\n" +
		describe("  --components COMPONENT [COMPONENT ...]",
			"capture only these; every other component keeps what the release already published") +
		describe("  --no-commit", "write the manifest only") +
		describe("  --no-push", "commit but do not push") +
		describe("  --json", "") +
		componentChoices()
}

func pullHelp(program string) string {
	return "usage: " + program + " pull [-h] (--hosts HOST [HOST ...] | --all | --self)" +
		" [--skip-unreachable] [--components COMPONENT [COMPONENT ...]]" +
		" [--dry-run] [--timeout TIMEOUT] [--json]\n\noptions:\n" +
		helpOption + "\n" +
		describe("  --hosts HOST [HOST ...]", "exact inventory names") +
		describe("  --all", "every inventory host") +
		describe("  --self", "read this checkout and converge only this inventory machine") +
		convergeOptions() +
		componentChoices()
}

func rollbackHelp(program string) string {
	return "usage: " + program + " rollback [-h] --to RELEASE-COMMIT (--hosts HOST [HOST ...] | --all)" +
		" [--skip-unreachable] [--components COMPONENT [COMPONENT ...]]" +
		" [--dry-run] [--timeout TIMEOUT] [--json]\n\noptions:\n" +
		helpOption + "\n" +
		describe("  --to RELEASE-COMMIT", "") +
		describe("  --hosts HOST [HOST ...]", "exact inventory names") +
		describe("  --all", "every inventory host") +
		convergeOptions() +
		componentChoices()
}

// fleetPlanHelp documents a command that only reads. Saying so in the help is
// part of the contract: the one thing an operator must be able to trust about
// this command is that running it cannot enroll anything.
func fleetPlanHelp(program string) string {
	return "usage: " + program + " fleet-plan [-h] [--previous RELEASE-COMMIT] [--json]\n\n" +
		"Report the Steward fleet intent this release carries: each host's desired\n" +
		"model allowlist, the models observed on it where an observation exists, the\n" +
		"digest of every projection, and the enrollment this release proposes.\n\n" +
		"It writes nothing, reaches no host and never enrolls. Applying a catalog is\n" +
		"a deliberate Steward admin operation with its own revision fence.\n\n" +
		"options:\n" + helpOption + "\n" +
		describe("  --previous RELEASE-COMMIT",
			"diff the authorization against the release at this commit") +
		describe("  --json", "") +
		"\nexit codes: 0 reported, 2 malformed command line, 30 refused\n" +
		"\nexample:\n  " + program + " fleet-plan --previous 6bcd2f8 --json\n"
}

func scheduledHelp(program, command string) string {
	purpose := "refresh the gated release and converge this machine only"
	if command == "scheduled-pull" {
		purpose = "converge the fleet on the published release, for the timer"
	}
	return "usage: " + program + " " + command + " [-h] [--dry-run] [--converge]\n\n" +
		purpose + "\n\noptions:\n" + helpOption + "\n" +
		describe("  --dry-run", "observe; change nothing") +
		describe("  --converge", "skip the refresh and converge the checkout as it stands")
}

func plainHelp(program, command string) string {
	purpose := ""
	for _, summary := range commandSummaries {
		if summary[0] == command {
			purpose = summary[1] + "\n\n"
		}
	}
	return "usage: " + program + " " + command + " [-h]\n\n" + purpose +
		"options:\n" + helpOption + "\n"
}

func convergeOptions() string {
	return describe("  --skip-unreachable",
		"skip offline hosts; authentication failures still fail") +
		describe("  --components COMPONENT [COMPONENT ...]", "") +
		describe("  --dry-run", "observe; change nothing") +
		describe("  --timeout TIMEOUT", "seconds per host") +
		describe("  --json", "")
}

// componentChoices lists what --components accepts. The Python parser inlined
// the choice set into the usage line, which wrapped over four lines and was
// unreadable; the names are the same, listed once where they can be read.
func componentChoices() string {
	return "\nCOMPONENT is one of: " + strings.Join(release.ComponentOrder, ", ") + "\n"
}

// describe lays out one argument and its help at argparse's column, dropping
// to the next line when the argument is too wide for it, exactly as argparse
// did.
func describe(argument, help string) string {
	const column = 24
	if help == "" {
		return argument + "\n"
	}
	if len(argument) >= column {
		return argument + "\n" + strings.Repeat(" ", column) + help + "\n"
	}
	return argument + strings.Repeat(" ", column-len(argument)) + help + "\n"
}
