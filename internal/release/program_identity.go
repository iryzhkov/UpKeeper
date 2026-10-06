package release

import (
	"errors"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// ProgramGit runs one git command against the repository whose program is
// being compared.
type ProgramGit func(arguments ...string) procx.Result

// ErrProgramIncomparable is returned when git could not establish whether two
// commits hold the same program. It never means that the program changed.
var ErrProgramIncomparable = errors.New("cannot compare the program")

var programEmbedDirective = regexp.MustCompile(`^[ \t]*//go:embed(?:[ \t]+(.*))?$`)

// ProgramDiffers reports whether the program differs between two commits:
// whether any build input under ProgramPaths changed.
//
// Tests are not build inputs, so a commit that only changes a test or a test
// fixture keeps the program's identity. A changed path is ignored only when it
// is proven not to be compiled at either commit:
//
//   - a *_test.go file that no //go:embed directive in a non-test Go file
//     can match;
//   - a file under a testdata directory that is not a Go file, whose
//     directory holds no non-test Go file (a package there can be imported
//     explicitly, and files beside its Go files can be compiled with it), and
//     that no //go:embed directive can match.
//
// Every other changed path, including a non-test Go file under testdata,
// changes the program. A directive this function cannot parse makes every
// path in its package's tree count.
func ProgramDiffers(git ProgramGit, from, to string) (bool, error) {
	quiet := git(append([]string{"diff", "--quiet", from, to, "--"}, ProgramPaths...)...)
	if quiet.Err != nil || (quiet.Code != 0 && quiet.Code != 1) {
		return false, ErrProgramIncomparable
	}
	if quiet.Code == 0 {
		return false, nil
	}
	names := git(append([]string{"diff", "--name-only", "--no-renames", "-z", from, to, "--"},
		ProgramPaths...)...)
	if !names.OK() {
		return false, ErrProgramIncomparable
	}
	changed := splitNul(names.Stdout)
	if len(changed) == 0 {
		// git found a difference it does not name: nothing proves it is a test.
		return true, nil
	}
	candidates := []string{}
	for _, name := range changed {
		if !testOnlyCandidate(name) {
			return true, nil
		}
		candidates = append(candidates, name)
	}
	packages := map[string]bool{}
	embeds := []programEmbed{}
	for _, commit := range []string{from, to} {
		files := git(append([]string{"ls-tree", "-r", "-z", "--name-only", commit, "--"},
			ProgramPaths...)...)
		if !files.OK() {
			return false, ErrProgramIncomparable
		}
		for _, name := range splitNul(files.Stdout) {
			if isProgramSource(name) {
				packages[path.Dir(name)] = true
			}
		}
		found, err := programEmbeds(git, commit)
		if err != nil {
			return false, err
		}
		embeds = append(embeds, found...)
	}
	for _, name := range candidates {
		if !strings.HasSuffix(path.Base(name), "_test.go") && packages[path.Dir(name)] {
			return true, nil
		}
		for _, embed := range embeds {
			if embed.matches(name) {
				return true, nil
			}
		}
	}
	return false, nil
}

// testOnlyCandidate is the first, name-only half of the proof: a test file,
// or a file under testdata that is not a Go source file.
func testOnlyCandidate(name string) bool {
	base := path.Base(name)
	if strings.HasSuffix(base, "_test.go") {
		return true
	}
	if strings.HasSuffix(base, ".go") {
		return false
	}
	return slices.Contains(strings.Split(path.Dir(name), "/"), "testdata")
}

func isProgramSource(name string) bool {
	return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
}

func splitNul(output string) []string {
	names := []string{}
	for _, name := range strings.Split(output, "\x00") {
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

// programEmbed is one //go:embed directive: the package directory it is
// relative to and its patterns, or every path below the directory when the
// directive could not be parsed.
type programEmbed struct {
	dir      string
	patterns []string
	all      bool
}

// matches is deliberately generous: a pattern matching the path or any of
// its parent directories embeds it, whatever Go's rules for hidden files.
func (e programEmbed) matches(name string) bool {
	prefix := e.dir + "/"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	if e.all {
		return true
	}
	elements := strings.Split(strings.TrimPrefix(name, prefix), "/")
	for _, pattern := range e.patterns {
		for count := 1; count <= len(elements); count++ {
			matched, err := path.Match(pattern, strings.Join(elements[:count], "/"))
			if err != nil || matched {
				return true
			}
		}
	}
	return false
}

// programEmbeds lists the //go:embed directives of the non-test Go files under
// ProgramPaths at one commit.
func programEmbeds(git ProgramGit, commit string) ([]programEmbed, error) {
	arguments := []string{"grep", "-z", "-I", "-E", `^[[:space:]]*//go:embed`, commit, "--"}
	for _, name := range ProgramPaths {
		arguments = append(arguments, ":(glob)"+name+"/**/*.go")
	}
	arguments = append(arguments, ":(exclude,glob)**/*_test.go")
	result := git(arguments...)
	if result.Err != nil || (result.Code != 0 && result.Code != 1) {
		return nil, ErrProgramIncomparable
	}
	if result.Code == 1 {
		return nil, nil
	}
	embeds := []programEmbed{}
	for _, line := range strings.Split(result.Stdout, "\n") {
		if line == "" {
			continue
		}
		// git grep -z prints "<commit>:<path>\x00<line>".
		location, text, ok := strings.Cut(line, "\x00")
		name, found := strings.CutPrefix(location, commit+":")
		if !ok || !found || name == "" {
			return nil, ErrProgramIncomparable
		}
		embed := programEmbed{dir: path.Dir(name)}
		match := programEmbedDirective.FindStringSubmatch(text)
		if match == nil {
			// For example "//go:embedded": not a directive, but nothing is
			// lost by counting its package's whole tree.
			embed.all = true
		} else if patterns, ok := embedPatterns(match[1]); ok {
			embed.patterns = patterns
		} else {
			embed.all = true
		}
		embeds = append(embeds, embed)
	}
	return embeds, nil
}

// embedPatterns splits a directive's arguments the way the go command does:
// space-separated, each optionally a Go string literal, with an optional
// "all:" prefix.
func embedPatterns(arguments string) ([]string, bool) {
	patterns := []string{}
	rest := strings.TrimLeft(arguments, " \t")
	for rest != "" {
		var pattern string
		switch rest[0] {
		case '"', '`':
			end := strings.IndexByte(rest[1:], rest[0])
			if rest[0] == '"' {
				end = -1
				for index := 1; index < len(rest); index++ {
					if rest[index] == '\\' {
						index++
						continue
					}
					if rest[index] == '"' {
						end = index - 1
						break
					}
				}
			}
			if end < 0 {
				return nil, false
			}
			unquoted, err := strconv.Unquote(rest[:end+2])
			if err != nil {
				return nil, false
			}
			pattern, rest = unquoted, rest[end+2:]
			if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
				return nil, false
			}
		default:
			end := strings.IndexAny(rest, " \t")
			if end < 0 {
				end = len(rest)
			}
			pattern, rest = rest[:end], rest[end:]
		}
		pattern = strings.TrimPrefix(pattern, "all:")
		if pattern == "" {
			return nil, false
		}
		patterns = append(patterns, pattern)
		rest = strings.TrimLeft(rest, " \t")
	}
	return patterns, len(patterns) > 0
}
