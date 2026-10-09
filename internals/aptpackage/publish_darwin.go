//go:build darwin

package aptpackage

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

var syncPublishParent = syncDarwinDirectory

func repositoryInitializationSupported() bool { return true }

func publishDirectory(_, _ string) error {
	return fmt.Errorf("atomic publish is supported only on Linux")
}

func publishInitializedDirectory(staging, live string) error {
	if err := unix.RenameatxNp(unix.AT_FDCWD, staging, unix.AT_FDCWD, live, unix.RENAME_EXCL); err != nil {
		return err
	}
	if err := syncPublishParent(filepath.Dir(live)); err != nil {
		return &AtomicPublishError{Path: live, Stage: "sync-parent", Published: true, Err: err}
	}
	return nil
}

func syncDarwinDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
