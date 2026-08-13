#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
case $(uname -s) in Linux) os=linux ;; Darwin) os=darwin ;; *) echo "unsupported test host" >&2; exit 1 ;; esac
case $(uname -m) in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) echo "unsupported test host" >&2; exit 1 ;; esac
version=v9.8.7-test
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

latest_dir="$fixture_root/latest-bin"
CONVEYOR_REPOSITORY_URL="$base_url" CONVEYOR_INSTALL_DIR="$latest_dir" sh "$repo_root/install.sh"
test "$("$latest_dir/conveyor" --version)" = "conveyor version $version"
test "$("$latest_dir/conveyord" version)" = "conveyord $version"
"$latest_dir/conveyor" init --help >/dev/null

pinned_dir="$fixture_root/pinned-bin"
CONVEYOR_REPOSITORY_URL="$base_url" CONVEYOR_INSTALL_DIR="$pinned_dir" sh "$repo_root/install.sh" "$version"
test -x "$pinned_dir/conveyor"
test -x "$pinned_dir/conveyord"

printf 'old conveyor\n' > "$pinned_dir/conveyor"
printf 'old conveyord\n' > "$pinned_dir/conveyord"
printf 'corruption\n' >> "$asset_dir/conveyor_${version}_${os}_${arch}.tar.gz"
if CONVEYOR_REPOSITORY_URL="$base_url" CONVEYOR_INSTALL_DIR="$pinned_dir" sh "$repo_root/install.sh" "$version"; then
  echo "corrupted release unexpectedly installed" >&2
  exit 1
fi
test "$(cat "$pinned_dir/conveyor")" = "old conveyor"
test "$(cat "$pinned_dir/conveyord")" = "old conveyord"

echo "installer release, pinning, and checksum-failure checks passed"
