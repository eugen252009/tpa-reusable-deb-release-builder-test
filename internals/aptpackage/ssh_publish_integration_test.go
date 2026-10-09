package aptpackage

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestSSHOutputFailureBoundaries is opt-in and is run only by the disposable
// OpenSSH/SFTP qualification harness. It injects failures at real SFTP phase
// boundaries; ordinary unit-test runs never contact a remote host.
func TestSSHOutputFailureBoundaries(t *testing.T) {
	if os.Getenv("TPA_SSH_OUTPUT_INTEGRATION") != "1" {
		t.Skip("requires the disposable SSH output qualification server")
	}
	baseURL := os.Getenv("TPA_SSH_OUTPUT_BASE_URL")
	configPath := os.Getenv("TPA_SSH_OUTPUT_CONFIG")
	if baseURL == "" || configPath == "" {
		t.Fatal("TPA_SSH_OUTPUT_BASE_URL and TPA_SSH_OUTPUT_CONFIG are required")
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	parent := base.Path
	if parent == "." || parent == "/" {
		t.Fatalf("integration base URL must be a child of a disposable parent: %s", baseURL)
	}

	t.Run("cancel-before-upload-cleans-candidate", func(t *testing.T) {
		destination := integrationDestination(t, base, "cancel-"+testToken(t))
		cfg := integrationEmptyCandidate(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		err := PublishRepositoryToSSHWithProgress(ctx, cfg, destination, func(phase RemotePublishPhase) {
			if phase == RemotePhaseUpload {
				cancel()
			}
		})
		assertRemoteNotPublished(t, destination, err, RemoteNotActivated)
		assertNoRemoteCandidateArtifacts(t, destination, parent)
	})

	t.Run("interrupted-upload-cleans-partial-candidate", func(t *testing.T) {
		destination := integrationDestination(t, base, "interrupted-"+testToken(t))
		cfg := integrationEmptyCandidate(t)
		wrapperDir := t.TempDir()
		realSFTP, err := exec.LookPath("sftp")
		if err != nil {
			t.Fatal(err)
		}
		wrapper := filepath.Join(wrapperDir, "sftp")
		contents := fmt.Sprintf(`#!/bin/sh
batch=$(mktemp) || exit 90
cat >"$batch" || exit 91
if grep -q '^put ' "$batch"; then
  awk '/^put / { print; exit }' "$batch" | %s "$@"
  status=$?
  rm -f "$batch"
  [ "$status" -eq 0 ] || exit "$status"
  exit 255
fi
%s "$@" <"$batch"
status=$?
rm -f "$batch"
exit "$status"
`, shellQuote(realSFTP), shellQuote(realSFTP))
		if err := os.WriteFile(wrapper, []byte(contents), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		err = PublishRepositoryToSSH(context.Background(), cfg, destination)
		assertRemoteNotPublished(t, destination, err, RemoteNotActivated)
		assertNoRemoteCandidateArtifacts(t, destination, parent)
	})

	t.Run("corrupted-upload-is-rejected-and-cleaned", func(t *testing.T) {
		destination := integrationDestination(t, base, "corrupt-"+testToken(t))
		cfg := integrationEmptyCandidate(t)
		var injectErr error
		err := PublishRepositoryToSSHWithProgress(context.Background(), cfg, destination, func(phase RemotePublishPhase) {
			if phase == RemotePhaseVerifyUpload {
				injectErr = integrationRemoteShell(t, destination, fmt.Sprintf("stage=$(find %s -mindepth 1 -maxdepth 1 -type d -name '.tpa-upload-*' -print -quit); test -n \"$stage\" && printf X | dd of=\"$stage/index.html\" bs=1 seek=0 conv=notrunc status=none", shellQuote(parent)))
			}
		})
		if injectErr != nil {
			t.Fatalf("inject uploaded-byte corruption: %v", injectErr)
		}
		assertRemoteNotPublished(t, destination, err, RemoteNotActivated)
		if err == nil || !strings.Contains(err.Error(), "differ from the local candidate") {
			t.Fatalf("corruption was not detected by remote readback: %v", err)
		}
		assertNoRemoteCandidateArtifacts(t, destination, parent)
	})

	t.Run("target-appearing-at-activation-is-preserved", func(t *testing.T) {
		destination := integrationDestination(t, base, "appeared-"+testToken(t))
		cfg := integrationEmptyCandidate(t)
		finalPath := integrationFinalPath(destination)
		if destination.PathMode == SSHPathHomeRelative {
			t.Fatalf("integration destination must be absolute")
		}
		var injectErr error
		err := PublishRepositoryToSSHWithProgress(context.Background(), cfg, destination, func(phase RemotePublishPhase) {
			if phase == RemotePhaseActivate {
				injectErr = integrationRemoteShell(t, destination, fmt.Sprintf("mkdir -p %s && printf protected > %s/marker", shellQuote(finalPath), shellQuote(finalPath)))
			}
		})
		if injectErr != nil {
			t.Fatalf("inject competing destination: %v", injectErr)
		}
		var publishErr *RemotePublishError
		if err == nil || !errorsAs(err, &publishErr) || publishErr.Activation != RemoteActivationUnknown {
			t.Fatalf("appearing destination did not report ambiguous activation: %v", err)
		}
		if err := integrationRemoteShell(t, destination, "test \"$(cat "+shellQuote(finalPath)+"/marker)\" = protected"); err != nil {
			t.Fatalf("competing destination was modified: %v", err)
		}
		cleanupIntegrationStageAndLock(t, destination, parent)
	})

	t.Run("lost-rename-ack-is-not-reported-as-success", func(t *testing.T) {
		destination := integrationDestination(t, base, "lost-ack-"+testToken(t))
		cfg := integrationEmptyCandidate(t)
		wrapperDir := t.TempDir()
		realSFTP, err := exec.LookPath("sftp")
		if err != nil {
			t.Fatal(err)
		}
		wrapper := filepath.Join(wrapperDir, "sftp")
		contents := fmt.Sprintf("#!/bin/sh\nbatch=$(mktemp) || exit 90\ncat >\"$batch\" || exit 91\nTPA_REAL_SFTP=%s\n\"$TPA_REAL_SFTP\" \"$@\" <\"$batch\"\nstatus=$?\nif [ $status -eq 0 ] && grep -q '^rename ' \"$batch\"; then rm -f \"$batch\"; exit 255; fi\nrm -f \"$batch\"\nexit $status\n", shellQuote(realSFTP))
		if err := os.WriteFile(wrapper, []byte(contents), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		err = PublishRepositoryToSSH(context.Background(), cfg, destination)
		var publishErr *RemotePublishError
		if err == nil || !errorsAs(err, &publishErr) || publishErr.Activation != RemoteActivationUnknown {
			t.Fatalf("lost activation acknowledgement did not report uncertainty: %v", err)
		}
		if err := integrationRemoteShell(t, destination, "test -f "+shellQuote(integrationFinalPath(destination)+"/index.html")); err != nil {
			t.Fatalf("renamed repository was not independently present: %v", err)
		}
		cleanupIntegrationStageAndLock(t, destination, parent)
	})
}

func integrationDestination(t *testing.T, base *url.URL, name string) RepositoryDestination {
	t.Helper()
	copyURL := *base
	copyURL.Path = path.Join(base.Path, name)
	destination, err := ParseRepositoryDestination(copyURL.String())
	if err != nil {
		t.Fatal(err)
	}
	destination.SSHConfigPath = os.Getenv("TPA_SSH_OUTPUT_CONFIG")
	return destination
}

func integrationFinalPath(destination RepositoryDestination) string {
	if destination.PathMode == SSHPathHomeRelative {
		return "~/" + destination.RemotePath
	}
	return path.Join("/", destination.RemotePath)
}

func integrationEmptyCandidate(t *testing.T) Config {
	t.Helper()
	cfg := Config{OutDir: filepath.Join(t.TempDir(), "candidate"), Repo: RepoConfig{Suite: "stable", Codename: "bookworm", Components: "main"}}
	if err := InitializeRepository(cfg, []string{"all"}); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func assertRemoteNotPublished(t *testing.T, destination RepositoryDestination, err error, want RemoteActivationState) {
	t.Helper()
	var publishErr *RemotePublishError
	if err == nil || !errorsAs(err, &publishErr) || publishErr.Activation != want {
		t.Fatalf("publication error = %v, want activation state %s", err, want)
	}
	if err := integrationRemoteShell(t, destination, "test ! -e "+shellQuote(integrationFinalPath(destination))); err != nil {
		t.Fatalf("failed publication created its final path: %v", err)
	}
}

func assertNoRemoteCandidateArtifacts(t *testing.T, destination RepositoryDestination, parent string) {
	t.Helper()
	lockPath := remotePublicationLockPath(parent, path.Base(destination.RemotePath))
	command := "test ! -e " + shellQuote(lockPath) + " && test -z \"$(find " + shellQuote(parent) + " -mindepth 1 -maxdepth 1 -type d -name '.tpa-upload-*' -print -quit)\""
	if err := integrationRemoteShell(t, destination, command); err != nil {
		t.Fatalf("pre-activation cleanup left a remote lock or candidate: %v", err)
	}
}

func cleanupIntegrationStageAndLock(t *testing.T, destination RepositoryDestination, parent string) {
	t.Helper()
	lockPath := remotePublicationLockPath(parent, path.Base(destination.RemotePath))
	command := "for stage in " + shellQuote(parent) + "/.tpa-upload-*; do [ ! -d \"$stage\" ] || rm -rf -- \"$stage\"; done; rmdir " + shellQuote(lockPath) + " 2>/dev/null || true"
	if err := integrationRemoteShell(t, destination, command); err != nil {
		t.Fatalf("clean injected remote test state: %v", err)
	}
}

func integrationRemoteShell(t *testing.T, destination RepositoryDestination, command string) error {
	t.Helper()
	args := []string{"-F", destination.SSHConfigPath, "-oBatchMode=yes", "-oStrictHostKeyChecking=yes"}
	if destination.PortExplicit {
		args = append(args, "-p", strconv.Itoa(destination.Port))
	}
	host := destination.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if destination.User != "" {
		host = destination.User + "@" + host
	}
	args = append(args, host, command)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "ssh", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ssh test helper: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func testToken(t *testing.T) string {
	t.Helper()
	return strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func errorsAs(err error, target any) bool {
	return errors.As(err, target)
}
