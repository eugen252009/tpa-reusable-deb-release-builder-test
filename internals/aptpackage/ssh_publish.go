package aptpackage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type RemotePublishPhase string

const (
	RemotePhaseValidateLocal RemotePublishPhase = "verify-local-candidate"
	RemotePhaseConnect       RemotePublishPhase = "connect"
	RemotePhaseResolve       RemotePublishPhase = "resolve-destination"
	RemotePhasePrepare       RemotePublishPhase = "prepare-candidate"
	RemotePhaseUpload        RemotePublishPhase = "upload"
	RemotePhaseVerifyUpload  RemotePublishPhase = "verify-upload"
	RemotePhaseActivate      RemotePublishPhase = "activate"
	RemotePhaseVerifyLive    RemotePublishPhase = "verify-activated-repository"
	RemotePhaseCleanup       RemotePublishPhase = "cleanup"
)

type RemoteActivationState string

const (
	RemoteNotActivated      RemoteActivationState = "not-activated"
	RemoteActivated         RemoteActivationState = "activated"
	RemoteActivationUnknown RemoteActivationState = "activation-unknown"
)

// RemotePublishError preserves publication phase and activation state so callers
// do not mistake a post-rename error for an unchanged destination.
type RemotePublishError struct {
	Destination string
	Phase       RemotePublishPhase
	Activation  RemoteActivationState
	Err         error
}

func (err *RemotePublishError) Error() string {
	prefix := "SSH publication failed"
	diagnostic := strings.ToLower(err.Err.Error())
	switch {
	case strings.Contains(diagnostic, "host key verification failed"), strings.Contains(diagnostic, "host key is not known"), strings.Contains(diagnostic, "remote host identification has changed"), strings.Contains(diagnostic, "host key mismatch"):
		prefix = "SSH host key verification failed"
	case strings.Contains(diagnostic, "permission denied"), strings.Contains(diagnostic, "authentication failed"), strings.Contains(diagnostic, "no supported authentication methods"):
		prefix = "SSH authentication failed"
	case strings.Contains(diagnostic, "could not resolve hostname"), strings.Contains(diagnostic, "connection refused"), strings.Contains(diagnostic, "connection timed out"), strings.Contains(diagnostic, "no route to host"), strings.Contains(diagnostic, "network is unreachable"):
		prefix = "SSH destination unreachable"
	}
	return fmt.Sprintf("%s during %s for %s (activation %s): %v", prefix, err.Phase, err.Destination, err.Activation, err.Err)
}

func (err *RemotePublishError) Unwrap() error { return err.Err }

type remoteTree struct {
	manifest GenerationManifest
	dirs     []string
	children map[string]map[string]remoteEntry
}

type remoteEntry struct {
	directory bool
	file      GenerationFile
}

const (
	maxSFTPTranscriptBytes = 64 << 20
	maxSFTPBatchDuration   = 2 * time.Minute
)

// PublishRepositoryToSSH publishes an already-built, strictly verified TPA v1
// tree to a new destination using only the OpenSSH SFTP subsystem. Existing
// destinations are never updated; generic SFTP cannot safely exchange a
// populated directory. The remote parent must be controlled by the operator so
// out-of-band writers cannot race the final rename.
func PublishRepositoryToSSH(ctx context.Context, cfg Config, destination RepositoryDestination) error {
	return PublishRepositoryToSSHWithProgress(ctx, cfg, destination, nil)
}

