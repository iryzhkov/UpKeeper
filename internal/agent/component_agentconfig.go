package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func init() {
	Register("agent-config", Handler{Observe: agentConfigObserve, Apply: agentConfigApply})
}

// agentConfigFiles returns the file entries of an agent-config manifest entry.
func agentConfigFiles(spec map[string]any) []map[string]any {
	entries, _ := spec["files"].([]any)
	files := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		if typed, ok := entry.(map[string]any); ok {
			files = append(files, typed)
		}
	}
	return files
}

func agentConfigObserve(c *Context, spec map[string]any) (map[string]any, error) {
	observed := map[string]any{}
	for _, entry := range agentConfigFiles(spec) {
		relative := adapterSpecString(entry, "path")
		digest := c.SHA256File(c.Path(relative))
		if digest == "" {
			// An unreadable file is reported as absent, not as a digest nobody
			// could have produced.
			observed[relative] = nil
			continue
		}
		observed[relative] = digest
	}
	return observed, nil
}

func agentConfigApply(
	c *Context, spec map[string]any, before map[string]any, actions *[]string,
) (string, error) {
	changed := false
	for _, entry := range agentConfigFiles(spec) {
		relative := adapterSpecString(entry, "path")
		wanted := adapterSpecString(entry, "sha256")
		if observed, _ := before[relative].(string); observed == wanted {
			continue
		}
		store := adapterSpecString(entry, "store")
		content, carried := c.Stored[store]
		if !carried {
			return "", fmt.Errorf("the release carries no stored content for %s", store)
		}
		payload := []byte(content)
		sum := sha256.Sum256(payload)
		if hex.EncodeToString(sum[:]) != wanted {
			return "", Failf("stored content for %s does not match its digest", relative)
		}
		if c.DryRun {
			*actions = append(*actions, "would write "+relative)
			changed = true
			continue
		}
		target := c.Path(relative)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		if _, err := os.Stat(target); err == nil {
			// The run identifier is in the backup name so that two runs never
			// overwrite each other's copy of what they replaced.
			backup := target + ".upkeeper-" + c.RunID + ".bak"
			if err := agentConfigCopy(target, backup); err != nil {
				return "", err
			}
			*actions = append(*actions,
				"backed up "+relative+" to "+filepath.Base(backup))
		}
		temporary := filepath.Join(filepath.Dir(target),
			"."+filepath.Base(target)+".upkeeper-tmp")
		if err := os.WriteFile(temporary, payload, 0o644); err != nil {
			return "", err
		}
		if err := os.Chmod(temporary, 0o644); err != nil {
			return "", err
		}
		if err := os.Rename(temporary, target); err != nil {
			return "", err
		}
		if c.SHA256File(target) != wanted {
			return "", Failf("%s did not verify after write", relative)
		}
		*actions = append(*actions, "wrote "+relative)
		changed = true
	}
	if changed {
		return "updated", nil
	}
	return "converged", nil
}

// agentConfigCopy copies a file and its permission bits, which is what the
// Python agent's shutil.copy2 did for the backup it takes before a write.
func agentConfigCopy(source, destination string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(destination, info.Mode().Perm())
}
