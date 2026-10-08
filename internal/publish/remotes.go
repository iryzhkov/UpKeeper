package publish

import (
	"sort"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// PublicationBranch is the one branch a publication updates, on every remote.
// Targets and release-preview read main; a release published under any other
// name is a release nobody consumes.
const PublicationBranch = "main"

const publicationRef = "refs/heads/" + PublicationBranch

// checkPublication is the publication preflight. It runs before anything is
// captured, written or committed, and every refusal names what it found and
// the command that fixes it:
//
//   - every configured remote is published to, so no remote is left behind
//     (Gitea's main once lagged GitHub's by a week and nobody noticed until
//     release-preview refused to author);
//   - the checkout is on main, so a release is never published under the
//     name of whatever branch the clone happened to be on;
//   - every remote's main is the same commit, and that commit is an ancestor
//     of HEAD, so the publication is a fast-forward everywhere or nowhere.
func checkPublication(root string, runner procx.Runner, remotes, configured []string) error {
	if omitted := missing(configured, remotes); len(omitted) > 0 {
		return failure.Push("a publication updates %s on every configured remote, and %s was not named; "+
			"omit the remote arguments to publish to all of them", PublicationBranch, strings.Join(omitted, ", "))
	}
	ref, err := git(runner, root, 60*time.Second, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return failure.Push("cannot publish from a detached HEAD; check out %s first: git switch %s",
			PublicationBranch, PublicationBranch)
	}
	if ref != publicationRef {
		return failure.Push("the checkout is on branch %s; a publication updates %s on every remote and "+
			"never the branch it was run from. Switch to %s (git switch %s), bring the change onto it, "+
			"and publish again", strings.TrimPrefix(ref, "refs/heads/"), PublicationBranch,
			PublicationBranch, PublicationBranch)
	}
	tips := map[string]string{}
	for _, remote := range remotes {
		output, err := git(runner, root, 60*time.Second, "ls-remote", "--heads", remote, publicationRef)
		if err != nil {
			return failure.Push("cannot read %s on %s: %v", PublicationBranch, remote, err)
		}
		tips[remote] = ""
		for _, line := range strings.Split(output, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[1] == publicationRef {
				tips[remote] = fields[0]
			}
		}
	}
	if len(remotes) == 0 {
		return nil
	}
	distinct := map[string]bool{}
	for _, tip := range tips {
		distinct[tip] = true
	}
	if len(distinct) > 1 {
		return divergedRemotes(root, runner, remotes, tips)
	}
	tip := tips[remotes[0]]
	if tip == "" {
		// No remote has main yet: the first publication creates it everywhere.
		return nil
	}
	if err := fetchMain(root, runner, remotes[0]); err != nil {
		return err
	}
	if _, err := git(runner, root, 60*time.Second, "merge-base", "--is-ancestor", tip, "HEAD"); err != nil {
		return failure.Push("%s on every remote is at %s, which this checkout's HEAD does not contain; "+
			"nothing was published. Bring the checkout up to date first: git pull --ff-only %s %s",
			PublicationBranch, tip, remotes[0], PublicationBranch)
	}
	return nil
}

// divergedRemotes explains remotes whose main differ. When one remote's main
// contains every other's, the others are behind and one push each resyncs
// them; otherwise their histories have diverged and a person has to merge.
func divergedRemotes(root string, runner procx.Runner, remotes []string, tips map[string]string) error {
	states := make([]string, 0, len(remotes))
	for _, remote := range remotes {
		tip := tips[remote]
		if tip == "" {
			tip = "absent"
		}
		states = append(states, remote+" at "+tip)
	}
	summary := "the remotes disagree on " + PublicationBranch + ": " + strings.Join(states, ", ") +
		"; nothing was published"
	for _, remote := range remotes {
		if tips[remote] != "" {
			if err := fetchMain(root, runner, remote); err != nil {
				return failure.Push("%s, and the remote histories could not be compared: %v", summary, err)
			}
		}
	}
	leader := ""
	for _, candidate := range remotes {
		if tips[candidate] == "" {
			continue
		}
		contains := true
		for _, other := range remotes {
			if other == candidate || tips[other] == "" || tips[other] == tips[candidate] {
				continue
			}
			if _, err := git(runner, root, 60*time.Second,
				"merge-base", "--is-ancestor", tips[other], tips[candidate]); err != nil {
				contains = false
				break
			}
		}
		if contains {
			leader = candidate
			break
		}
	}
	if leader == "" {
		return failure.Push("%s. Their histories have diverged: merge them by hand, push the result "+
			"to %s on every remote, and publish again", summary, PublicationBranch)
	}
	commands := []string{}
	for _, remote := range remotes {
		if tips[remote] != tips[leader] {
			commands = append(commands, "git push "+remote+" refs/remotes/"+leader+"/"+PublicationBranch+
				":"+publicationRef)
		}
	}
	sort.Strings(commands)
	return failure.Push("%s. %s contains every other remote's %s, so the others are behind; resync "+
		"them with: %s, then publish again", summary, leader, PublicationBranch, strings.Join(commands, " && "))
}

// fetchMain brings one remote's main into its remote-tracking ref, so that
// its ancestry can be checked locally. It writes no branch and no index.
func fetchMain(root string, runner procx.Runner, remote string) error {
	_, err := git(runner, root, 120*time.Second, "fetch", "--quiet", "--no-tags", remote,
		"+"+publicationRef+":refs/remotes/"+remote+"/"+PublicationBranch)
	return err
}