// PublishRepositoryToSSHWithProgress reports stable phases before remote operations.
func PublishRepositoryToSSHWithProgress(ctx context.Context, cfg Config, destination RepositoryDestination, progress func(RemotePublishPhase)) error {
	report := func(phase RemotePublishPhase) {
		if progress != nil {
			progress(phase)
		}
	}
	if destination.Kind != DestinationSSH {
		return fmt.Errorf("destination is not an SSH/SFTP destination")
	}
	report(RemotePhaseValidateLocal)
	if err := VerifyTPARepositoryTree(cfg); err != nil {
		return fmt.Errorf("verify local repository candidate: %w", err)
	}
	manifest, err := CreateGenerationManifest(cfg.OutDir, "standalone-ssh", "ssh-output", "")
	if err != nil {
		return fmt.Errorf("inventory local repository candidate: %w", err)
	}
	tree, err := buildRemoteTree(manifest)
	if err != nil {
		return err
	}
	remoteDisplay := formatSSHOutputDestination(destination)
	activation := RemoteNotActivated
	var lockPath, stagePath string
	lockAcquired, stageCreated := false, false
	cleanupBeforeActivation := func(phase RemotePublishPhase, cause error) error {
		cleanupCtx := ctx
		var cancel context.CancelFunc
		if ctx.Err() != nil || errors.Is(cause, context.DeadlineExceeded) {
			cleanupCtx, cancel = context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
		}
		var cleanupErr error
		if stageCreated {
			cleanupErr = cleanupRemoteTree(cleanupCtx, destination, stagePath, tree)
		}
		if lockAcquired {
			if err := removeRemoteDirectory(cleanupCtx, destination, lockPath); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove publication lock: %w", err))
			} else {
				lockAcquired = false
			}
		}
		if cleanupErr != nil {
			return &RemotePublishError{Destination: remoteDisplay, Phase: RemotePhaseCleanup, Activation: RemoteNotActivated, Err: errors.Join(cause, cleanupErr)}
		}
		return &RemotePublishError{Destination: remoteDisplay, Phase: phase, Activation: RemoteNotActivated, Err: cause}
	}

	report(RemotePhaseConnect)
	home, err := sftpRemotePWD(ctx, destination)
	if err != nil {
		return &RemotePublishError{Destination: remoteDisplay, Phase: RemotePhaseConnect, Activation: activation, Err: err}
	}
	finalPath, parentPath, err := resolveSSHDestinationPath(destination, home)
	if err != nil {
		return &RemotePublishError{Destination: remoteDisplay, Phase: RemotePhaseResolve, Activation: activation, Err: err}
	}
	report(RemotePhaseResolve)
	if err := verifyRemoteCanonicalDirectory(ctx, destination, parentPath); err != nil {
		return &RemotePublishError{Destination: remoteDisplay, Phase: RemotePhaseResolve, Activation: activation, Err: err}
	}
	if exists, err := remotePathExists(ctx, destination, finalPath); err != nil {
		return &RemotePublishError{Destination: remoteDisplay, Phase: RemotePhaseResolve, Activation: activation, Err: err}
	} else if exists {
		return &RemotePublishError{Destination: remoteDisplay, Phase: RemotePhaseResolve, Activation: activation, Err: fmt.Errorf("destination already exists; generic SFTP replacement is unsupported")}
	}

	lockPath = remotePublicationLockPath(parentPath, path.Base(finalPath))
	lockCreated, err := acquireRemoteLock(ctx, destination, lockPath)
	if lockCreated {
		lockAcquired = true
	}
	if err != nil {
		return cleanupBeforeActivation(RemotePhasePrepare, fmt.Errorf("remote destination is locked or not writable: %w", err))
	}
	// Recheck while holding TPA's sibling lock. SFTP cannot exclude arbitrary
	// non-TPA writers; callers must keep the parent namespace under operator control.
	if exists, err := remotePathExists(ctx, destination, finalPath); err != nil {
		return cleanupBeforeActivation(RemotePhaseResolve, err)
	} else if exists {
		return cleanupBeforeActivation(RemotePhaseResolve, fmt.Errorf("destination already exists; no files were replaced"))
	}

	token, err := randomHexToken(16)
	if err != nil {
		return cleanupBeforeActivation(RemotePhasePrepare, err)
	}
	stagePath = path.Join(parentPath, ".tpa-upload-"+token)
	stageCreated, err = createRemoteStage(ctx, destination, stagePath, tree)
	if err != nil {
		return cleanupBeforeActivation(RemotePhasePrepare, err)
	}
	report(RemotePhaseUpload)
	if err := uploadRemoteTree(ctx, destination, stagePath, cfg.OutDir, tree); err != nil {
		return cleanupBeforeActivation(RemotePhaseUpload, err)
	}
	report(RemotePhaseVerifyUpload)
	if err := verifyRemoteTree(ctx, destination, stagePath, cfg.OutDir, cfg, manifest, tree); err != nil {
		return cleanupBeforeActivation(RemotePhaseVerifyUpload, err)
	}
	if err := publishRemotePermissions(ctx, destination, stagePath, tree); err != nil {
		return cleanupBeforeActivation(RemotePhasePrepare, err)
	}
	// Check once more immediately before rename. The sibling lock serializes TPA
	// clients, and the SFTP rename activates only a complete same-parent tree.
	if exists, err := remotePathExists(ctx, destination, finalPath); err != nil {
		return cleanupBeforeActivation(RemotePhaseActivate, err)
	} else if exists {
		return cleanupBeforeActivation(RemotePhaseActivate, fmt.Errorf("destination appeared before activation; no files were replaced"))
	}
	report(RemotePhaseActivate)
	if err := sftpRename(ctx, destination, stagePath, finalPath); err != nil {
		stateErr := resolveAmbiguousActivation(ctx, destination, finalPath, cfg, manifest, tree)
		if stateErr == nil {
			return &RemotePublishError{Destination: remoteDisplay, Phase: RemotePhaseActivate, Activation: RemoteActivationUnknown, Err: fmt.Errorf("SFTP rename returned an error, but the final path contains the verified candidate; inspect it before retrying: %w", err)}
		}
		if errors.Is(stateErr, os.ErrNotExist) {
			return cleanupBeforeActivation(RemotePhaseActivate, err)
		}
		return &RemotePublishError{Destination: remoteDisplay, Phase: RemotePhaseActivate, Activation: RemoteActivationUnknown, Err: errors.Join(err, stateErr)}
	}
	activation = RemoteActivated
	stageCreated = false // the rename moved the staging directory to finalPath.
	report(RemotePhaseVerifyLive)
	if err := verifyRemoteTree(ctx, destination, finalPath, cfg.OutDir, cfg, manifest, tree); err != nil {
		return &RemotePublishError{Destination: remoteDisplay, Phase: RemotePhaseVerifyLive, Activation: RemoteActivated, Err: err}
	}
	report(RemotePhaseCleanup)
	if err := removeRemoteDirectory(ctx, destination, lockPath); err != nil {
		return &RemotePublishError{Destination: remoteDisplay, Phase: RemotePhaseCleanup, Activation: RemoteActivated, Err: fmt.Errorf("repository is active and verified, but the publication lock remains: %w", err)}
	}
	lockAcquired = false
	return nil
}

