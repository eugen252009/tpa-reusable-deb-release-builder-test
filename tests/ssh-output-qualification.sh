#!/usr/bin/env bash
set -euo pipefail

root_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root_dir"
for tool in docker go ssh-keygen ssh-keyscan sftp dpkg-deb timeout; do
  command -v "$tool" >/dev/null || { echo "missing required tool: $tool" >&2; exit 1; }
done
docker info >/dev/null

tmp=$(mktemp -d "${TMPDIR:-/tmp}/tpa-ssh-output-qualification.XXXXXX")
container="tpa-ssh-output-qual-$$"
cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
  rm -rf -- "$tmp"
}
trap cleanup EXIT INT TERM

mkdir -p "$tmp/client/.ssh"
chmod 700 "$tmp/client" "$tmp/client/.ssh"
ssh-keygen -q -t ed25519 -N '' -f "$tmp/client/.ssh/id_ed25519"

echo "[1/7] Start disposable OpenSSH/SFTP server"
docker run -d --rm --name "$container" -p 127.0.0.1::2222 debian:bookworm-slim sleep infinity >/dev/null
for _ in $(seq 1 12); do
  if docker exec "$container" true >/dev/null 2>&1; then break; fi
  sleep 1
done
docker exec "$container" sh -c '
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq openssh-server python3 >/dev/null
  useradd -m -s /bin/sh tpauser
  mkdir -p /home/tpauser/.ssh /run/sshd
  chown -R tpauser:tpauser /home/tpauser
  chmod 700 /home/tpauser/.ssh
  ssh-keygen -A
  echo "PubkeyAuthentication yes" >> /etc/ssh/sshd_config
  echo "PasswordAuthentication no" >> /etc/ssh/sshd_config
' >/dev/null
docker cp "$tmp/client/.ssh/id_ed25519.pub" "$container:/home/tpauser/.ssh/authorized_keys" >/dev/null
docker exec "$container" chown tpauser:tpauser /home/tpauser/.ssh/authorized_keys
docker exec "$container" chmod 600 /home/tpauser/.ssh/authorized_keys
docker exec -d "$container" /usr/sbin/sshd -D -e -p 2222
port=$(docker port "$container" 2222/tcp | sed 's/.*://')
for _ in $(seq 1 10); do
  if ssh-keyscan -p "$port" 127.0.0.1 >/dev/null 2>&1; then break; fi
  sleep 1
done
ssh-keyscan -p "$port" 127.0.0.1 >"$tmp/client/.ssh/known_hosts" 2>/dev/null
chmod 600 "$tmp/client/.ssh/known_hosts" "$tmp/client/.ssh/id_ed25519"
cat >"$tmp/ssh_config" <<EOF
Host tpa-test
  HostName 127.0.0.1
  Port $port
  User tpauser
  IdentityFile $tmp/client/.ssh/id_ed25519
  IdentitiesOnly yes
  UserKnownHostsFile $tmp/client/.ssh/known_hosts
  StrictHostKeyChecking yes
Host *
  IdentityFile $tmp/client/.ssh/id_ed25519
  IdentitiesOnly yes
  UserKnownHostsFile $tmp/client/.ssh/known_hosts
  StrictHostKeyChecking yes
EOF
chmod 600 "$tmp/ssh_config"

go build -o "$tmp/tpa" "$root_dir"
tpa() { timeout 30s "$tmp/tpa" "$@"; }
pkg_arch=$(dpkg --print-architecture)
remote_ssh() { timeout 10s ssh -F "$tmp/ssh_config" -oBatchMode=yes -oStrictHostKeyChecking=yes "$@"; }
absolute_output() { printf 'ssh://tpauser@127.0.0.1:%s/home/tpauser/%s' "$port" "$1"; }
expect_failure() {
  local log=$1 status
  shift
  if "$@" >"$log.out" 2>"$log.err"; then
    echo "expected failure but command succeeded: $*" >&2
    exit 1
  else
    status=$?
  fi
  if [[ $status -eq 124 || $status -eq 137 ]]; then
    echo "expected failure but operation timed out: $*" >&2
    exit 1
  fi
  test -s "$log.err" || { echo "failure did not report a diagnostic: $*" >&2; exit 1; }
}

export TPA_SSH_OUTPUT_INTEGRATION=1
export TPA_SSH_OUTPUT_BASE_URL="ssh://tpauser@127.0.0.1:$port/home/tpauser"
export TPA_SSH_OUTPUT_CONFIG="$tmp/ssh_config"

echo "[2/7] Empty repository over SSH URL and SCP-style destination"
tpa pack --empty --output="$(absolute_output 'empty%20with%20space')" --ssh-config="$tmp/ssh_config" \
  --suite=stable --codename=bookworm --components=main --architectures="$pkg_arch,all"
