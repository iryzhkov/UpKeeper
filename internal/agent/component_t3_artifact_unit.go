package agent

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/release"
)

// t3ExecWords decodes systemctl's quoted/C-escaped display, never shell code.
// Unknown escapes and control characters are refused rather than guessed.
func t3ExecWords(s string) ([]string, error) {
	var words []string
	var word strings.Builder
	var quote byte
	active := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '\\' {
			i++
			if i == len(s) {
				return nil, errors.New("truncated ExecStart escape")
			}
			ch = s[i]
			switch ch {
			case '\\', '"', '\'', ' ':
				word.WriteByte(ch)
			case 's':
				word.WriteByte(' ')
			case 'x':
				if i+2 >= len(s) {
					return nil, errors.New("truncated ExecStart hex escape")
				}
				n, e := strconv.ParseUint(s[i+1:i+3], 16, 8)
				if e != nil || n < 32 || n == 127 {
					return nil, errors.New("invalid ExecStart hex escape")
				}
				word.WriteByte(byte(n))
				i += 2
			default:
				return nil, errors.New("unsupported ExecStart escape")
			}
			active = true
		} else if quote != 0 {
			if ch == quote {
				quote = 0
			} else {
				if ch < 32 || ch == 127 {
					return nil, errors.New("ExecStart control character")
				}
				word.WriteByte(ch)
			}
		} else if ch == '"' || ch == '\'' {
			quote = ch
			active = true
		} else if ch == ' ' || ch == '\t' {
			if active {
				words = append(words, word.String())
				word.Reset()
				active = false
			}
		} else {
			if ch < 32 || ch == 127 || ch == '{' || ch == '}' || ch == ';' {
				return nil, errors.New("ambiguous ExecStart display")
			}
			word.WriteByte(ch)
			active = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated ExecStart quote")
	}
	if active {
		words = append(words, word.String())
	}
	return words, nil
}

type t3UnitCommand struct {
	Path         string
	Argv         []string
	IgnoreErrors string
}

// Read exactly one systemctl ExecStart record. Runtime result fields are
// deliberately excluded; every configuration field must be understood.
func t3ParseUnitCommand(s string) (t3UnitCommand, error) {
	var command t3UnitCommand
	s = strings.TrimSpace(s)
	if len(s) > 65536 || !strings.HasPrefix(s, "{ ") || !strings.HasSuffix(s, " }") {
		return command, errors.New("unknown ExecStart record")
	}
	s = s[2 : len(s)-2]
	fields := map[string]string{}
	start := 0
	var quote byte
	escaped := false
	for i := 0; i <= len(s); i++ {
		if i < len(s) {
			ch := s[i]
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if quote != 0 {
				if ch == quote {
					quote = 0
				}
				continue
			}
			if ch == '"' || ch == '\'' {
				quote = ch
				continue
			}
			if ch == '{' || ch == '}' || ch == '\n' || ch == '\r' {
				return command, errors.New("multiple or malformed ExecStart records")
			}
			if ch != ';' {
				continue
			}
		}
		field := strings.TrimSpace(s[start:i])
		start = i + 1
		if field == "" {
			if i != len(s) {
				return command, errors.New("empty ExecStart field")
			}
			continue
		}
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			return command, errors.New("malformed ExecStart field")
		}
		if _, duplicate := fields[key]; duplicate {
			return command, errors.New("duplicate ExecStart field")
		}
		switch key {
		case "path", "argv[]", "ignore_errors":
		case "start_time", "stop_time", "pid", "code", "status":
		default:
			return command, errors.New("unsupported ExecStart field or flags")
		}
		fields[key] = value
	}
	if quote != 0 || escaped || fields["ignore_errors"] != "no" {
		return command, errors.New("unsupported ExecStart flags or quoting")
	}
	path, e := t3ExecWords(fields["path"])
	if e != nil || len(path) != 1 || !filepath.IsAbs(path[0]) {
		return command, errors.New("unknown configured ExecStart path")
	}
	argv, e := t3ExecWords(fields["argv[]"])
	if e != nil || len(argv) < 2 || argv[0] != path[0] {
		return command, errors.New("unknown or modified ExecStart argv")
	}
	command = t3UnitCommand{path[0], argv, fields["ignore_errors"]}
	return command, nil
}

