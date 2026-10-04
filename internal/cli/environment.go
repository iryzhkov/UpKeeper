package cli

import (
	"encoding/json"
	"fmt"
	"github.com/iryzhkov/UpKeeper/internal/redact"
	"path/filepath"

	"github.com/iryzhkov/UpKeeper/internal/toolcatalog"
)

// parseEnvironment precedes the legacy broad help shortcut.
func parseEnvironment(parsed *arguments, argv []string) (*arguments, error) {
	if len(argv) == 1 && isHelpFlag(argv[0]) {
		return nil, &helpRequest{command: "environment"}
	}
	if len(argv) == 2 && argv[0] == "tools" && isHelpFlag(argv[1]) {
		return nil, &helpRequest{command: "environment-tools"}
	}
	if len(argv) == 0 || argv[0] != "tools" {
		return nil, usagef("invalid environment command")
	}
	seen := map[string]bool{}
	for i := 1; i < len(argv); i++ {
		key := argv[i]
		if seen[key] {
			return nil, usagef("duplicate environment option")
		}
		seen[key] = true
		if key == "--json" {
			parsed.asJSON = true
			continue
		}
		if key != "--metadata" && key != "--root" && key != "--expect-sha256" && key != "--platform" {
			return nil, usagef("invalid environment option")
		}
		if i+1 >= len(argv) {
			return nil, usagef("missing environment option value")
		}
		i++
		value := argv[i]
		switch key {
		case "--metadata":
			parsed.catalog.Metadata = value
		case "--root":
			parsed.catalog.Root = value
		case "--expect-sha256":
			parsed.catalog.SHA256 = value
		case "--platform":
			parsed.catalog.Platform = value
		}
	}
	o := parsed.catalog
	if !filepath.IsAbs(o.Metadata) || !filepath.IsAbs(o.Root) || !toolcatalog.Pin(o.SHA256, 64) || seen["--platform"] && !toolcatalog.Platform(o.Platform) {
		return nil, usagef("invalid or missing environment arguments")
	}
	return parsed, nil
}

func environmentHelp(program, command string) string {
	if command == "environment" {
		return "usage: " + program + " environment tools [OPTIONS]\n\nInspect an operator-pinned candidate catalog offline.\nUse environment tools --help for required pins.\n"
	}
	return "usage: " + program + " environment tools --metadata ABS --root ABS --expect-sha256 HEX64\n" +
		"       [--platform linux-amd64|linux-arm64] [--json]\n\n" +
		"Verify declared local content without execution, configuration, network or writes.\n" +
		"candidate-inert; declaration-only; per-file observations, no atomic snapshot.\n" +
		"Installed/configured/authorized unknown; enforcement unqualified; readiness unassessed.\n" +
		"Linux requires procfs. Paths are explicit; the metadata pin is lowercase SHA256.\n" +
		"Platform compatibility compares declarations only. Optional missing content is visible.\n" +
		"Exit codes: 0 content verified, 2 usage, 30 refused, 1 internal/output failure.\n" +
		helpOption + "\n"
}
func runEnvironment(parsed *arguments, streams Streams) (int, error) {
	doc, err := toolcatalog.Inspect(parsed.catalog)
	code := 0
	if err != nil {
		code = 30
	}
	if parsed.asJSON {
		// Normalize typed declarations to JSON containers so existing redaction walks them.
		raw, e := json.Marshal(doc)
		if e != nil {
			return 1, fmt.Errorf("environment output failed")
		}
		var printable map[string]any
		if json.Unmarshal(raw, &printable) != nil {
			return 1, fmt.Errorf("environment output failed")
		}
		if e := printJSON(streams.Out, printable); e != nil {
			return 1, fmt.Errorf("environment output failed")
		}
		return code, nil
	}
	if err != nil {
		e, ok := err.(*toolcatalog.Error)
		if !ok {
			return 1, fmt.Errorf("environment inspection failed")
		}
		if _, writeErr := fmt.Fprintf(streams.Err, "upkeeper: catalog refused (%s/%s)\n", e.Code, e.Stage); writeErr != nil {
			return 1, fmt.Errorf("environment output failed")
		}
		return code, nil
	}
	if _, e := fmt.Fprintln(streams.Out, "candidate-inert; declaration-only; content-verified; per-file-no-atomic-snapshot\ninstalled unknown; configured unknown; authorized unknown; enforcement unqualified; readiness unassessed"); e != nil {
		return 1, fmt.Errorf("environment output failed")
	}
	for _, a := range doc["artifacts"].([]map[string]any) {
		if _, e := fmt.Fprintln(streams.Out, redact.Text(fmt.Sprintf("artifact %s: %s", a["id"], a["content_state"]))); e != nil {
			return 1, fmt.Errorf("environment output failed")
		}
	}
	for _, o := range doc["tools"].([]map[string]any) {
		t := o["declaration"].(toolcatalog.Tool)
		if _, e := fmt.Fprintln(streams.Out, redact.Text(fmt.Sprintf("%s: selected declared; content %s; platform %s; purpose %s", t.ID, o["content_state"], o["platform_compatibility"], t.Purpose))); e != nil {
			return 1, fmt.Errorf("environment output failed")
		}
	}
	return 0, nil
}
