package aptpackage

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInitializeEmptyRepositoryThenAtomicallyPackFirstArtifact(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	cfg := Config{OutDir: repository, Repo: RepoConfig{
		Origin: "Fixture", Label: "Fixture", Suite: "stable", Codename: "bookworm",
		Components: "main", Description: "empty fixture",
	}}
	if err := InitializeRepository(cfg, []string{"amd64", "all"}); err != nil {
		t.Fatal(err)
	}
	if err := VerifyTPARepositoryTree(cfg); err != nil {
		t.Fatalf("verify empty repository: %v", err)
	}
	for _, architecture := range []string{"all", "amd64"} {
		index := filepath.Join(repository, "dists", "bookworm", "main", "binary-"+architecture, "Packages")
		data, err := os.ReadFile(index)
		if err != nil || len(data) != 0 {
			t.Fatalf("empty %s Packages index = %q, err=%v", architecture, data, err)
		}
		compressed, err := os.Open(index + ".gz")
		if err != nil {
			t.Fatal(err)
		}
		reader, err := gzip.NewReader(compressed)
		if err != nil {
			_ = compressed.Close()
			t.Fatal(err)
		}
		decompressed, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		fileErr := compressed.Close()
		if readErr != nil || closeErr != nil || fileErr != nil || len(decompressed) != 0 {
			t.Fatalf("empty compressed index invalid: data=%q read=%v close=%v file=%v", decompressed, readErr, closeErr, fileErr)
		}
	}
	var index repositoryBrowserIndex
	browserData, err := os.ReadFile(filepath.Join(repository, "repository.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(browserData, &index); err != nil {
		t.Fatal(err)
	}
	if index.Format != RepositoryBrowserFormat || index.Version != RepositoryBrowserVersion || index.Packages == nil || len(index.Packages) != 0 {
		t.Fatalf("unexpected empty repository sidecar: %+v", index)
	}

	input := filepath.Join(root, "packages")
	buildTestDeb(t, input, "fixture_1.0_all.deb", basicControl("fixture", "1.0", "all"), "first package")
	cfg.InDir = input
	if err := AtomicPack(cfg, repository); err != nil {
		t.Fatalf("pack first artifact into initialized repository format: %v", err)
	}
	if err := VerifyTPARepositoryTree(cfg); err != nil {
		t.Fatalf("verify populated repository: %v", err)
	}
	packages, err := os.ReadFile(filepath.Join(repository, "dists", "bookworm", "main", "binary-all", "Packages"))
	if err != nil || !bytes.Contains(packages, []byte("Package: fixture\n")) {
		t.Fatalf("first package was not indexed: err=%v data=%q", err, packages)
	}
}

func TestInitializeRepositoryIsReproducibleWithSourceDateEpoch(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "1700000000")
	cfg := Config{Repo: RepoConfig{Suite: "stable", Codename: "stable", Components: "main"}}
	var snapshots [][]byte
	for _, name := range []string{"one", "two"} {
		cfg.OutDir = filepath.Join(t.TempDir(), name)
		if err := InitializeRepository(cfg, []string{"amd64"}); err != nil {
			t.Fatal(err)
		}
		var snapshot bytes.Buffer
		for _, rel := range []string{
			"dists/stable/Release", "dists/stable/main/binary-amd64/Packages.gz",
			"repository.json", "index.html",
		} {
			data, err := os.ReadFile(filepath.Join(cfg.OutDir, rel))
			if err != nil {
				t.Fatal(err)
			}
			snapshot.WriteString(rel)
			snapshot.WriteByte(0)
			snapshot.Write(data)
			snapshot.WriteByte(0)
		}
		snapshots = append(snapshots, snapshot.Bytes())
	}
	if !bytes.Equal(snapshots[0], snapshots[1]) {
		t.Fatal("empty repository metadata changed with a fixed SOURCE_DATE_EPOCH")
	}
}

func TestInitializeRepositoryRefusesExistingPathsWithoutMutation(t *testing.T) {
	root := t.TempDir()
	cfg := Config{Repo: RepoConfig{Suite: "stable", Codename: "stable", Components: "main"}}
	for _, test := range []struct {
		name string
		path func() string
	}{
		{name: "empty directory", path: func() string {
			p := filepath.Join(root, "empty")
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatal(err)
			}
			return p
		}},
		{name: "nonempty directory", path: func() string {
			p := filepath.Join(root, "nonempty")
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(p, "keep"), []byte("safe"), 0o644); err != nil {
				t.Fatal(err)
			}
			return p
		}},
		{name: "regular file", path: func() string {
			p := filepath.Join(root, "file")
			if err := os.WriteFile(p, []byte("safe"), 0o644); err != nil {
				t.Fatal(err)
			}
			return p
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg.OutDir = test.path()
			if err := InitializeRepository(cfg, []string{"all"}); err == nil {
				t.Fatal("initialization unexpectedly replaced an existing path")
			}
			if test.name == "nonempty directory" {
				data, err := os.ReadFile(filepath.Join(cfg.OutDir, "keep"))
				if err != nil || string(data) != "safe" {
					t.Fatalf("existing file changed: data=%q err=%v", data, err)
				}
			}
			if test.name == "empty directory" {
				entries, err := os.ReadDir(cfg.OutDir)
				if err != nil || len(entries) != 0 {
					t.Fatalf("existing empty directory changed: entries=%v err=%v", entries, err)
				}
			}
		})
	}
}

