package release

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/procx"
)

const ToolingComponent = "agent-tooling"

// AgentTools is the fixed set of tools the tooling component installs, in the
// order it names them. A host's agent tool profile is drawn from it.
var AgentTools = []string{"homelab-cli", "ov-memory", "local-file-llm"}

// CheckAgentTools validates a declared agent tool profile: it names at least
// one tool, every one of them known, and none twice.
func CheckAgentTools(tools []string) error {
	if len(tools) == 0 {
		return errors.New("must name at least one tool")
	}
	seen := map[string]bool{}
	for _, tool := range tools {
		known := false
		for _, name := range AgentTools {
			known = known || tool == name
		}
		if !known {
			return fmt.Errorf("names unknown tool %q; known tools: %s", tool, strings.Join(AgentTools, ", "))
		}
		if seen[tool] {
			return fmt.Errorf("names %q twice", tool)
		}
		seen[tool] = true
	}
	return nil
}

var toolFileName = regexp.MustCompile(`^[a-zA-Z0-9_./-]+$`)
var pythonRequirement = regexp.MustCompile(`^[A-Za-z0-9_.-]+==[A-Za-z0-9_.+-]+$`)

// Tooling carries portable source bytes, never virtualenvs, credentials or host
// configuration. Its digest identifies the immutable local runtime directory.
type Tooling struct {
	Kind        string     `json:"kind"`
	Files       []ToolFile `json:"files"`
	Python      []string   `json:"python_requirements"`
	ModelPolicy string     `json:"model_policy"`
}
type ToolFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	SHA256  string `json:"sha256"`
	Mode    int    `json:"mode"`
}

func DecodeTooling(raw map[string]any) (*Tooling, error) {
	if !hasExactKeys(raw, "kind", "files", "python_requirements", "model_policy") {
		return nil, errors.New("unknown agent-tooling fields")
	}
	b, err := canonjson.Compact(raw)
	if err != nil {
		return nil, err
	}
	var t Tooling
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&t); err != nil {
		return nil, err
	}
	if t.Kind != ToolingComponent || len(t.Files) == 0 || len(t.Files) > 512 {
		return nil, errors.New("invalid agent-tooling bundle")
	}
	seen := map[string]bool{}
	total := 0
	for _, f := range t.Files {
		if !toolFileName.MatchString(f.Path) || filepath.Clean(f.Path) != f.Path || strings.HasPrefix(f.Path, "/") || strings.Contains(f.Path, "..") || seen[f.Path] {
			return nil, errors.New("unsafe or duplicate tooling path")
		}
		if !(strings.HasPrefix(f.Path, "homelab-cli/bin/") || strings.HasPrefix(f.Path, "homelab-cli/lib/") || f.Path == "ov-memory/server.py" || f.Path == "ov-memory/friction.py" || (filepath.Dir(f.Path) == "local-file-llm" && (strings.HasSuffix(f.Path, ".mjs") || filepath.Base(f.Path) == "package.json" || filepath.Base(f.Path) == "package-lock.json"))) {
			return nil, errors.New("unknown tooling package")
		}
		if f.Mode != 0644 && f.Mode != 0755 {
			return nil, errors.New("unsafe tooling permissions")
		}
		data, err := base64.StdEncoding.Strict().DecodeString(f.Content)
		if err != nil || digestOf(data) != f.SHA256 {
			return nil, errors.New("tooling digest mismatch")
		}
		total += len(data)
		if total > 16<<20 {
			return nil, errors.New("tooling bundle too large")
		}
		seen[f.Path] = true
	}
	for _, p := range []string{"homelab-cli/bin/ha", "homelab-cli/bin/toolfeedback", "homelab-cli/lib/common.sh", "ov-memory/server.py", "ov-memory/friction.py", "local-file-llm/server.mjs", "local-file-llm/package.json", "local-file-llm/package-lock.json"} {
		if !seen[p] {
			return nil, fmt.Errorf("missing required tooling source: %s", p)
		}
	}
	if len(t.Python) == 0 || len(t.Python) > 256 {
		return nil, errors.New("missing Python dependency lock")
	}
	for _, r := range t.Python {
		if !pythonRequirement.MatchString(r) {
			return nil, errors.New("Python dependency must be an exact public package version")
		}
	}
	if t.ModelPolicy != "discover" {
		return nil, errors.New("model_policy must be discover")
	}
	return &t, nil
}

func CaptureTooling(home string, runner procx.Runner) (map[string]any, error) {
	t := Tooling{Kind: ToolingComponent, ModelPolicy: "discover"}
	// First captures use the existing installations. Subsequent captures use the
	// active managed bundle, so a copied Work tree cannot downgrade a release.
	roots := map[string]string{
		"homelab-cli":    filepath.Join(home, "Work/homelab-cli"),
		"ov-memory":      filepath.Join(home, ".local/lib/ov-mcp"),
		"local-file-llm": filepath.Join(home, "Work/mcp-local-file-llm"),
	}
	active := filepath.Join(home, ".local/share/upkeeper-tools/current")
	if _, err := os.Stat(filepath.Join(active, ".complete")); err == nil {
		for name := range roots {
			roots[name] = filepath.Join(active, name)
		}
	}
	for _, name := range []string{"homelab-cli", "ov-memory", "local-file-llm"} {
		root := roots[name]
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			if d.IsDir() {
				if rel != "." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "venv" || d.Name() == "node_modules" || d.Name() == "__pycache__") {
					return filepath.SkipDir
				}
				return nil
			}
			include := name == "homelab-cli" && (strings.HasPrefix(rel, "bin/") || strings.HasPrefix(rel, "lib/")) ||
				name == "ov-memory" && (rel == "server.py" || rel == "friction.py") ||
				name == "local-file-llm" && filepath.Dir(rel) == "." && (strings.HasSuffix(rel, ".mjs") || rel == "package.json" || rel == "package-lock.json")
			if !include {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("tool source is not a regular file: %s", rel)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			mode := 0644
			if info.Mode()&0111 != 0 {
				mode = 0755
			}
			t.Files = append(t.Files, ToolFile{Path: name + "/" + filepath.ToSlash(rel), Content: base64.StdEncoding.EncodeToString(data), SHA256: digestOf(data), Mode: mode})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("%s capture: %w", name, err)
		}
	}
	python := filepath.Join(roots["ov-memory"], "venv/bin/python")
	result := runner(procx.Command{Argv: []string{python, "-m", "pip", "freeze"}, Timeout: 30 * time.Second})
	if result.Err != nil || result.Code != 0 {
		return nil, errors.New("cannot capture OV memory Python dependency lock")
	}
	for _, line := range strings.Split(strings.TrimSpace(result.Stdout), "\n") {
		t.Python = append(t.Python, strings.TrimSpace(line))
	}
	sort.Strings(t.Python)
	sort.Slice(t.Files, func(i, j int) bool { return t.Files[i].Path < t.Files[j].Path })
	b, err := canonjson.Compact(t)
	if err != nil {
		return nil, err
	}
	raw, err := canonjson.DecodeObject(b)
	if err != nil {
		return nil, err
	}
	_, err = DecodeTooling(raw)
	return raw, err
}