// These are the seeded serve command and the retained legacy start command.
// Flags are kept in full in the binding; unknown subcommands/options fail closed.
func t3SupportedServiceArgs(argv []string) bool {
	if len(argv) == 1 && argv[0] == "start" {
		return true
	}
	if len(argv) == 0 || argv[0] != "serve" {
		return false
	}
	seen := map[string]bool{}
	for i := 1; i < len(argv); i++ {
		flag := argv[i]
		if seen[flag] {
			return false
		}
		seen[flag] = true
		switch flag {
		case "--no-browser":
		case "--host", "--port":
			i++
			if i == len(argv) || argv[i] == "" || strings.HasPrefix(argv[i], "-") {
				return false
			}
			if flag == "--port" {
				n, e := strconv.Atoi(argv[i])
				if e != nil || n < 1 || n > 65535 {
					return false
				}
			}
		default:
			return false
		}
	}
	return true
}

// Bind stable configured path/complete argv/flags and the actual owned launcher.
// The owned bin link may legitimately change between legacy and modern layout.
func t3UnitBinding(c *Context, service, prefix string) (string, error) {
	result := c.Run([]string{"systemctl", "--user", "show", service, "-p", "ExecStart", "--value"}, 10*time.Second, "")
	command, e := t3ParseUnitCommand(result.Output)
	if !result.OK || e != nil {
		return "", errors.New("unknown T3 service invocation")
	}
	selected := command.Path
	args := command.Argv[1:]
	resolved, resolveErr := filepath.EvalSymlinks(selected)
	mise := ""
	if resolveErr == nil && filepath.Base(resolved) == "mise" {
		mise = resolved
		lookup := selected == filepath.Join(c.Home, ".local/share/mise/shims/t3")
		if lookup {
			// argv[0] must be the t3 shim, not a generic mise binary.
		} else {
			// No tool selectors, cwd/environment modifiers or other subcommands.
			if len(args) < 4 || args[0] != "exec" || args[1] != "--" {
				return "", errors.New("unsupported mise T3 invocation")
			}
			selected = args[2]
			args = args[3:]
			if selected != "t3" && !filepath.IsAbs(selected) {
				return "", errors.New("unknown mise invoked program")
			}
		}
		if !t3SupportedServiceArgs(args) {
			return "", errors.New("unsupported T3 service arguments")
		}
		if lookup || selected == "t3" {
			which := c.Run([]string{mise, "which", "t3"}, 15*time.Second, "")
			words := strings.Split(strings.TrimSpace(which.Output), "\n")
			if !which.OK || len(words) != 1 || !filepath.IsAbs(words[0]) {
				return "", errors.New("unknown mise T3 selection")
			}
			selected = words[0]
		}
		resolved, resolveErr = filepath.EvalSymlinks(selected)
	} else if !t3SupportedServiceArgs(args) {
		return "", errors.New("unsupported T3 service arguments")
	}
	root := filepath.Join(prefix, "lib/node_modules/t3")
	if !filepath.IsAbs(selected) {
		return "", errors.New("relative T3 service launcher")
	}
	if resolveErr != nil {
		if selected != filepath.Join(prefix, "bin/t3") || !t3Absent(root) || !t3Absent(selected) {
			return "", errors.New("unresolvable T3 service launcher")
		}
	} else if resolved != filepath.Join(root, "bin/t3.js") &&
		!(selected == filepath.Join(prefix, "bin/t3") && resolved == filepath.Join(root, "dist/bin.mjs")) &&
		resolved != filepath.Join(root, "node_modules/@t3code/t3-"+release.T3Target()+"/t3") {
		return "", errors.New("T3 service launcher is outside supported owned package")
	}
	binding, e := json.Marshal(struct {
		Command        t3UnitCommand
		Mise, Selected string
		EffectiveArgs  []string
	}{command, mise, selected, args})
	return string(binding), e
}

func t3UnitSelectsOwned(c *Context, service, prefix string) bool {
	_, e := t3UnitBinding(c, service, prefix)
	return e == nil
}
