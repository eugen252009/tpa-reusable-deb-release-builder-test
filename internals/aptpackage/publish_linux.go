//go:build linux

package aptpackage

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

var syncPublishParent = syncDirectory

func repositoryInitializationSupported() bool { return true }

func publishDirectory(staging, live string) error {
	info, err := os.Lstat(live)
	if os.IsNotExist(err) {
		if err := unix.Renameat2(unix.AT_FDCWD, staging, unix.AT_FDCWD, live, unix.RENAME_NOREPLACE); err != nil {
			return err
		}
		if err := syncPublishParent(filepathDir(live)); err != nil {
			return &AtomicPublishError{Path: live, Stage: "sync-parent", Published: true, Err: err}
		}
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("live path is not a real directory")
	}
	// renameat2(RENAME_EXCHANGE) swaps two directory names without exposing a
	// missing or partially populated live path. Both paths are siblings.
	if err := unix.Renameat2(unix.AT_FDCWD, staging, unix.AT_FDCWD, live, unix.RENAME_EXCHANGE); err != nil {
		return fmt.Errorf("rename exchange: %w", err)
	}
	if err := syncPublishParent(filepathDir(live)); err != nil {
		return &AtomicPublishError{Path: live, Stage: "sync-parent", Published: true, Err: err}
	}
	return nil
}

func publishInitializedDirectory(staging, live string) error {
	if err := unix.Renameat2(unix.AT_FDCWD, staging, unix.AT_FDCWD, live, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	if err := syncPublishParent(filepathDir(live)); err != nil {
		return &AtomicPublishError{Path: live, Stage: "sync-parent", Published: true, Err: err}
	}
	return nil
}

func filepathDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}

func syncDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
