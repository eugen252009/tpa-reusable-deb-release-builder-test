package aptpackage

import (
	"fmt"
	"os"
	"path/filepath"
)

func validateDirectPackOutput(path string) error {
	if path == "" {
		return fmt.Errorf("repository output path is empty")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve repository output path: %w", err)
	}
	if absolute == string(filepath.Separator) {
		return fmt.Errorf("refusing to build directly into filesystem root")
	}
	info, err := os.Lstat(absolute)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect repository output path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("repository output path must be a real directory")
	}
	entries, err := os.ReadDir(absolute)
	if err != nil {
		return fmt.Errorf("inspect repository output directory: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("direct repository output directory must be empty; use --atomic-publish to replace a live repository")
	}
	return nil
}

// InitializeRepository creates a verified, initially empty TPA repository.
// Unlike Pack, it requires explicit distribution identity and architectures.
// The destination must be absent. Existing files, directories (including empty
// ones), and symlinks are never replaced.
func InitializeRepository(cfg Config, architectures []string) error {
	if !repositoryInitializationSupported() {
		return fmt.Errorf("safe repository initialization is unsupported on this platform")
	}
	if cfg.OutDir == "" {
		return fmt.Errorf("repository output path is empty")
	}
	if err := validateEmptyRepository(cfg, architectures); err != nil {
		return err
	}
	cfg.emptyRepositoryArchitectures = append([]string(nil), architectures...)
	live, err := filepath.Abs(cfg.OutDir)
	if err != nil {
		return fmt.Errorf("resolve repository output path: %w", err)
	}
	if live == string(filepath.Separator) {
		return fmt.Errorf("refusing to initialize filesystem root")
	}
	parent := filepath.Dir(live)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create repository parent: %w", err)
	}
	return withRepositoryLock(live, func() error {
		return initializeRepositoryLocked(cfg, live, parent)
	})
}

func initializeRepositoryLocked(cfg Config, live, parent string) error {
	if _, err := os.Lstat(live); err == nil {
		return fmt.Errorf("repository output path already exists; initialization never overwrites existing paths")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect repository output path: %w", err)
	}

	staging, err := os.MkdirTemp(parent, "."+filepath.Base(live)+".tpa-init-")
	if err != nil {
		return fmt.Errorf("create repository staging directory: %w", err)
	}
	defer os.RemoveAll(staging)
	if err := os.Chmod(staging, 0o755); err != nil {
		return fmt.Errorf("set staging permissions: %w", err)
	}

	cfg.OutDir = staging
	if err := buildRepository(cfg); err != nil {
		return err
	}
	if err := prepareRepositoryPermissions(staging); err != nil {
		return fmt.Errorf("prepare repository permissions: %w", err)
	}
	if err := VerifyTPARepositoryTree(cfg); err != nil {
		return fmt.Errorf("verify empty repository: %w", err)
	}
	if err := publishInitializedDirectory(staging, live); err != nil {
		return fmt.Errorf("publish empty repository: %w", err)
	}
	return nil
}