# SCP-style relative destinations are relative to the remote home.
tpa pack --empty --output=tpa-test:empty-home --ssh-config="$tmp/ssh_config" \
  --suite=stable --codename=bookworm --components=main --architectures="$pkg_arch,all"
remote_ssh tpa-test 'test -s "/home/tpauser/empty with space/dists/bookworm/Release" && test -s /home/tpauser/empty-home/dists/bookworm/Release'

echo "[3/7] APT update against empty repository over HTTP"
docker exec -d "$container" python3 -m http.server 8080 --bind 127.0.0.1 --directory /home/tpauser >/tmp/tpa-http.log 2>&1
for _ in $(seq 1 20); do
  if docker exec "$container" python3 -c 'import urllib.request; urllib.request.urlopen("http://127.0.0.1:8080/empty-home/").read(1)' >/dev/null 2>&1; then break; fi
  sleep 0.1
done
docker exec "$container" sh -c '
  mkdir -p /tmp/tpa-apt/lists/partial /tmp/tpa-apt/cache/archives/partial
  printf "%s\n" "deb [trusted=yes] http://127.0.0.1:8080/empty-home bookworm main" >/tmp/tpa-empty.list
  apt-get -o Dir::Etc::sourcelist=/tmp/tpa-empty.list -o Dir::Etc::sourceparts=- \
    -o Dir::State::lists=/tmp/tpa-apt/lists -o Dir::Cache::archives=/tmp/tpa-apt/cache/archives \
    -o APT::Get::List-Cleanup=0 update -qq
' >/dev/null

# Build a tiny real Debian artifact and publish it to a separate home-relative destination.
mkdir -p "$tmp/package/DEBIAN" "$tmp/package/usr/bin" "$tmp/artifacts"
cat >"$tmp/package/DEBIAN/control" <<EOF
Package: tpa-ssh-fixture
Version: 1.0
Architecture: $pkg_arch
Maintainer: TPA Qualification <tpa@example.invalid>
Description: Disposable SSH publication qualification fixture
EOF
cat >"$tmp/package/usr/bin/tpa-ssh-fixture" <<'EOF'
#!/bin/sh
printf 'ssh publication qualification passed\n'
EOF
chmod 755 "$tmp/package/usr/bin/tpa-ssh-fixture"
dpkg-deb --build --root-owner-group "$tmp/package" "$tmp/artifacts/tpa-ssh-fixture_1.0_${pkg_arch}.deb" >/dev/null

echo "[4/7] Publish package repository and install via APT"
tpa pack -in="$tmp/artifacts" --output=tpa-test:package-repo --ssh-config="$tmp/ssh_config" \
  --suite=stable --codename=bookworm --components=main
remote_ssh tpa-test "test -s /home/tpauser/package-repo/dists/bookworm/main/binary-$pkg_arch/Packages"
docker exec "$container" sh -c '
  printf "%s\n" "deb [trusted=yes] http://127.0.0.1:8080/package-repo bookworm main" >/tmp/tpa-package.list
  apt-get -o Dir::Etc::sourcelist=/tmp/tpa-package.list -o Dir::Etc::sourceparts=- \
    -o Dir::State::lists=/tmp/tpa-apt/lists -o Dir::Cache::archives=/tmp/tpa-apt/cache/archives \
    -o APT::Get::List-Cleanup=0 update -qq
  apt-get -y -o Dir::Etc::sourcelist=/tmp/tpa-package.list -o Dir::Etc::sourceparts=- \
    -o Dir::State::lists=/tmp/tpa-apt/lists -o Dir::Cache::archives=/tmp/tpa-apt/cache/archives \
    install tpa-ssh-fixture -qq
  /usr/bin/tpa-ssh-fixture | grep -q "qualification passed"
' >/dev/null

# Existing trees, unrelated paths, absent parents, unwritable parents, and symlinked parents are refused.
echo "[5/7] Protect existing paths and reject unsafe remote parents"
expect_failure "$tmp/existing-repo" tpa pack --empty --output=tpa-test:package-repo --ssh-config="$tmp/ssh_config" \
  --suite=stable --codename=bookworm --components=main --architectures=all
remote_ssh tpa-test 'mkdir /home/tpauser/unrelated && printf keep >/home/tpauser/unrelated/marker && mkdir /home/tpauser/readonly && chmod 555 /home/tpauser/readonly && mkdir /home/tpauser/real-parent && ln -s real-parent /home/tpauser/symlink-parent && ln -s unrelated /home/tpauser/final-symlink && ln -s missing-target /home/tpauser/dangling-symlink'
expect_failure "$tmp/existing-unrelated" tpa pack --empty --output=tpa-test:unrelated --ssh-config="$tmp/ssh_config" \
  --suite=stable --codename=bookworm --components=main --architectures=all
