#!/bin/sh
set -eu

command -v python3 >/dev/null 2>&1 || {
  echo "installer test harness requires python3" >&2
  exit 1
}

repo_root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
case $(uname -s) in Linux) os=linux ;; Darwin) os=darwin ;; *) echo "unsupported test host" >&2; exit 1 ;; esac
case $(uname -m) in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) echo "unsupported test host" >&2; exit 1 ;; esac
version=v9.8.7-test
archive="conveyor_${version}_${os}_${arch}.tar.gz"
fixture_root=$(mktemp -d "${TMPDIR:-/tmp}/conveyor-release-test.XXXXXX")
port_file="$fixture_root/port"
server_pid=

cleanup() {
  if [ -n "$server_pid" ]; then kill "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true; fi
  case $fixture_root in
    "${TMPDIR:-/tmp}"/conveyor-release-test.*) rm -rf -- "$fixture_root" ;;
    *) echo "refusing to remove unexpected fixture path: $fixture_root" >&2 ;;
  esac
}
trap cleanup EXIT HUP INT TERM
printf 'installer test fixture: %s\n' "$fixture_root"

make -C "$repo_root" release VERSION="$version" RELEASE_DIR="$fixture_root/build" RELEASE_TARGETS="$os/$arch"
tar -tzf "$fixture_root/build/$archive" | grep -F "conveyor_${version}_${os}_${arch}/LICENSE" >/dev/null
asset_dir="$fixture_root/releases/download/$version"
mkdir -p "$asset_dir"
mv "$fixture_root/build/conveyor_${version}_${os}_${arch}.tar.gz" "$asset_dir/"
mv "$fixture_root/build/checksums.txt" "$asset_dir/"
rmdir "$fixture_root/build"

python3 - "$fixture_root" "$port_file" "$version" <<'PY' &
import http.server
import os
import sys

root, port_file, version = sys.argv[1:]
class Handler(http.server.SimpleHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/releases/latest":
            self.send_response(302)
            self.send_header("Location", "/releases/tag/" + version)
            self.end_headers()
            return
        if self.path == "/releases/tag/" + version:
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"fixture release")
            return
        super().do_GET()
    def log_message(self, _format, *_args):
        pass

os.chdir(root)
server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
with open(port_file, "w", encoding="utf-8") as handle:
    handle.write(str(server.server_port))
server.serve_forever()
PY
server_pid=$!

tries=0
while [ ! -s "$port_file" ]; do
  tries=$((tries + 1))
  [ "$tries" -lt 100 ] || { echo "fixture server did not start" >&2; exit 1; }
  sleep 0.05
done
base_url="http://127.0.0.1:$(cat "$port_file")"

truncated_dir="$fixture_root/truncated-bin"
script_bytes=$(wc -c < "$repo_root/install.sh")
sed '$d' "$repo_root/install.sh" > "$fixture_root/install-without-main.sh"
before_main_bytes=$(wc -c < "$fixture_root/install-without-main.sh")
for offset in "$((script_bytes / 4))" "$((script_bytes / 2))" "$before_main_bytes"; do
  dd if="$repo_root/install.sh" bs=1 count="$offset" 2>/dev/null |
    CONVEYOR_REPOSITORY_URL="$base_url" CONVEYOR_INSTALL_DIR="$truncated_dir" sh >/dev/null 2>&1 || true
  test ! -e "$truncated_dir"
done

latest_dir="$fixture_root/latest-bin"
cat "$repo_root/install.sh" |
  CONVEYOR_REPOSITORY_URL="$base_url" CONVEYOR_INSTALL_DIR="$latest_dir" sh
test "$("$latest_dir/conveyor" --version)" = "conveyor version $version"
test "$("$latest_dir/conveyord" version)" = "conveyord $version"
"$latest_dir/conveyor" init --help >/dev/null

pinned_dir="$fixture_root/pinned-bin"
CONVEYOR_REPOSITORY_URL="$base_url" CONVEYOR_INSTALL_DIR="$pinned_dir" sh "$repo_root/install.sh" "$version"
test -x "$pinned_dir/conveyor"
test -x "$pinned_dir/conveyord"

printf 'old conveyor\n' > "$pinned_dir/conveyor"
printf 'old conveyord\n' > "$pinned_dir/conveyord"
cp "$asset_dir/checksums.txt" "$fixture_root/exact-checksums.txt"
expected=$(awk -v archive="$archive" '$2 == archive { print $1 }' "$fixture_root/exact-checksums.txt")
near_archive=$(printf '%s\n' "$archive" | sed 's/\./X/g')
printf '%s  %s\n' "$expected" "$near_archive" > "$asset_dir/checksums.txt"
if CONVEYOR_REPOSITORY_URL="$base_url" CONVEYOR_INSTALL_DIR="$pinned_dir" sh "$repo_root/install.sh" "$version"; then
  echo "nonmatching checksum manifest unexpectedly installed" >&2
  exit 1
fi
test "$(cat "$pinned_dir/conveyor")" = "old conveyor"
test "$(cat "$pinned_dir/conveyord")" = "old conveyord"

cp "$fixture_root/exact-checksums.txt" "$asset_dir/checksums.txt"
printf 'corruption\n' >> "$asset_dir/$archive"
if CONVEYOR_REPOSITORY_URL="$base_url" CONVEYOR_INSTALL_DIR="$pinned_dir" sh "$repo_root/install.sh" "$version"; then
  echo "corrupted release unexpectedly installed" >&2
  exit 1
fi
test "$(cat "$pinned_dir/conveyor")" = "old conveyor"
test "$(cat "$pinned_dir/conveyord")" = "old conveyord"

test_shell=$(command -v sh)
mkdir "$fixture_root/pythonless-path"
if PATH="$fixture_root/pythonless-path" "$test_shell" "$repo_root/scripts/test-install.sh" 2> "$fixture_root/pythonless-error"; then
  echo "installer harness unexpectedly ran without python3" >&2
  exit 1
fi
grep -F "requires python3" "$fixture_root/pythonless-error" >/dev/null

grep -F 'ref: ${{ github.sha }}' "$repo_root/.github/workflows/release.yml" >/dev/null
if grep -F 'ref: ${{ github.ref }}' "$repo_root/.github/workflows/release.yml" >/dev/null; then
  echo "release checkout unexpectedly resolves the mutable tag ref" >&2
  exit 1
fi

echo "installer pipe safety, release pinning, checksum failure, and prerequisite checks passed"