func TestInitializeRepositoryRequiresExplicitSafeFormat(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct {
		name string
		repo RepoConfig
		arch []string
	}{
		{name: "missing suite", repo: RepoConfig{Codename: "stable", Components: "main"}, arch: []string{"all"}},
		{name: "missing codename", repo: RepoConfig{Suite: "stable", Components: "main"}, arch: []string{"all"}},
		{name: "path component", repo: RepoConfig{Suite: "stable", Codename: "stable", Components: "../escape"}, arch: []string{"all"}},
		{name: "duplicate architecture", repo: RepoConfig{Suite: "stable", Codename: "stable", Components: "main"}, arch: []string{"all", "all"}},
		{name: "unsafe architecture", repo: RepoConfig{Suite: "stable", Codename: "stable", Components: "main"}, arch: []string{"../escape"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{OutDir: filepath.Join(root, strings.ReplaceAll(test.name, " ", "-")), Repo: test.repo}
			if err := InitializeRepository(cfg, test.arch); err == nil {
				t.Fatal("invalid empty repository configuration was accepted")
			}
			if _, err := os.Lstat(cfg.OutDir); !os.IsNotExist(err) {
				t.Fatalf("invalid request created output path: %v", err)
			}
		})
	}
	if _, err := repositoryReleaseDate(); err != nil {
		t.Fatalf("unexpected default date failure: %v", err)
	}
	t.Setenv("SOURCE_DATE_EPOCH", "invalid")
	if _, err := repositoryReleaseDate(); err == nil {
		t.Fatal("invalid SOURCE_DATE_EPOCH was accepted")
	}
}

func TestPackRejectsSymlinkArtifactInput(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "packages")
	realArtifact := buildTestDeb(t, input, "fixture_1.0_all.deb", basicControl("fixture", "1.0", "all"), "fixture")
	if err := os.Symlink(realArtifact, filepath.Join(input, "linked.deb")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	out := filepath.Join(root, "repo")
	if err := Pack(Config{InDir: input, OutDir: out}); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("Pack accepted a symlink package input: %v", err)
	}
	if _, err := os.Lstat(out); !os.IsNotExist(err) {
		t.Fatalf("symlink input created repository output: %v", err)
	}
}

func TestVerifyTPARepositoryTreeAllowsRetainedUnindexedPoolArtifact(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "packages")
	buildTestDeb(t, input, "fixture_1.0_all.deb", basicControl("fixture", "1.0", "all"), "fixture")
	repository := filepath.Join(root, "repo")
	cfg := Config{InDir: input, OutDir: repository}
	if err := Pack(cfg); err != nil {
		t.Fatal(err)
	}
	orphanInput := filepath.Join(root, "orphan-input")
	orphan := buildTestDeb(t, orphanInput, "orphan_1.0_all.deb", basicControl("orphan", "1.0", "all"), "orphan")
	orphanPath := filepath.Join(repository, "pool", "main", "o", "orphan", filepath.Base(orphan))
	if err := os.MkdirAll(filepath.Dir(orphanPath), 0o755); err != nil {
		t.Fatal(err)
	}
	artifact, err := os.ReadFile(orphan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphanPath, artifact, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyTPARepositoryTree(cfg); err != nil {
		t.Fatalf("strict verifier rejected a valid retained pool artifact: %v", err)
	}
}

