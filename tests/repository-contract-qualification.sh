#!/bin/sh
# Qualifies empty repository initialization and transition to the first package.
# Requires docker, gpg, dpkg-deb, gzip, and a Go toolchain. Uses a throw-away key.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
TMP=$(mktemp -d)
GNUPGHOME="$TMP/gnupg"
export GNUPGHOME
cleanup() {
    rm -rf "$TMP"
}
trap cleanup EXIT INT TERM
mkdir -m 700 "$GNUPGHOME"

cat >"$TMP/key.conf" <<'EOF'
%no-protection
Key-Type: RSA
Key-Length: 2048
Name-Real: TPA empty repository qualification
Name-Email: tpa-empty-qualification@example.invalid
Expire-Date: 0
%commit
EOF
gpg --batch --generate-key "$TMP/key.conf" >/dev/null 2>&1
KEY=$(gpg --batch --with-colons --list-secret-keys | awk -F: '$1 == "fpr" { print $10; exit }')
[ -n "$KEY" ] || { echo 'could not create qualification key' >&2; exit 1; }
gpg --batch --export "$KEY" >"$TMP/fixture.gpg"

(cd "$ROOT" && go build -o "$TMP/tpa" .)
"$TMP/tpa" capabilities >"$TMP/capabilities.json"
grep -q '"format": "tpa-capabilities"' "$TMP/capabilities.json"
grep -q '"version": 1' "$TMP/capabilities.json"

"$TMP/tpa" pack --empty --output="$TMP/repo" -suite=stable -codename=stable \
    -components=main -architectures=all -gpg="$KEY" >/dev/null
[ -s "$TMP/repo/dists/stable/Release" ]
[ -s "$TMP/repo/dists/stable/InRelease" ]
[ -f "$TMP/repo/dists/stable/main/binary-all/Packages" ]
[ ! -s "$TMP/repo/dists/stable/main/binary-all/Packages" ]
[ -s "$TMP/repo/dists/stable/main/binary-all/Packages.gz" ]
[ -s "$TMP/repo/index.html" ]
[ -s "$TMP/repo/repository.json" ]
grep -q '"format": "tpa-repository-index"' "$TMP/repo/repository.json"
grep -q '"packages": \[\]' "$TMP/repo/repository.json"
! grep -q '<script' "$TMP/repo/index.html"
gpg --batch --verify "$TMP/repo/dists/stable/InRelease" >/dev/null 2>&1
"$TMP/tpa" verify --json -keyring="$TMP/fixture.gpg" -fingerprint="$KEY" \
    "$TMP/repo" >"$TMP/empty-verify.json"
grep -q '"valid":true' "$TMP/empty-verify.json"

# Both an existing empty directory and a non-empty path are protected from init.
mkdir "$TMP/protected-empty"
if "$TMP/tpa" pack --empty --output="$TMP/protected-empty" -suite=stable \
    -codename=stable -components=main -architectures=all >/dev/null 2>&1; then
    echo 'pack --empty replaced an existing empty directory' >&2
    exit 1
fi
mkdir "$TMP/protected"
printf '%s\n' keep >"$TMP/protected/marker"
if "$TMP/tpa" pack --empty --output="$TMP/protected" -suite=stable \
    -codename=stable -components=main -architectures=all >/dev/null 2>&1; then
    echo 'pack --empty replaced a non-empty directory' >&2
    exit 1
fi
grep -qx keep "$TMP/protected/marker"

make_deb() {
    package_root="$TMP/package-root"
    mkdir -p "$package_root/DEBIAN" "$package_root/usr/share/tpa-empty-fixture"
    cat >"$package_root/DEBIAN/control" <<'EOF'
Package: tpa-empty-fixture
Version: 1.0.0
Architecture: all
Maintainer: TPA qualification <tpa-empty-qualification@example.invalid>
Description: First package after empty repository initialization
EOF
    printf '%s\n' installed >"$package_root/usr/share/tpa-empty-fixture/state"
    mkdir "$TMP/packages"
    dpkg-deb --build --root-owner-group "$package_root" \
        "$TMP/packages/tpa-empty-fixture_1.0.0_all.deb" >/dev/null
}
make_deb
"$TMP/tpa" pack -in="$TMP/packages" --atomic-publish="$TMP/repo" \
    -suite=stable -codename=stable -components=main -gpg="$KEY" >/dev/null
"$TMP/tpa" verify --json -keyring="$TMP/fixture.gpg" -fingerprint="$KEY" \
    "$TMP/repo" >"$TMP/populated-verify.json"
grep -q '"valid":true' "$TMP/populated-verify.json"
grep -q 'Package: tpa-empty-fixture' \
    "$TMP/repo/dists/stable/main/binary-all/Packages"
grep -q 'tpa-empty-fixture' "$TMP/repo/repository.json"

make_apt_client() {
    name=$1
    install=$2
    repository=$3
    context="$TMP/$name"
    mkdir -p "$context"
    cp -a "$repository" "$context/repo"
    cp "$TMP/fixture.gpg" "$context/fixture.gpg"
    if [ "$install" = yes ]; then
        cat >"$context/Dockerfile" <<'EOF'
FROM debian:bookworm-slim
COPY repo /repo
COPY fixture.gpg /usr/share/keyrings/fixture.gpg
RUN rm -f /etc/apt/sources.list /etc/apt/sources.list.d/* && \
    printf 'deb [signed-by=/usr/share/keyrings/fixture.gpg] file:/repo stable main\n' > /etc/apt/sources.list.d/fixture.list && \
    apt-get update && \
    apt-get install -y tpa-empty-fixture=1.0.0 && \
    test "$(cat /usr/share/tpa-empty-fixture/state)" = installed
EOF
    else
        cat >"$context/Dockerfile" <<'EOF'
FROM debian:bookworm-slim
COPY repo /repo
COPY fixture.gpg /usr/share/keyrings/fixture.gpg
RUN rm -f /etc/apt/sources.list /etc/apt/sources.list.d/* && \
    printf 'deb [signed-by=/usr/share/keyrings/fixture.gpg] file:/repo stable main\n' > /etc/apt/sources.list.d/fixture.list && \
    apt-get update && \
    ! apt-cache show tpa-empty-fixture | grep -q '^Package:'
EOF
    fi
    docker build --network=none -f "$context/Dockerfile" "$context" >/dev/null
}

# Check APT can update from the signed empty repository before any package exists.
cp -a "$TMP/repo" "$TMP/populated-repo"
"$TMP/tpa" pack --empty --output="$TMP/repo-empty" -suite=stable -codename=stable \
    -components=main -architectures=all -gpg="$KEY" >/dev/null
make_apt_client empty no "$TMP/repo-empty"
make_apt_client populated yes "$TMP/populated-repo"

printf '%s\n' 'TPA empty repository initialization, signed APT update, first-package transition, and repository-format qualification passed.'
