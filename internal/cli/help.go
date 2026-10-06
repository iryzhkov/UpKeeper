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
	{"environment", "inspect operator-pinned candidate tool content offline"},
	{"doctor", "report whether each host can be managed at all"},
	{"status", "report what each host is running now"},
	{"push", "capture this machine as the release manifest, commit it and push the branch"},
	{"pull", "converge hosts on an exact release manifest"},
	{"rollback", "converge hosts on an exact release manifest"},
	{"fleet-plan", "report the Steward fleet intent and what enrolling it would change"},
	{"project", "add or remove a Steward project in the release manifest and show the plan"},
	{"provider", "register or remove a provider instance and its quota binding in the release manifest"},
	{"author", "pin a Steward release and an omarchy-setup commit in the release manifest, without capturing this host"},
	{"self-pull", "refresh the gated release and converge this machine only"},
	{"scheduled-pull", "converge the fleet on the published release, for the timer"},
	{"install-self-timer", "install the self-convergence systemd user timer"},
	{"install-pull-timer", "install the controller systemd user timer"},
	{"hold", "show or clear the scheduled hold markers this host keeps"},
	{"self", "install, roll back or inspect this host's own UpKeeper binary generations"},
	{"version", "print the exact source commit this binary was built from"},
}

const helpOption = "  -h, --help            show this help message and exit"

// helpText renders the help for a command, or for the program when command is
// empty. The layout follows the parser this replaced: a usage line, an
// optional description, then the argument sections.
func helpText(program, command string) string {
	switch command {
	case "environment", "environment-tools":
		return environmentHelp(program, command)
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
	case "project":
		return projectHelp(program)
	case "provider":
		return providerHelp(program)
	case "author":
		return authorHelp(program)
	case "self-pull", "scheduled-pull":
		return scheduledHelp(program, command)
	case "hold":
		return holdHelp(program)
	case "self":
		return selfHelp(program)
	default:
		return plainHelp(program, command)
	}
}

// holdHelp documents the two hold actions. A hold is cleared by name and with
// a reason, never in bulk: each marker stands for one host somebody has to
// look at.
func holdHelp(program string) string {
	return "usage: " + program + " hold show [-h] [--json]\n" +
		"       " + program + " hold clear [-h] HOST --reason TEXT [--json]\n\n" +
		"A scheduled run writes ~/.local/state/dev-fleet/scheduled-holds/HOST.hold\n" +
		"before it dispatches and removes it only after an understood result. A\n" +
		"marker that is still there means the run died, lost its output or failed;\n" +
		"later scheduled runs refuse that host until the marker is cleared.\n\n" +
		"show lists every marker on this host with its content, age and the run\n" +
		"that wrote it. clear removes exactly one marker after you have observed the\n" +
		"host, and records who cleared it, when and why in HOST.hold.cleared.\n\n" +
		"options:\n" + helpOption + "\n" +
		describe("  --reason TEXT", "why the hold is safe to clear (clear only; required)") +
		describe("  --json", "") +
		"\nexit codes: 0 done, 2 malformed command line or no marker for HOST, 30 refused\n"
}

// selfHelp documents the generation commands. A self-pull updates the binary
// on its own when the gated release pins another program; these are for the
// first bootstrap and for an operator who has to undo or inspect one.
func selfHelp(program string) string {
	return "usage: " + program + " self install [-h] --expect-sha256 SHA256 [--json]\n" +
		"       " + program + " self rollback [-h] [--json]\n" +
		"       " + program + " self status [-h] [--json]\n\n" +
		"Binaries live as generations under ~/.local/lib/upkeeper/generations, and\n" +
		"~/.local/bin/upkeeper links to the current one. A self-pull installs the\n" +
		"binary the gated release pins, verified by size and sha256, when the\n" +
		"program differs from the one running.\n\n" +
		"install installs the binary running this command, after checking that its\n" +
		"sha256 is the one given and that it names a clean source commit. rollback\n" +
		"makes the previous generation current again. status changes nothing.\n\n" +
		"options:\n" + helpOption + "\n" +
		describe("  --expect-sha256 SHA256", "the digest this binary must have (install only; required)") +
		describe("  --json", "") +
		"\nexit codes: 0 done, 2 malformed command line, 30 refused, 60 another change holds the lock\n"
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
		" [--no-commit] [--no-push] [--publish-diverged-instructions]" +
		" [--authored --expect-head SHA --expect-release-sha256 SHA256]" +
		" [--json] [REMOTE ...]\n\npositional arguments:\n" +
		describe("  REMOTE", "git remotes to push to (default: all)") +
		"\noptions:\n" + helpOption + "\n" +
		describe("  --components COMPONENT [COMPONENT ...]",
			"capture only these; every other component keeps what the release already published") +
		describe("  --no-commit", "write the manifest only") +
		describe("  --no-push", "commit but do not push") +
		describe("  --authored", "publish the reviewed release bytes without recapturing this host") +
		describe("  --expect-head SHA", "required authored-mode source HEAD fence") +
		describe("  --expect-release-sha256 SHA256", "required authored-mode release content fence") +
		describe("  --publish-diverged-instructions",
			"publish this host's agent instructions even though they differ from the "+
				"omarchy-setup checkout that generates them, printing every path it promotes") +
		describe("  --json", "") +
		componentChoices()
}

