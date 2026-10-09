//go:build !linux && !darwin && !windows

package aptpackage

import "fmt"

var syncPublishParent = func(string) error { return nil }

func repositoryInitializationSupported() bool { return false }

func publishDirectory(_, _ string) error {
	return fmt.Errorf("atomic publish is supported only on Linux")
}

func publishInitializedDirectory(_, _ string) error {
	return fmt.Errorf("safe repository initialization is unsupported on this platform")
}
