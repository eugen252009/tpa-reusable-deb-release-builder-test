package aptpackage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func TestBuildRemoteTreeHasExactParentsAndChildren(t *testing.T) {
	manifest := GenerationManifest{Version: 1, RepositoryID: "fixture", GenerationID: "generation", Files: []GenerationFile{
		{Path: "dists/bookworm/Release", Size: 12},
		{Path: "dists/bookworm/main/binary-amd64/Packages", Size: 0},
		{Path: "index.html", Size: 10},
		{Path: "pool/main/example_1_all.deb", Size: 42},
		{Path: "repository.json", Size: 18},
	}}
	tree, err := buildRemoteTree(manifest)
	if err != nil {
		t.Fatal(err)
	}
	wantDirs := []string{"", "dists", "pool", "dists/bookworm", "pool/main", "dists/bookworm/main", "dists/bookworm/main/binary-amd64"}
	if !reflect.DeepEqual(tree.dirs, wantDirs) {
		t.Fatalf("directories = %#v, want %#v", tree.dirs, wantDirs)
	}
	for directory, children := range tree.children {
		for name, entry := range children {
			if entry.directory {
				continue
			}
			wantPath := name
			if directory != "" {
				wantPath = directory + "/" + name
			}
			if entry.file.Path != wantPath {
				t.Errorf("file child %s/%s has inconsistent manifest path %q", directory, name, entry.file.Path)
			}
		}
	}
}

func TestRunSFTPBatchHonorsCallerDeadline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test SFTP wrapper requires a POSIX shell")
	}
	wrapperDir := t.TempDir()
	wrapper := filepath.Join(wrapperDir, "sftp")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec sleep 10\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := runSFTPBatch(ctx, RepositoryDestination{Kind: DestinationSSH, Host: "example.invalid"}, func(_ io.Writer) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runSFTPBatch error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("runSFTPBatch ignored caller deadline for %s", elapsed)
	}
}

func TestRemotePublicationLockIsStableAndDestinationScoped(t *testing.T) {
	first := remotePublicationLockPath("/srv/repos", "stable")
	if first != remotePublicationLockPath("/srv/repos", "stable") {
		t.Fatal("lock path is not stable")
	}
	if first == remotePublicationLockPath("/srv/other", "stable") || first == remotePublicationLockPath("/srv/repos", "other") {
		t.Fatal("distinct destination shares a lock path")
	}
}