func pullHelp(program string) string {
	return "usage: " + program + " pull [-h] (--hosts HOST [HOST ...] | --all | --self)" +
		" [--skip-unreachable] [--components COMPONENT [COMPONENT ...]]" +
		" [--dry-run] [--timeout TIMEOUT] [--json]" +
		" [--reconcile-t3-transaction NAME]\n\noptions:\n" +
		helpOption + "\n" +
		describe("  --hosts HOST [HOST ...]", "exact inventory names") +
		describe("  --all", "every inventory host") +
		describe("  --self", "read this checkout and converge only this inventory machine") +
		describe("  --reconcile-t3-transaction NAME",
			"verify and record one retained .upkeeper-t3-* transaction left at files-restored; "+
				"needs one --hosts HOST and --components t3, restarts and moves nothing, "+
				"refuses any drift; with --dry-run it only reports") +
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

// projectHelp documents the one command that edits the release manifest for
// a person. It says what the command does not do as plainly as what it does:
// the edit is local, reviewed as a Git diff, and published by the same push
// every other release change goes through.
func projectHelp(program string) string {
	return "usage: " + program + " project add NAME --repository URL --default-ref REF" +
		" --workers HOST[,HOST...] [--type git] [--setup-profile PROFILE] [--json]\n" +
		"       " + program + " project add NAME --type fresh --workers HOST[,HOST...]" +
		" [--setup-profile PROFILE] [--json]\n" +
		"       " + program + " project remove NAME [--json]\n" +
		"       " + program + " project update NAME --expect-release-sha256 SHA256" +
		" [--repository URL] [--default-ref REF] [--setup-profile PROFILE]" +
		" [--workers HOST[,HOST...]] [--json]\n\n" +
		"Add or remove one Steward project in the steward-fleet-configuration component\n" +
		"of desired/release.json, validate the edited manifest with the same parser\n" +
		"fleet-plan uses, and print the fleet-plan diff against the release committed at\n" +
		"HEAD. The edit is written atomically in the manifest's canonical encoding.\n\n" +
		"It commits nothing, pushes nothing, pulls nothing and never enrolls: review the\n" +
		"diff, commit it, and publish it with `" + program + " push`. An added project is\n" +
		"loaded by the coordinator with default local bindings, and every worker it is\n" +
		"eligible for needs `t3-steward worker enroll <worker> --current-catalog` on the\n" +
		"coordinator host after the release converges.\n\n" +
		"positional arguments:\n" +
		describe("  {add,remove,update}", "") +
		describe("  NAME", "project name: lowercase letters, digits and hyphens") +
		"\noptions:\n" + helpOption + "\n" +
		describe("  --type {git,fresh}", "project workspace type; omitted preserves the legacy Git form (add)") +
		describe("  --repository URL", "canonical https or ssh remote (Git add)") +
		describe("  --default-ref REF", "branch, tag or commit the project checks out (Git add)") +
		describe("  --workers HOST[,HOST...]",
			"fleet hosts eligible for the project; an unknown host is refused (add)") +
		describe("  --setup-profile PROFILE",
			"Steward setup profile (add default; update optional)") +
		describe("  --expect-release-sha256 SHA256", "exact release fence (update only; required)") +
		describe("  --json", "") +
		"\nexit codes: 0 edited, 2 malformed command line, 30 refused (nothing written)\n" +
		"\nexample:\n  " + program + " project add home-assistant-config \\\n" +
		"    --repository ssh://git@git.ryzhkov.dev/igor/home-assistant-config.git \\\n" +
		"    --default-ref main --workers homelab,normandy,omarchy-pc\n"
}

// providerHelp documents the second command that edits the release manifest
// for a person. Like projectHelp it says what the command does not do, and it
// says what the binding is: authorization to charge a pool, never evidence
// that the instance exists on the host.
func providerHelp(program string) string {
	return "usage: " + program + " provider add INSTANCE --quota-pool POOL --hosts HOST[,HOST...]" +
		" [--models MODEL[,MODEL...]] [--json]\n" +
		"       " + program + " provider remove INSTANCE [--hosts HOST[,HOST...]] [--json]\n\n" +
		"Register or remove one provider instance in the steward-fleet-configuration\n" +
		"component of desired/release.json: add adds INSTANCE to provider_instances, its\n" +
		"models (default \"*\") to models, POOL to quota_pools and INSTANCE: POOL to\n" +
		"quota_bindings, on every named host at once. When every host of a profile is\n" +
		"named the profile is edited; otherwise each named host's exceptions are, and\n" +
		"the output says which. The edited manifest is validated with the same parser\n" +
		"fleet-plan uses, written atomically in its canonical encoding, and the\n" +
		"fleet-plan diff against the release committed at HEAD is printed.\n\n" +
		"It commits nothing, pushes nothing, pulls nothing and never enrolls: review the\n" +
		"diff, commit it, and publish it with `" + program + " push`. Every host whose\n" +
		"catalog entry changed needs `t3-steward worker enroll <worker> --current-catalog`\n" +
		"on the coordinator host after the release converges. A binding is authorization\n" +
		"to charge the pool; it never claims the instance is installed or signed in.\n\n" +
		"positional arguments:\n" +
		describe("  {add,remove}", "") +
		describe("  INSTANCE", "provider instance name, as the T3 instance is named on the hosts") +
		"\noptions:\n" + helpOption + "\n" +
		describe("  --quota-pool POOL", "the one quota pool the instance's work is charged to (add)") +
		describe("  --hosts HOST[,HOST...]",
			"fleet hosts to register on (add: required; remove: default every host); "+
				"an unknown host is refused") +
		describe("  --models MODEL[,MODEL...]",
			"desired model allowlist (add; default \"*\" for a new instance, kept for an existing one)") +
		describe("  --json", "") +
		"\nexit codes: 0 edited, 2 malformed command line, 30 refused (nothing written)\n" +
		"\nexample:\n  " + program + " provider add opencode --quota-pool opencode-free \\\n" +
		"    --hosts homelab,normandy,omarchy-pc --models glm-4.6,kimi-k2\n"
}

// authorHelp documents the command that writes an authored release. Its one
// promise is determinism: the output depends on the named versions and the
// checkout's HEAD, never on what this host has installed.
func authorHelp(program string) string {
	return "usage: " + program + " author [-h] --steward VERSION --omarchy-setup COMMIT" +
		" [--omarchy-setup-repo PATH] [--dry-run] [--json]\n\n" +
		"Edit desired/release.json in this checkout for an authored release: pin\n" +
		"t3-steward.version and t3.compatibility.steward_version to VERSION and\n" +
		"omarchy-setup.commit to COMMIT, then refresh every agent-environment file that\n" +
		"derives from that commit: the skills under config/claude/skills, projected to\n" +
		".claude/skills and .agents/skills; ~/.config/agents and ~/.codex/AGENTS.md as the\n" +
		"commit's own config/bin/agents-instructions-gen writes them into an empty scratch\n" +
		"home; and ~/.claude/omarchy-setup/CLAUDE.md. New skill files are added, files the\n" +
		"commit no longer produces are retired, and the spec is rebuilt the way capture\n" +
		"builds it, with the instruction provenance at this checkout's HEAD. When no\n" +
		"derived file changed, the component is left byte for byte as it was.\n\n" +
		"Nothing is read from this host's home directory. The commit is read from\n" +
		"the omarchy-setup clone, which fetches origin first and must reach it from\n" +
		"the ref the release follows. The checkout may have no local change except to\n" +
		"desired/release.json itself. The edit is validated as push validates it, and\n" +
		"every other byte of the manifest is preserved.\n\n" +
		"It commits nothing and pushes nothing: review the diff, commit it, and publish\n" +
		"it with `" + program + " push --authored --expect-head SHA --expect-release-sha256 SHA256`.\n\n" +
		"options:\n" + helpOption + "\n" +
		describe("  --steward VERSION", "t3-steward release version to pin") +
		describe("  --omarchy-setup COMMIT", "omarchy-setup commit to pin, in hexadecimal; recorded in full") +
		describe("  --omarchy-setup-repo PATH", "local omarchy-setup clone (default: ~/Work/omarchy-setup)") +
		describe("  --dry-run", "report what would change; write nothing") +
		describe("  --json", "") +
		"\nexit codes: 0 authored or already current, 2 malformed command line, 30 refused (nothing written)\n" +
		"\nexample:\n  " + program + " author --steward 0.11.0-rc.95 \\\n" +
		"    --omarchy-setup 89e222624c983f33b4f54d28fb5b4c3ea5b555ad --dry-run\n"
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