func TestVerifyTPARepositoryTreeRejectsBrowserIndexMismatch(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "packages")
	buildTestDeb(t, input, "fixture_1.0_all.deb", basicControl("fixture", "1.0", "all"), "fixture")
	cfg := Config{InDir: input, OutDir: filepath.Join(root, "repo")}
	if err := Pack(cfg); err != nil {
		t.Fatal(err)
	}
	browserPath := filepath.Join(cfg.OutDir, "repository.json")
	if err := os.WriteFile(browserPath, []byte(`{"format":"tpa-repository-index","version":1,"packages":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyRepository(cfg); err != nil {
		t.Fatalf("generic APT verification must not depend on browser files: %v", err)
	}
	if err := VerifyTPARepositoryTree(cfg); err == nil || !strings.Contains(err.Error(), "sidecars") {
		t.Fatalf("strict TPA format verification accepted mismatched browser data: %v", err)
	}
	if err := writeRepositoryBrowserFiles(cfg.OutDir, cfg.Repo); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.OutDir, "untrusted.txt"), []byte("extra"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyTPARepositoryTree(cfg); err == nil || !strings.Contains(err.Error(), "unexpected entries") {
		t.Fatalf("strict TPA format verification accepted an arbitrary root file: %v", err)
	}
}

func TestPackRejectsNonemptyDirectOutputAndUnsafeRepositoryPaths(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "packages")
	buildTestDeb(t, input, "fixture_1.0_all.deb", basicControl("fixture", "1.0", "all"), "fixture")
	out := filepath.Join(root, "existing")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(out, "keep")
	if err := os.WriteFile(marker, []byte("unchanged"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Pack(Config{InDir: input, OutDir: out}); err == nil {
		t.Fatal("Pack accepted nonempty direct output")
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "unchanged" {
		t.Fatalf("Pack changed existing output: %q, %v", data, err)
	}
	for _, repo := range []RepoConfig{
		{Codename: "../escape", Components: "main"},
		{Codename: "stable", Components: "../escape"},
		{Codename: "stable", Components: "main", Origin: "safe\nInjected: yes"},
	} {
		cfg := Config{InDir: input, OutDir: filepath.Join(root, "unsafe"), Repo: repo}
		if err := Pack(cfg); err == nil {
			t.Errorf("Pack accepted unsafe repository configuration: %+v", repo)
		}
		if _, err := os.Lstat(cfg.OutDir); !os.IsNotExist(err) {
			t.Errorf("unsafe repository configuration created output: %v", err)
		}
	}

	badInput := filepath.Join(root, "bad-filenames")
	buildTestDeb(t, badInput, "fixture\nInjected: yes.deb", basicControl("fixture", "1.0", "all"), "fixture")
	badOutput := filepath.Join(root, "bad-output")
	if err := Pack(Config{InDir: badInput, OutDir: badOutput}); err == nil || !strings.Contains(err.Error(), "artifact basename") {
		t.Fatalf("Pack accepted a control-character artifact basename: %v", err)
	}
	if _, err := os.Lstat(badOutput); !os.IsNotExist(err) {
		t.Fatalf("invalid artifact basename created output: %v", err)
	}
}

func TestAtomicPackClassifiesReplacedTreeCleanupFailure(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("atomic publication failure seam requires Linux")
	}
	root := t.TempDir()
	live := filepath.Join(root, "repo")
	oldInput := filepath.Join(root, "old-packages")
	buildTestDeb(t, oldInput, "fixture_0.9_all.deb", basicControl("fixture", "0.9", "all"), "old")
	if err := Pack(Config{InDir: oldInput, OutDir: live}); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(root, "packages")
	buildTestDeb(t, input, "fixture_1.0_all.deb", basicControl("fixture", "1.0", "all"), "new")
	originalRemove := removeReplacedDirectory
	removeReplacedDirectory = func(string) error { return errors.New("injected replaced-tree cleanup failure") }
	defer func() { removeReplacedDirectory = originalRemove }()

	err := AtomicPack(Config{InDir: input}, live)
	var publishErr *AtomicPublishError
	if !errors.As(err, &publishErr) || !publishErr.Published || publishErr.Stage != AtomicPublishStageCleanupReplacedTree {
		t.Fatalf("expected a classified post-activation cleanup error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(live, "dists", "stable", "Release")); err != nil {
		t.Fatalf("new repository is not active despite Published=true: %v", err)
	}
	staging, err := filepath.Glob(filepath.Join(root, ".repo.tpa-staging-*"))
	if err != nil || len(staging) != 1 {
		t.Fatalf("replaced tree was not retained for cleanup recovery: %v %v", staging, err)
	}
	if _, err := os.Stat(filepath.Join(staging[0], "pool", "main", "f", "fixture", "fixture_0.9_all.deb")); err != nil {
		t.Fatalf("retained replaced tree is unavailable: %v", err)
	}
}

func TestAtomicPackClassifiesFailureAfterActivation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("atomic publication failure seam requires Linux")
	}
	root := t.TempDir()
	live := filepath.Join(root, "repo")
	oldInput := filepath.Join(root, "old-packages")
	buildTestDeb(t, oldInput, "fixture_0.9_all.deb", basicControl("fixture", "0.9", "all"), "old")
	if err := Pack(Config{InDir: oldInput, OutDir: live}); err != nil {
		t.Fatal(err)
	}
	oldArtifact := filepath.Join(live, "pool", "main", "f", "fixture", "fixture_0.9_all.deb")
	input := filepath.Join(root, "packages")
	buildTestDeb(t, input, "fixture_1.0_all.deb", basicControl("fixture", "1.0", "all"), "new")
	originalSync := syncPublishParent
	syncPublishParent = func(string) error { return errors.New("injected directory sync failure") }
	defer func() { syncPublishParent = originalSync }()

	err := AtomicPack(Config{InDir: input}, live)
	var publishErr *AtomicPublishError
	if !errors.As(err, &publishErr) || !publishErr.Published || publishErr.Stage != AtomicPublishStageSyncParent {
		t.Fatalf("expected a classified post-activation sync error, got %v", err)
	}
	if !strings.Contains(publishErr.Error(), "was activated") {
		t.Fatalf("post-activation diagnostic is unclear: %v", publishErr)
	}
	if _, err := os.Stat(filepath.Join(live, "dists", "stable", "Release")); err != nil {
		t.Fatalf("new repository is not active despite Published=true: %v", err)
	}
	if _, err := os.Stat(oldArtifact); !os.IsNotExist(err) {
		t.Fatalf("old repository remains active after exchange: %v", err)
	}
}
