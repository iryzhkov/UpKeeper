package cli

import (
	"github.com/iryzhkov/UpKeeper/internal/agentenv"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// The release manifest model knows that an agent-environment component exists
// and that it has to be validated and captured, but not how. Wiring the two
// together here, once, keeps the manifest model free of the projection logic
// and keeps the projection free of the manifest's component taxonomy.
func init() {
	release.EnvironmentValidator = agentenv.ValidateEnvironment
	release.EnvironmentToolingNormalizer = agentenv.WithTooling
	release.EnvironmentCapturer = func(
		home string, provenance release.InstructionRef,
	) (map[string]any, error) {
		return agentenv.CaptureEnvironment(home, agentenv.ProjectInstructionRef{
			Repository:         provenance.Repository,
			RepositoryRevision: provenance.Commit,
			Path:               provenance.Path,
			SHA256:             provenance.SHA256,
		})
	}
}
