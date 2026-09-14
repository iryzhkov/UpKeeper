package cli

import (
	"os"
	"path/filepath"

	"github.com/iryzhkov/UpKeeper/internal/failure"
)

// ConfigRootEnvironment overrides where the fleet configuration is read from.
// The name is kept from the Python implementation so that existing unit files
// and operator habits keep working.
const ConfigRootEnvironment = "DEV_FLEET_CONFIG_ROOT"

// ConfigRoot resolves where inventory/hosts.yml and desired/components.yml live.
//
// A compiled binary cannot find its source tree the way the Python package
// could, so the candidates are explicit: an operator override, the working
// directory when it is a checkout, the installed checkout, and finally a
// configuration-only installation. Resolution is deliberate rather than
// searched, because converging a host from the wrong inventory is the worst
// mistake this program can make.
func ConfigRoot() (string, error) {
	candidates, err := rootCandidates()
	if err != nil {
		return "", err
	}
	for _, candidate := range candidates {
		if hasFleetConfiguration(candidate) {
			return candidate, nil
		}
	}
	return "", failure.Validation(
		"fleet configuration unavailable; install inventory/hosts.yml and " +
			"desired/components.yml under ~/.config/dev-fleet or set " + ConfigRootEnvironment)
}

// CheckoutRoot resolves the release checkout that `pull --self` and `rollback`
// read immutable release intent out of. It must be a Git checkout, because
// release history is what those commands are reading.
func CheckoutRoot() (string, error) {
	candidates, err := rootCandidates()
	if err != nil {
		return "", err
	}
	for _, candidate := range candidates {
		if hasFleetConfiguration(candidate) && isGitCheckout(candidate) {
			return candidate, nil
		}
	}
	return "", failure.Validation(
		"no UpKeeper release checkout found; --self and rollback read release " +
			"intent from Git history, so they need the checkout, not just its files")
}

func rootCandidates() ([]string, error) {
	candidates := []string{}
	if override := os.Getenv(ConfigRootEnvironment); override != "" {
		if !filepath.IsAbs(override) {
			return nil, failure.Validation(
				ConfigRootEnvironment + " must be an absolute path")
		}
		candidates = append(candidates, filepath.Clean(override))
	}
	if working, err := os.Getwd(); err == nil {
		candidates = append(candidates, working)
	}
	// A binary installed inside a checkout, for example <checkout>/bin/upkeeper,
	// should find the checkout it came from before any installed copy.
	if executable, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(executable); err == nil {
			executable = resolved
		}
		candidates = append(candidates, filepath.Dir(filepath.Dir(executable)))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".local", "share", "dev-fleet"),
			filepath.Join(home, ".config", "dev-fleet"),
		)
	}
	return candidates, nil
}

func hasFleetConfiguration(root string) bool {
	return isRegular(filepath.Join(root, "inventory", "hosts.yml")) &&
		isRegular(filepath.Join(root, "desired", "components.yml"))
}

func isGitCheckout(root string) bool {
	_, err := os.Stat(filepath.Join(root, ".git"))
	return err == nil
}

func isRegular(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