func buildRemoteTree(manifest GenerationManifest) (remoteTree, error) {
	tree := remoteTree{manifest: manifest, dirs: []string{""}, children: map[string]map[string]remoteEntry{"": {}}}
	for _, file := range manifest.Files {
		if err := validateGenerationRelativePath(file.Path); err != nil {
			return remoteTree{}, err
		}
		parent := path.Dir(file.Path)
		if parent == "." {
			parent = ""
		}
		for directory := parent; directory != "" && directory != "."; directory = path.Dir(directory) {
			if _, exists := tree.children[directory]; !exists {
				tree.children[directory] = make(map[string]remoteEntry)
				tree.dirs = append(tree.dirs, directory)
			}
		}
		if _, exists := tree.children[parent]; !exists {
			tree.children[parent] = make(map[string]remoteEntry)
		}
		tree.children[parent][path.Base(file.Path)] = remoteEntry{file: file}
	}
	for _, directory := range append([]string(nil), tree.dirs...) {
		if directory == "" {
			continue
		}
		parent := path.Dir(directory)
		if parent == "." {
			parent = ""
		}
		if _, exists := tree.children[parent]; !exists {
			tree.children[parent] = make(map[string]remoteEntry)
		}
		tree.children[parent][path.Base(directory)] = remoteEntry{directory: true}
	}
	// Walk-derived manifests are already sorted. Make directory operations
	// deterministic and ensure parents precede their children.
	sortRemoteDirectories(tree.dirs)
	return tree, nil
}

func sortRemoteDirectories(directories []string) {
	sort.Slice(directories, func(i, j int) bool {
		depthI, depthJ := strings.Count(directories[i], "/"), strings.Count(directories[j], "/")
		if directories[i] == "" || directories[j] == "" {
			return directories[i] == "" && directories[j] != ""
		}
		if depthI != depthJ {
			return depthI < depthJ
		}
		return directories[i] < directories[j]
	})
}

func sftpRemotePWD(ctx context.Context, destination RepositoryDestination) (string, error) {
	output, err := runSFTPBatch(ctx, destination, func(writer io.Writer) error { _, err := io.WriteString(writer, "pwd\n"); return err })
	if err != nil {
		return "", sshOperationError("read remote home directory", output, err)
	}
	return parseRemotePWD(output)
}

