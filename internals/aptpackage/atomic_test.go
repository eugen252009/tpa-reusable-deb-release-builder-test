package aptpackage

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAtomicPackLeavesLiveRepositoryOnBuildFailure(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "repo")
	oldInput := filepath.Join(root, "old-packages")
	oldArtifact := buildTestDeb(t, oldInput, "fixture_0.9_all.deb", basicControl("fixture", "0.9", "all"), "previous release")
	cfg := Config{InDir: oldInput, OutDir: live, Repo: RepoConfig{Codename: "stable", Components: "main"}}
	if err := Pack(cfg); err != nil {
		t.Fatal(err)
	}
	releasePath := filepath.Join(live, "dists", "stable", "Release")
	before, err := os.ReadFile(releasePath)
	if err != nil {
		t.Fatal(err)
	}

	err = AtomicPack(Config{InDir: filepath.Join(root, "missing"), Repo: cfg.Repo}, live)
	if err == nil {
		t.Fatal("expected build failure")
	}
	after, readErr := os.ReadFile(releasePath)
	if readErr != nil || string(after) != string(before) {
		t.Fatalf("live repository changed after failure: %v", readErr)
	}
	if _, err := os.Stat(oldArtifact); err != nil {
		t.Fatalf("previous package artifact changed after failure: %v", err)
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".repo.tpa-staging-") {
			t.Errorf("staging directory was not cleaned: %s", entry.Name())
		}
	}
}

func TestAtomicPackRefusesArbitraryExistingDirectory(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "repo")
	if err := os.Mkdir(live, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(live, "keep-me")
	if err := os.WriteFile(marker, []byte("not a repository"), 0o644); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(root, "packages")
	buildTestDeb(t, input, "fixture_1.0_all.deb", basicControl("fixture", "1.0", "all"), "fixture")
	if err := AtomicPack(Config{InDir: input}, live); err == nil || !strings.Contains(err.Error(), "refusing to replace") {
		t.Fatalf("AtomicPack accepted an arbitrary existing directory: %v", err)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "not a repository" {
		t.Fatalf("arbitrary live file changed: %v", err)
	}
}

func TestAtomicPackSuccessfullyReplacesAndCleansLiveRepository(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("successful atomic publication requires Linux")
	}
	root := t.TempDir()
	live := filepath.Join(root, "repo")
	oldInput := filepath.Join(root, "old-packages")
	buildTestDeb(t, oldInput, "fixture_0.9_all.deb", basicControl("fixture", "0.9", "all"), "previous repository")
	if err := Pack(Config{InDir: oldInput, OutDir: live, Repo: RepoConfig{Codename: "stable", Components: "main"}}); err != nil {
		t.Fatal(err)
	}
	oldArtifact := filepath.Join(live, "pool", "main", "f", "fixture", "fixture_0.9_all.deb")
	input := filepath.Join(root, "packages")
	buildTestDeb(t, input, "fixture_1.0_all.deb", basicControl("fixture", "1.0", "all"), "new repository")
	newArtifact := filepath.Join(live, "pool", "main", "f", "fixture", "fixture_1.0_all.deb")

	stop := make(chan struct{})
	observationErrors := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := os.Stat(oldArtifact); err != nil {
				if !os.IsNotExist(err) {
					select {
					case observationErrors <- err:
					default:
					}
					return
				}
				for _, path := range []string{filepath.Join(live, "dists", "stable", "Release"), newArtifact} {
					if _, pathErr := os.Stat(path); pathErr != nil {
						select {
						case observationErrors <- fmt.Errorf("live tree was neither complete old nor complete new repository: %w", pathErr):
						default:
						}
						return
					}
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()

	err := AtomicPack(Config{InDir: input, Repo: RepoConfig{Codename: "stable", Components: "main"}}, live)
	close(stop)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-observationErrors:
		t.Fatal(err)
	default:
	}
	if _, err := os.Stat(oldArtifact); !os.IsNotExist(err) {
		t.Fatalf("old live package artifact remains after replacement: %v", err)
	}
	if _, err := os.Stat(newArtifact); err != nil {
		t.Fatalf("new live package artifact is missing: %v", err)
	}
	packages := filepath.Join(live, "dists", "stable", "main", "binary-all", "Packages")
	if _, err := os.Stat(packages); err != nil {
		t.Fatalf("new repository is not live: %v", err)
	}
	for path, wantMode := range map[string]os.FileMode{live: 0o755, packages: 0o644} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != wantMode {
			t.Errorf("%s mode is %o, want %o", path, mode, wantMode)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".repo.tpa-staging-") {
			t.Errorf("replaced staging tree was not cleaned: %s", entry.Name())
		}
	}
}
