package release

import (
	"errors"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

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
//     that no //go:embed directive can match. When either commit has a cgo
//     Go file or a non-Go source the go command compiles (assembly, C and
//     the like), no such file is ignored: an #include can reach it anywhere.
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
	includes := false
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
			if programForeignSources[path.Ext(name)] {
				includes = true
			}
		}
		found, err := programEmbeds(git, commit)
		if err != nil {
			return false, err
		}
		embeds = append(embeds, found...)
		cgo, err := programUsesCgo(git, commit)
		if err != nil {
			return false, err
		}
		includes = includes || cgo
	}
	for _, name := range candidates {
		if !strings.HasSuffix(path.Base(name), "_test.go") && (includes || packages[path.Dir(name)]) {
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

// programForeignSources are the non-Go file extensions the go command
// compiles in a package (go/build's lists). Any of them can #include a file
// from anywhere in the tree.
var programForeignSources = map[string]bool{
	".c": true, ".cc": true, ".cpp": true, ".cxx": true, ".m": true,
	".h": true, ".hh": true, ".hpp": true, ".hxx": true,
	".f": true, ".F": true, ".for": true, ".f90": true,
	".s": true, ".S": true, ".sx": true, ".swig": true, ".swigcxx": true, ".syso": true,
}

// programGrep runs git grep over the non-test Go files under ProgramPaths at
// one commit. Every file is read as text whatever git's attributes say, so a
// file marked binary cannot hide a directive, and the output format does not
// depend on the user's grep configuration. found is false when nothing matched.
func programGrep(git ProgramGit, commit string, options ...string) (string, bool, error) {
	arguments := append([]string{"grep", "--text", "--no-color", "--no-line-number", "-z"},
		options...)
	arguments = append(arguments, commit, "--")
	for _, name := range ProgramPaths {
		arguments = append(arguments, ":(glob)"+name+"/**/*.go")
	}
	arguments = append(arguments, ":(exclude,glob)**/*_test.go")
	result := git(arguments...)
	if result.Err != nil || (result.Code != 0 && result.Code != 1) {
		return "", false, ErrProgramIncomparable
	}
	return result.Stdout, result.Code == 0, nil
}

// programUsesCgo reports whether a non-test Go file may import "C". It only
// looks for the literal, so a mention in a comment or string counts as well:
// that costs nothing but a needlessly conservative comparison.
func programUsesCgo(git ProgramGit, commit string) (bool, error) {
	_, found, err := programGrep(git, commit, "-l", "-F", "-e", `"C"`)
	return found, err
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
	output, found, err := programGrep(git, commit, "-E", "-e", `^[[:space:]]*//go:embed`)
	if err != nil || !found {
		return nil, err
	}
	embeds := []programEmbed{}
	for _, line := range strings.Split(output, "\n") {
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
		// The go command drops carriage returns from comments.
		match := programEmbedDirective.FindStringSubmatch(strings.ReplaceAll(text, "\r", ""))
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
	// The go command splits on any Unicode space; anything but a space or a
	// tab, or any other control character, is left to the generous fallback.
	if strings.IndexFunc(arguments, func(r rune) bool {
		return r != ' ' && r != '\t' && (unicode.IsSpace(r) || unicode.IsControl(r))
	}) >= 0 {
		return nil, false
	}
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