func parseRemotePWD(output string) (string, error) {
	const marker = "Remote working directory: "
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if index := strings.Index(line, marker); index >= 0 {
			value := strings.TrimSpace(line[index+len(marker):])
			if value != "" && strings.HasPrefix(value, "/") && !strings.ContainsAny(value, "\x00\r\n") && path.Clean(value) == value {
				return value, nil
			}
			return "", fmt.Errorf("SFTP returned a non-canonical remote working directory")
		}
	}
	return "", fmt.Errorf("SFTP did not report the remote working directory")
}

func resolveSSHDestinationPath(destination RepositoryDestination, home string) (finalPath, parentPath string, err error) {
	if destination.PathMode == SSHPathHomeRelative {
		finalPath = path.Join(home, destination.RemotePath)
	} else {
		finalPath = path.Join("/", destination.RemotePath)
	}
	if finalPath == "/" || path.Base(finalPath) == "." || path.Base(finalPath) == "/" {
		return "", "", fmt.Errorf("remote destination must name a new repository directory")
	}
	parentPath = path.Dir(finalPath)
	return finalPath, parentPath, nil
}

func verifyRemoteCanonicalDirectory(ctx context.Context, destination RepositoryDestination, expected string) error {
	output, err := runSFTPBatch(ctx, destination, func(writer io.Writer) error {
		if _, err := fmt.Fprintf(writer, "cd %s\npwd\n", quoteSFTP(expected)); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("remote parent directory %s is missing or inaccessible: %w", expected, sshOperationError("resolve remote parent", output, err))
	}
	actual, err := parseRemotePWD(output)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("remote parent resolves through a symlink or non-canonical path (%s -> %s)", expected, actual)
	}
	return nil
}

func remotePathExists(ctx context.Context, destination RepositoryDestination, remotePath string) (bool, error) {
	output, err := runSFTPBatch(ctx, destination, func(writer io.Writer) error {
		_, err := fmt.Fprintf(writer, "ls -l %s\n", quoteSFTP(remotePath))
		return err
	})
	if err == nil {
		return true, nil
	}
	message := strings.ToLower(output)
	if strings.Contains(message, "no such file or directory") || strings.Contains(message, "not found") {
		return false, nil
	}
	return false, sshOperationError("inspect remote destination", output, err)
}

func remotePublicationLockPath(parent, basename string) string {
	sum := sha256.Sum256([]byte(parent + "\x00" + basename))
	return path.Join(parent, ".tpa-publish-lock-"+hex.EncodeToString(sum[:8]))
}

