#!/bin/sh

# Auditable Conveyor release installer. It performs no telemetry or update checks:
# every network request below fetches only the release selected for this run.
set -eu

repository_url=${CONVEYOR_REPOSITORY_URL:-https://github.com/kidus-tiliksew/conveyor}
install_dir=${CONVEYOR_INSTALL_DIR:-"${HOME}/.local/bin"}
requested_version=${1:-}

fail() {
  printf 'conveyor install: %s\n' "$*" >&2
  exit 1
}

for tool in curl tar mktemp grep awk; do
  command -v "$tool" >/dev/null 2>&1 || fail "required tool '$tool' is not installed"
done

case $(uname -s) in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) fail "unsupported operating system '$(uname -s)'; supported systems are Linux and macOS" ;;
esac

case $(uname -m) in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) fail "unsupported architecture '$(uname -m)'; supported architectures are amd64 and arm64" ;;
esac

if [ -n "$requested_version" ]; then
  version=$requested_version
else
  latest_url=${CONVEYOR_LATEST_RELEASE_URL:-"${repository_url}/releases/latest"}
  resolved_url=$(curl -fsSL -o /dev/null -w '%{url_effective}' "$latest_url") ||
    fail "could not resolve the latest release from $latest_url"
  version=${resolved_url##*/}
fi

case $version in
  ''|*[!A-Za-z0-9._+-]*) fail "invalid release version '$version'" ;;
esac

archive="conveyor_${version}_${os}_${arch}.tar.gz"
asset_base=${CONVEYOR_RELEASE_DOWNLOAD_URL:-"${repository_url}/releases/download"}
archive_url="${asset_base}/${version}/${archive}"
checksums_url="${asset_base}/${version}/checksums.txt"

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/conveyor-install.XXXXXX") || fail "could not create a temporary directory"
installing=0
had_conveyor=0
had_conveyord=0
cleanup() {
  if [ "$installing" -eq 1 ]; then
    if [ "$had_conveyor" -eq 1 ]; then mv "$work_dir/old-conveyor" "$install_dir/conveyor" 2>/dev/null || true; else rm -f "$install_dir/conveyor"; fi
    if [ "$had_conveyord" -eq 1 ]; then mv "$work_dir/old-conveyord" "$install_dir/conveyord" 2>/dev/null || true; else rm -f "$install_dir/conveyord"; fi
  fi
  rm -f "$install_dir/.conveyor.new.$$" "$install_dir/.conveyord.new.$$" 2>/dev/null || true
  rm -f "$work_dir/$archive" "$work_dir/checksums.txt"
  rm -f "$work_dir/old-conveyor" "$work_dir/old-conveyord"
  if [ -d "$work_dir/conveyor_${version}_${os}_${arch}" ]; then
    rm -f "$work_dir/conveyor_${version}_${os}_${arch}/conveyor" "$work_dir/conveyor_${version}_${os}_${arch}/conveyord"
    rmdir "$work_dir/conveyor_${version}_${os}_${arch}" 2>/dev/null || true
  fi
  rmdir "$work_dir" 2>/dev/null || true
}
trap cleanup EXIT HUP INT TERM

curl -fsSL --retry 3 -o "$work_dir/$archive" "$archive_url" ||
  fail "could not download release archive $archive_url"
curl -fsSL --retry 3 -o "$work_dir/checksums.txt" "$checksums_url" ||
  fail "could not download checksum manifest $checksums_url"

expected=$(grep "  ${archive}$" "$work_dir/checksums.txt" | awk '{print $1}') || true
[ -n "$expected" ] || fail "checksum manifest does not contain $archive"

if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$work_dir/$archive" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$work_dir/$archive" | awk '{print $1}')
else
  fail "checksum verification requires sha256sum or shasum"
fi
[ "$actual" = "$expected" ] || fail "checksum verification failed for $archive; nothing was installed"

tar -C "$work_dir" -xzf "$work_dir/$archive" || fail "could not unpack verified archive $archive"
payload="$work_dir/conveyor_${version}_${os}_${arch}"
[ -f "$payload/conveyor" ] && [ -f "$payload/conveyord" ] || fail "verified archive is missing conveyor or conveyord"

mkdir -p "$install_dir" || fail "could not create install directory $install_dir"
[ -d "$install_dir" ] && [ -w "$install_dir" ] || fail "install directory is not writable: $install_dir"
cp "$payload/conveyor" "$install_dir/.conveyor.new.$$" || fail "could not stage conveyor in $install_dir"
cp "$payload/conveyord" "$install_dir/.conveyord.new.$$" || fail "could not stage conveyord in $install_dir"
chmod 755 "$install_dir/.conveyor.new.$$" "$install_dir/.conveyord.new.$$"
if [ -e "$install_dir/conveyor" ]; then cp -p "$install_dir/conveyor" "$work_dir/old-conveyor"; had_conveyor=1; fi
if [ -e "$install_dir/conveyord" ]; then cp -p "$install_dir/conveyord" "$work_dir/old-conveyord"; had_conveyord=1; fi
installing=1
mv "$install_dir/.conveyor.new.$$" "$install_dir/conveyor" || fail "could not replace $install_dir/conveyor"
mv "$install_dir/.conveyord.new.$$" "$install_dir/conveyord" || fail "could not replace $install_dir/conveyord"
installing=0

printf 'Installed Conveyor %s to %s\n' "$version" "$install_dir"
printf 'Next: conveyor init\n'
printf 'Ensure %s is on PATH.\n' "$install_dir"
