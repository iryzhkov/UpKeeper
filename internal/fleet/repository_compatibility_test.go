package fleet_test

import (
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/fleet"
)

func TestExistingFleetRepositorySpellingsPreserved(t *testing.T) {
	for _, repository := range []string{
		"ssh://igor@normandy/home/igor/Work/steward-tests",
		"ssh://igor@normandy/home/igor/.cache/track-s-campaign/dogfood-repo",
		"git@github.com:iryzhkov/UpKeeper.git",
		"https://github.com/iryzhkov/UpKeeper.git",
		"ssh://git@git.example.invalid:2222/igor/repo.git",
	} {
		t.Run(repository, func(t *testing.T) {
			document, err := fleet.Parse(edit(t, func(d map[string]any) { object(t, d, "projects", "upkeeper")["repository"] = repository }))
			if err != nil {
				t.Fatal(err)
			}
			if document.Projects["upkeeper"].Repository != repository {
				t.Fatal("repository rewritten")
			}
			raw, err := document.CoordinatorBytes()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), repository) {
				t.Fatal("repository absent from coordinator projection")
			}
		})
	}
}

func TestRepositoryCompatibilityStillRejectsInjectionAndSecrets(t *testing.T) {
	for _, repository := range []string{
		"ssh://igor:secret@normandy/repo",
		"https://token@github.com/owner/repo.git",
		"https://git:secret@github.com/owner/repo.git",
		"ssh://igor@normandy/repo?token=secret",
		"git@github.com:owner/repo.git#secret",
		"ssh://igor@normandy/repo\nnext",
		"ssh://igor@normandy/repo path",
		"ssh://igor@normandy/$(id)",
		"ssh://igor@normandy/repo;id",
		"-oProxyCommand=id",
		"git@github.com:-option",
		"ssh://igor@normandy/-option",
		"file:///tmp/repo",
	} {
		t.Run(repository, func(t *testing.T) {
			_, err := fleet.Parse(edit(t, func(d map[string]any) { object(t, d, "projects", "upkeeper")["repository"] = repository }))
			if err == nil {
				t.Fatal("unsafe repository accepted")
			}
		})
	}
}