func acquireRemoteLock(ctx context.Context, destination RepositoryDestination, lockPath string) (bool, error) {
	output, err := runSFTPBatch(ctx, destination, func(writer io.Writer) error {
		if _, err := fmt.Fprintf(writer, "mkdir %s\nchmod 700 %s\n", quoteSFTP(lockPath), quoteSFTP(lockPath)); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		// Batch mode stops after a failed mkdir. Seeing the following chmod
		// command echoed proves mkdir succeeded and the directory is ours.
		created := strings.Contains(output, "sftp> chmod 700 "+quoteSFTP(lockPath))
		return created, sshOperationError("create remote publication lock", output, err)
	}
	return true, nil
}

func createRemoteStage(ctx context.Context, destination RepositoryDestination, stagePath string, tree remoteTree) (bool, error) {
	output, err := runSFTPBatch(ctx, destination, func(writer io.Writer) error {
		if _, err := fmt.Fprintf(writer, "mkdir %s\nchmod 700 %s\n", quoteSFTP(stagePath), quoteSFTP(stagePath)); err != nil {
			return err
		}
		for _, directory := range tree.dirs {
			if directory == "" {
				continue
			}
			remote := path.Join(stagePath, directory)
			if _, err := fmt.Fprintf(writer, "mkdir %s\nchmod 700 %s\n", quoteSFTP(remote), quoteSFTP(remote)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		created := strings.Contains(output, "sftp> chmod 700 "+quoteSFTP(stagePath))
		return created, sshOperationError("create private remote candidate", output, err)
	}
	return true, nil
}

func uploadRemoteTree(ctx context.Context, destination RepositoryDestination, stagePath, localRoot string, tree remoteTree) error {
	output, err := runSFTPBatch(ctx, destination, func(writer io.Writer) error {
		for _, file := range tree.manifest.Files {
			local := filepath.Join(localRoot, filepath.FromSlash(file.Path))
			remote := path.Join(stagePath, file.Path)
			if _, err := fmt.Fprintf(writer, "put %s %s\n", quoteSFTP(local), quoteSFTP(remote)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return sshOperationError("upload repository candidate", output, err)
	}
	return nil
}

func verifyRemoteTree(ctx context.Context, destination RepositoryDestination, remoteRoot, localRoot string, cfg Config, manifest GenerationManifest, tree remoteTree) error {
	if err := verifyRemoteDirectories(ctx, destination, remoteRoot, tree); err != nil {
		return err
	}
	if err := verifyRemoteListings(ctx, destination, remoteRoot, tree); err != nil {
		return err
	}
	// Read each uploaded file back through SFTP over the same local path. This
	// keeps the scratch-space bound to the candidate size rather than doubling it.
	for _, file := range tree.manifest.Files {
		local := filepath.Join(localRoot, filepath.FromSlash(file.Path))
		if err := os.Remove(local); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	output, err := runSFTPBatch(ctx, destination, func(writer io.Writer) error {
		for _, file := range tree.manifest.Files {
			local := filepath.Join(localRoot, filepath.FromSlash(file.Path))
			remote := path.Join(remoteRoot, file.Path)
			if _, err := fmt.Fprintf(writer, "get %s %s\n", quoteSFTP(remote), quoteSFTP(local)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return sshOperationError("read back remote repository files", output, err)
	}
	if err := VerifyGenerationManifest(localRoot, manifest, manifest.RepositoryID); err != nil {
		return fmt.Errorf("remote file inventory or bytes differ from the local candidate: %w", err)
	}
	cfg.OutDir = localRoot
	if err := VerifyTPARepositoryTree(cfg); err != nil {
		return fmt.Errorf("remote candidate failed strict TPA v1 verification: %w", err)
	}
	return nil
}

func verifyRemoteDirectories(ctx context.Context, destination RepositoryDestination, remoteRoot string, tree remoteTree) error {
	output, err := runSFTPBatch(ctx, destination, func(writer io.Writer) error {
		for _, directory := range tree.dirs {
			remote := remoteRoot
			if directory != "" {
				remote = path.Join(remoteRoot, directory)
			}
			if _, err := fmt.Fprintf(writer, "cd %s\npwd\n", quoteSFTP(remote)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return sshOperationError("resolve uploaded repository directories", output, err)
	}
	var actual []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		const marker = "Remote working directory: "
		if index := strings.Index(line, marker); index >= 0 {
			actual = append(actual, strings.TrimSpace(line[index+len(marker):]))
		}
	}
	if len(actual) != len(tree.dirs) {
		return fmt.Errorf("SFTP reported %d repository directories, expected %d", len(actual), len(tree.dirs))
	}
	for index, directory := range tree.dirs {
		expected := remoteRoot
		if directory != "" {
			expected = path.Join(remoteRoot, directory)
		}
		if actual[index] != expected {
			return fmt.Errorf("remote repository directory %s resolves through a symlink to %s", expected, actual[index])
		}
	}
	return nil
}

func verifyRemoteListings(ctx context.Context, destination RepositoryDestination, remoteRoot string, tree remoteTree) error {
	directories := append([]string(nil), tree.dirs...)
	commands := make([]string, 0, len(directories))
	for _, directory := range directories {
		remote := remoteRoot
		if directory != "" {
			remote = path.Join(remoteRoot, directory)
		}
		commands = append(commands, "ls -l "+quoteSFTP(remote))
	}
	output, err := runSFTPBatch(ctx, destination, func(writer io.Writer) error {
		for _, command := range commands {
			if _, err := io.WriteString(writer, command+"\n"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return sshOperationError("list uploaded repository candidate", output, err)
	}
	rows := make([][]string, len(commands))
	current := -1
	commandIndex := make(map[string]int, len(commands))
	for index, command := range commands {
		commandIndex[command] = index
	}
	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(rawLine, "\r"))
		if strings.HasPrefix(line, "sftp> ") {
			current = -1
			if index, ok := commandIndex[strings.TrimPrefix(line, "sftp> ")]; ok {
				current = index
			}
			continue
		}
		if current >= 0 && line != "" {
			rows[current] = append(rows[current], line)
		}
	}
	for index, directory := range directories {
		expected := tree.children[directory]
		seen := make(map[string]bool, len(expected))
		for _, row := range rows[index] {
			fields := strings.Fields(row)
			if len(fields) < 9 || len(fields[0]) < 10 {
				return fmt.Errorf("invalid SFTP listing row for %s", directory)
			}
			name := path.Base(fields[len(fields)-1])
			entry, ok := expected[name]
			if !ok {
				return fmt.Errorf("remote candidate contains unexpected entry %s/%s", directory, name)
			}
			if seen[name] {
				return fmt.Errorf("remote candidate repeats entry %s/%s", directory, name)
			}
			seen[name] = true
			mode := fields[0][0]
			if entry.directory {
				if mode != 'd' {
					return fmt.Errorf("remote candidate directory %s/%s is not a real directory", directory, name)
				}
			} else {
				if mode != '-' {
					return fmt.Errorf("remote candidate file %s/%s is not regular", directory, name)
				}
				gotSize, err := strconv.ParseInt(fields[4], 10, 64)
				if err != nil || gotSize != entry.file.Size {
					return fmt.Errorf("remote uploaded file size mismatch for %s/%s", directory, name)
				}
			}
		}
		if len(seen) != len(expected) {
			return fmt.Errorf("remote candidate directory %s has missing entries", directory)
		}
	}
	return nil
}

func publishRemotePermissions(ctx context.Context, destination RepositoryDestination, stagePath string, tree remoteTree) error {
	output, err := runSFTPBatch(ctx, destination, func(writer io.Writer) error {
		for _, file := range tree.manifest.Files {
			if _, err := fmt.Fprintf(writer, "chmod 644 %s\n", quoteSFTP(path.Join(stagePath, file.Path))); err != nil {
				return err
			}
		}
		for index := len(tree.dirs) - 1; index >= 0; index-- {
			directory := tree.dirs[index]
			remote := stagePath
			if directory != "" {
				remote = path.Join(stagePath, directory)
			}
			if _, err := fmt.Fprintf(writer, "chmod 755 %s\n", quoteSFTP(remote)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return sshOperationError("set published repository permissions", output, err)
	}
	return nil
}

func sftpRename(ctx context.Context, destination RepositoryDestination, from, to string) error {
	output, err := runSFTPBatch(ctx, destination, func(writer io.Writer) error {
		_, err := fmt.Fprintf(writer, "rename %s %s\n", quoteSFTP(from), quoteSFTP(to))
		return err
	})
	if err != nil {
		return sshOperationError("activate remote repository", output, err)
	}
	return nil
}

func resolveAmbiguousActivation(ctx context.Context, destination RepositoryDestination, finalPath string, cfg Config, manifest GenerationManifest, tree remoteTree) error {
	exists, err := remotePathExists(ctx, destination, finalPath)
	if err != nil {
		return fmt.Errorf("inspect final path after ambiguous rename: %w", err)
	}
	if !exists {
		return os.ErrNotExist
	}
	if err := verifyRemoteTree(ctx, destination, finalPath, cfg.OutDir, cfg, manifest, tree); err != nil {
		return fmt.Errorf("final path exists but could not be verified after ambiguous rename: %w", err)
	}
	return nil
}

func cleanupRemoteTree(ctx context.Context, destination RepositoryDestination, stagePath string, tree remoteTree) error {
	output, err := runSFTPBatch(ctx, destination, func(writer io.Writer) error {
		for _, file := range tree.manifest.Files {
			if _, err := fmt.Fprintf(writer, "-rm %s\n", quoteSFTP(path.Join(stagePath, file.Path))); err != nil {
				return err
			}
		}
		for index := len(tree.dirs) - 1; index >= 0; index-- {
			directory := tree.dirs[index]
			remote := stagePath
			if directory != "" {
				remote = path.Join(stagePath, directory)
			}
			if _, err := fmt.Fprintf(writer, "-rmdir %s\n", quoteSFTP(remote)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return sshOperationError("remove private remote candidate", output, err)
	}
	if exists, err := remotePathExists(ctx, destination, stagePath); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("candidate directory remains after cleanup")
	}
	return nil
}

func removeRemoteDirectory(ctx context.Context, destination RepositoryDestination, remotePath string) error {
	output, err := runSFTPBatch(ctx, destination, func(writer io.Writer) error {
		_, err := fmt.Fprintf(writer, "rmdir %s\n", quoteSFTP(remotePath))
		return err
	})
	if err != nil {
		return sshOperationError("remove remote publication lock", output, err)
	}
	return nil
}

func randomHexToken(length int) (string, error) {
	data := make([]byte, length)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func formatSSHOutputDestination(destination RepositoryDestination) string {
	host := destination.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if destination.User != "" {
		host = destination.User + "@" + host
	}
	if destination.PortExplicit {
		host += ":" + strconv.Itoa(destination.Port)
	}
	pathValue := "/" + destination.RemotePath
	if destination.PathMode == SSHPathHomeRelative {
		pathValue = "/~/" + destination.RemotePath
	}
	return "ssh://" + host + pathValue
}

func quoteSFTP(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, `"`, `\"`)
	return `"` + value + `"`
}

type sftpTranscript struct {
	stdout []byte
	stderr []byte
	cut    bool
}

func runSFTPBatch(ctx context.Context, destination RepositoryDestination, writeBatch func(io.Writer) error) (string, error) {
	boundedCtx, cancel := context.WithTimeout(ctx, maxSFTPBatchDuration)
	defer cancel()
	ctx = boundedCtx
	if _, err := exec.LookPath("sftp"); err != nil {
		return "", fmt.Errorf("OpenSSH sftp client is not installed: %w", err)
	}
	args := []string{"-q", "-b", "-"}
	if destination.SSHConfigPath != "" {
		args = append(args, "-F", destination.SSHConfigPath)
	}
	args = append(args, "-oBatchMode=yes", "-oStrictHostKeyChecking=yes", "-oConnectTimeout=15", "-oServerAliveInterval=15", "-oServerAliveCountMax=2")
	if destination.PortExplicit {
		args = append(args, "-P", strconv.Itoa(destination.Port))
	}
	host := destination.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if destination.User != "" {
		host = destination.User + "@" + host
	}
	args = append(args, host)
	cmd := exec.CommandContext(ctx, "sftp", args...)
	cmd.Env = localeEnvironment(os.Environ())
	stdout := &limitedSFTPOutput{}
	stderr := &limitedSFTPOutput{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start OpenSSH sftp client: %w", err)
	}
	writeDone := make(chan error, 1)
	go func() {
		writeErr := writeBatch(stdin)
		closeErr := stdin.Close()
		if writeErr != nil {
			writeDone <- writeErr
		} else {
			writeDone <- closeErr
		}
	}()
	waitErr := cmd.Wait()
	writeErr := <-writeDone
	transcript := string(stdout.data) + "\n" + string(stderr.data)
	if stdout.cut || stderr.cut {
		return transcript, fmt.Errorf("SFTP diagnostic exceeded %d bytes", maxSFTPTranscriptBytes)
	}
	if writeErr != nil {
		return transcript, writeErr
	}
	if waitErr != nil {
		if ctx.Err() != nil {
			return transcript, fmt.Errorf("SSH/SFTP operation canceled: %w", ctx.Err())
		}
		return transcript, fmt.Errorf("SFTP client exited unsuccessfully: %w: %s", waitErr, strings.TrimSpace(transcript))
	}
	return transcript, nil
}

func localeEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, "LC_ALL=") {
			result = append(result, entry)
		}
	}
	return append(result, "LC_ALL=C")
}

func sshOperationError(operation, output string, err error) error {
	message := strings.TrimSpace(output)
	if message == "" {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if len(message) > 2048 {
		message = message[:2048] + "..."
	}
	return fmt.Errorf("%s: %w: %s", operation, err, message)
}

type limitedSFTPOutput struct {
	data []byte
	cut  bool
}

func (writer *limitedSFTPOutput) Write(data []byte) (int, error) {
	remaining := maxSFTPTranscriptBytes - len(writer.data)
	if remaining > 0 {
		take := len(data)
		if take > remaining {
			take = remaining
		}
		writer.data = append(writer.data, data[:take]...)
	}
	if len(data) > remaining {
		writer.cut = true
	}
	return len(data), nil
}
