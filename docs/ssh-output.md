# SSH/SFTP Repository Output

TPA can publish a verified repository to a new remote directory over the
OpenSSH SFTP subsystem. The repository builder remains the same as for local
output: `.deb` artifacts are authoritative and TPA v1 indexes, Release metadata,
and paired browser sidecars are generated and verified locally before transfer.

```sh
# New empty repository; absolute SSH URL path
tpa pack --empty \
  --output=ssh://repo-user@repo-host/srv/apt/empty \
  --ssh-config=$HOME/.ssh/config \
  -suite=stable -codename=bookworm -components=main \
  -architectures=amd64,arm64

# New package repository; SCP-style relative path is relative to remote home
tpa pack -in=dist --output=repo-host:apt/example \
  -suite=stable -codename=bookworm -components=main
```

The canonical empty command is `pack --empty`. Without `--empty`, `pack` still requires
at least one top-level `.deb`; it never silently converts an empty input set to
an empty repository.

## Destination syntax and SSH configuration

- `ssh://[user@]host[:port]/absolute/path` selects an absolute remote path.
- `ssh://[user@]host[:port]/~/path` selects a path relative to the remote home.
- `host:path` and `user@host:path` are SCP-style home-relative destinations.
- `host:/absolute/path` is an SCP-style absolute destination.
- Bracketed IPv6 is supported, for example `ssh://user@[2001:db8::1]:2222/srv/apt/repo`.
- Paths containing traversal, control characters, or ambiguous dot/empty
  segments are rejected. A relative local filename containing a colon should be
  written as `./name:part` (or must already exist) to disambiguate it from
  `host:path`.

TPA invokes the system OpenSSH `sftp` client in batch mode. By default it uses
the normal OpenSSH client configuration, identity/agent selection, and host-key
store. `--ssh-config=PATH` supplies an alternate OpenSSH configuration file.
The client is non-interactive (`BatchMode=yes`) and TPA forces
`StrictHostKeyChecking=yes`; unknown and changed host keys fail closed. SSH
password prompts are not supported. Configure a key or agent using OpenSSH. Each
SFTP batch has a two-minute operation deadline (or the caller's earlier context
deadline); a timed-out pre-activation operation gets at most 20 seconds for
best-effort candidate/lock cleanup.

The remote parent must already exist and resolve canonically; TPA does not
create missing parents. Parent symlinks are refused. The authenticated account
must be able to create a sibling directory, upload/read files, chmod the
candidate, and rename it. Protect the parent from untrusted writers and avoid
concurrent out-of-band changes during publication.

## Publication and safety guarantees

1. TPA builds a local temporary candidate and runs strict TPA v1 verification.
2. It resolves and checks the remote parent and refuses every existing final
   path, including an existing empty directory, symlink, or unrelated file.
3. A deterministic sibling lock serializes cooperating TPA writers. A random,
   private sibling staging directory is created; expected files are transferred
   with SFTP only.
4. TPA checks remote directory resolution, exact file inventory, regular-file
   types, sizes, and reads every file back. The downloaded bytes must match the
   local inventory and pass strict TPA v1 verification.
5. After setting repository permissions, TPA rechecks the destination and
   renames the complete candidate into place as a same-parent SFTP directory
   rename. It then reads the activated tree back and verifies it again before
   reporting success.

The backend does **not** replace an existing remote repository. Standard SFTP
has no portable atomic exchange for a populated directory. SFTP server rename
semantics and atomic-visibility guarantees are server-dependent. The sibling
lock prevents races between TPA publishers only; it cannot exclude arbitrary
processes that mutate the remote filesystem. OpenSSH servers may replace an
empty destination directory during a rename race, so the remote parent must be
under operator-exclusive write control. TPA checks the final path immediately
before activation but does not claim protection from an out-of-band writer that
races that check.

If the SFTP connection fails around activation, TPA inspects the final path. If
it cannot establish whether the rename took effect, it returns a typed
`RemotePublishError` with `activation-unknown` and preserves the sibling lock
and candidate for inspection. Do not blindly retry: inspect/verify the final
path and staging directory first. A retained `.tpa-publish-lock-*` can be stale
only after confirming no TPA publisher is active; remove it manually only after
checking the final and `.tpa-upload-*` trees. A post-activation verification or
lock-cleanup error reports `activation=activated`; the repository may already
be live.

`--atomic-publish` remains local-filesystem-only. The SSH client does not provide
hosted authorization, quotas, managed signing, publication staging services, or
an inbound upload server. It does not write to TPA.run or any hosted storage.
APT clients still need a separately configured HTTP(S) server to serve the
published directory. Read-only SSH inspection remains a distinct operation.

## Qualification

Run `tests/ssh-output-qualification.sh` on a host with Docker, Go, OpenSSH
clients, and `dpkg-deb`. It creates and removes a disposable OpenSSH/SFTP server,
uses synthetic host/user keys, exercises SSH URL and shorthand paths, tests
strict host-key/authentication failures, existing/unwritable/symlinked paths,
concurrent TPA writers, upload corruption/cancellation and uncertain activation,
and runs APT update/install against the remote tree over a temporary HTTP
server. No hosted credentials or production repository are used.