expect_failure "$tmp/final-symlink" tpa pack --empty --output=tpa-test:final-symlink --ssh-config="$tmp/ssh_config" \
  --suite=stable --codename=bookworm --components=main --architectures=all
expect_failure "$tmp/dangling-symlink" tpa pack --empty --output=tpa-test:dangling-symlink --ssh-config="$tmp/ssh_config" \
  --suite=stable --codename=bookworm --components=main --architectures=all
expect_failure "$tmp/missing-parent" tpa pack --empty --output="$(absolute_output missing-parent/repo)" --ssh-config="$tmp/ssh_config" \
  --suite=stable --codename=bookworm --components=main --architectures=all
expect_failure "$tmp/readonly-parent" tpa pack --empty --output="$(absolute_output readonly/repo)" --ssh-config="$tmp/ssh_config" \
  --suite=stable --codename=bookworm --components=main --architectures=all
expect_failure "$tmp/symlink-parent" tpa pack --empty --output="$(absolute_output symlink-parent/repo)" --ssh-config="$tmp/ssh_config" \
  --suite=stable --codename=bookworm --components=main --architectures=all
remote_ssh tpa-test 'test "$(cat /home/tpauser/unrelated/marker)" = keep && test -L /home/tpauser/final-symlink && test -L /home/tpauser/dangling-symlink && test ! -e /home/tpauser/readonly/repo && test ! -e /home/tpauser/real-parent/repo'

# Invalid host trust and invalid authentication fail before creating the destination.
cat >"$tmp/unknown-host.conf" <<EOF
Host *
  IdentityFile $tmp/client/.ssh/id_ed25519
  IdentitiesOnly yes
  UserKnownHostsFile $tmp/unknown-known-hosts
  StrictHostKeyChecking yes
EOF
: >"$tmp/unknown-known-hosts"
expect_failure "$tmp/unknown-host" tpa pack --empty --output="$(absolute_output unknown-host/repo)" --ssh-config="$tmp/unknown-host.conf" \
  --suite=stable --codename=bookworm --components=main --architectures=all
read -r fake_type fake_key <"$tmp/client/.ssh/id_ed25519.pub"
printf '[127.0.0.1]:%s %s %s\n' "$port" "$fake_type" "$fake_key" >"$tmp/changed-known-hosts"
cat >"$tmp/changed-host.conf" <<EOF
Host *
  IdentityFile $tmp/client/.ssh/id_ed25519
  IdentitiesOnly yes
  UserKnownHostsFile $tmp/changed-known-hosts
  StrictHostKeyChecking yes
EOF
expect_failure "$tmp/changed-host" tpa pack --empty --output="$(absolute_output changed-host/repo)" --ssh-config="$tmp/changed-host.conf" --suite=stable --codename=bookworm --components=main --architectures=all
ssh-keygen -q -t ed25519 -N '' -f "$tmp/wrong-key"
cat >"$tmp/bad-auth.conf" <<EOF
Host *
  IdentityFile $tmp/wrong-key
  IdentitiesOnly yes
  UserKnownHostsFile $tmp/client/.ssh/known_hosts
  StrictHostKeyChecking yes
EOF
expect_failure "$tmp/bad-auth" tpa pack --empty --output="$(absolute_output bad-auth/repo)" --ssh-config="$tmp/bad-auth.conf" \
  --suite=stable --codename=bookworm --components=main --architectures=all

echo "[6/7] Concurrent writers and injected SFTP failure boundaries"
# Start writer A, wait for its exclusive sibling lock, then race writer B.
tpa pack --empty --output=tpa-test:concurrent-repo --ssh-config="$tmp/ssh_config" \
  --suite=stable --codename=bookworm --components=main --architectures=all >"$tmp/writer-a.out" 2>"$tmp/writer-a.err" &
writer_a=$!
for _ in $(seq 1 100); do
  if remote_ssh tpa-test 'find /home/tpauser -maxdepth 1 -type d -name ".tpa-publish-lock-*" -print -quit | grep -q .' >/dev/null 2>&1; then break; fi
  sleep 0.05
done
expect_failure "$tmp/writer-b" tpa pack --empty --output=tpa-test:concurrent-repo --ssh-config="$tmp/ssh_config" \
  --suite=stable --codename=bookworm --components=main --architectures=all
wait "$writer_a"
TPA_SSH_OUTPUT_INTEGRATION=1 TPA_SSH_OUTPUT_BASE_URL="$TPA_SSH_OUTPUT_BASE_URL" \
  TPA_SSH_OUTPUT_CONFIG="$tmp/ssh_config" \
  go test ./internals/aptpackage -run '^TestSSHOutputFailureBoundaries$' -count=1 -timeout=45s

# Every operation above stayed in the disposable container; no hosted credentials are used.
echo "[7/7] SSH/SFTP output qualification passed (OpenSSH server, strict known_hosts, APT install)."
