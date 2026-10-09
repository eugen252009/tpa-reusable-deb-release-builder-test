//go:build windows

package aptpackage

import (
	"fmt"

	"golang.org/x/sys/windows"
)

var syncPublishParent = func(string) error { return nil }

func repositoryInitializationSupported() bool { return true }

func publishDirectory(_, _ string) error {
	return fmt.Errorf("atomic publish is supported only on Linux")
}

func publishInitializedDirectory(staging, live string) error {
	from, err := windows.UTF16PtrFromString(staging)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(live)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH)
}
