package aptpackage

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	AtomicPublishStageSyncParent          = "sync-parent"
	AtomicPublishStageCleanupReplacedTree = "cleanup-replaced-tree"
)

// AtomicPublishError reports a failure after the new repository tree became
// the live path. Published is true; callers must inspect the live path before
// retrying. Stage identifies the post-activation step that failed.
type AtomicPublishError struct {
	Path      string
	Stage     string
	Published bool
	Err       error
}

func (err *AtomicPublishError) Error() string {
	switch err.Stage {
	case AtomicPublishStageSyncParent:
		return fmt.Sprintf("repository was activated at %s, but syncing its parent directory failed: %v", err.Path, err.Err)
	case AtomicPublishStageCleanupReplacedTree:
		return fmt.Sprintf("repository was activated at %s, but cleanup of the replaced tree failed: %v", err.Path, err.Err)
	default:
		return fmt.Sprintf("repository was activated at %s, but post-publication step %q failed: %v", err.Path, err.Stage, err.Err)
	}
}

func (err *AtomicPublishError) Unwrap() error { return err.Err }

var removeReplacedDirectory = os.RemoveAll

// AtomicPack refuses to replace an invalid or non-TPA live directory, builds
// into a sibling directory, verifies the complete candidate, and publishes it
// with a same-filesystem Linux rename exchange. Its sibling lock serializes TPA
// atomic-pack and lifecycle writers for the same live path.
func AtomicPack(cfg Config, livePath string) error {
	if livePath == "" {
		return fmt.Errorf("atomic publish path is empty")
	}
	live, err := filepath.Abs(livePath)
	if err != nil {
		return fmt.Errorf("resolve publish path: %w", err)
	}
	if live == string(filepath.Separator) {
		return fmt.Errorf("refusing to publish over filesystem root")
	}
	parent := filepath.Dir(live)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create publish parent: %w", err)
	}
	return withRepositoryLock(live, func() error {
		return atomicPackLocked(cfg, live, parent)
	})
}

func verifyAtomicReplacementTarget(cfg Config, live string) error {
	info, err := os.Lstat(live)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect live repository: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("refusing to replace a path that is not a real repository directory")
	}
	if err := validateRepositoryTree(live); err != nil {
		return fmt.Errorf("refusing to replace an invalid live repository tree: %w", err)
	}
	oldConfig := cfg
	oldConfig.OutDir = live
	codename := oldConfig.Repo.Codename
	if codename == "" {
		codename = "stable"
	}
	distRoot := filepath.Join(live, "dists", codename)
	releaseData, err := readRepositoryTreeFile(filepath.Join(distRoot, "Release"), maxRepositoryReleaseBytes)
	if err != nil {
		return fmt.Errorf("refusing to replace a destination without a valid TPA Release: %w", err)
	}
	fields, _, err := parseRepositoryRelease(releaseData)
	if err != nil {
		return fmt.Errorf("refusing to replace a destination with an invalid TPA Release: %w", err)
	}
	oldConfig.Repo.Codename = codename
	oldConfig.Repo.Components = fields["components"]
	oldConfig.Repo.Origin = fields["origin"]
	oldConfig.Repo.Label = fields["label"]
	oldConfig.Repo.Suite = fields["suite"]
	oldConfig.Repo.Description = fields["description"]
	if _, err := os.Lstat(filepath.Join(distRoot, "InRelease")); err == nil {
		if cfg.GPG == "" {
			return fmt.Errorf("live repository is signed; supply its current signing key before atomic replacement")
		}
	} else if os.IsNotExist(err) {
		oldConfig.GPG = ""
	} else {
		return fmt.Errorf("inspect live repository signature: %w", err)
	}
	if err := VerifyTPARepositoryTree(oldConfig); err != nil {
		return fmt.Errorf("refusing to replace an invalid live repository: %w", err)
	}
	return nil
}

func atomicPackLocked(cfg Config, live, parent string) error {
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create publish parent: %w", err)
	}
	if err := verifyAtomicReplacementTarget(cfg, live); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(parent, "."+filepath.Base(live)+".tpa-staging-")
	if err != nil {
		return fmt.Errorf("create staging directory: %w", err)
	}
	if err := os.Chmod(staging, 0o755); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("set staging permissions: %w", err)
	}
	cleanupStaging := true
	defer func() {
		if cleanupStaging {
			_ = os.RemoveAll(staging)
		}
	}()

	cfg.OutDir = staging
	if err := buildRepository(cfg); err != nil {
		return err
	}
	if err := prepareRepositoryPermissions(staging); err != nil {
		return fmt.Errorf("prepare repository permissions: %w", err)
	}
	if err := VerifyTPARepositoryTree(cfg); err != nil {
		return fmt.Errorf("verify staging repository: %w", err)
	}
	if err := publishDirectory(staging, live); err != nil {
		return fmt.Errorf("publish repository: %w", err)
	}
	cleanupStaging = false
	if _, err := os.Lstat(staging); err == nil {
		if err := removeReplacedDirectory(staging); err != nil {
			return &AtomicPublishError{Path: live, Stage: AtomicPublishStageCleanupReplacedTree, Published: true, Err: err}
		}
	} else if !os.IsNotExist(err) {
		return &AtomicPublishError{Path: live, Stage: AtomicPublishStageCleanupReplacedTree, Published: true, Err: err}
	}
	return nil
}

// Repository data contains no signing secrets and is made read-only/readable
// for serving processes. GPG homes are outside this tree and remain private.
func prepareRepositoryPermissions(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("repository contains symlink %s", path)
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("repository contains non-regular file %s", path)
		}
		return os.Chmod(path, 0o644)
	})
}

func startsWithBinary(name string) bool {
	return len(name) > len("binary-") && name[:len("binary-")] == "binary-"
}
