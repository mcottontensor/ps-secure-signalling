#!/usr/bin/env bash
# Browser end-to-end checks with the stock frontend, wilbur, the SFU and teststreamer.
#   test/e2e/run.sh <path to the built frontend www>
# Needs Go, ffmpeg (libvpx, libopus), Node, and Chrome; installs Playwright and its Firefox on first run.
set -euo pipefail
www=$(realpath "${1:?usage: run.sh <www dir>}")
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
work=$(mktemp -d)
pids=()
cleanup() { for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done; }
trap cleanup EXIT

cd "$root"
go build -o "$work/bin/" ./cmd/...
SECS=20 deploy/make-test-media.sh "$work/media" >/dev/null
cd "$here"
[ -d node_modules/playwright ] || npm install --silent
npx playwright install firefox >/dev/null

cat > "$work/wilbur.yaml" <<YAML
listeners:
  player: { addr: "127.0.0.1:18080", serve_static: true }
  streamer: { addr: "127.0.0.1:18888" }
  sfu: { addr: "127.0.0.1:18889" }
static: { root: "$www" }
YAML
cat > "$work/sfu.yaml" <<YAML
signalling_url: ws://127.0.0.1:18889
ice: { udp_port: 50999 }
YAML

start() { local name=$1; shift; "$@" > "$work/$name.log" 2>&1 & pids+=($!); }
start wilbur "$work/bin/wilbur" -config "$work/wilbur.yaml"; sleep 0.5

page="AutoConnect=true&AutoPlayVideo=true&StartVideoMuted=true"
fail=0
run() { echo "== $*"; node play.cjs "$@" | tail -n 6 || fail=1; }

for mode in ps2 ps1; do
  flag=""; [ "$mode" = ps1 ] && flag=-ps1
  start "streamer-$mode" "$work/bin/teststreamer" -url ws://127.0.0.1:18888 -media "$work/media" $flag; sleep 1
  start "sfu-$mode" "$work/bin/sfu" -config "$work/sfu.yaml"; sleep 4
  for b in chrome firefox; do
    run "http://127.0.0.1:18080/?StreamerId=DefaultStreamer&$page" "$b" 12
    run "http://127.0.0.1:18080/?StreamerId=SFU&$page" "$b" 12 layers
  done
  kill "${pids[-1]}" "${pids[-2]}"; unset 'pids[-1]' 'pids[-1]'; sleep 1
done

echo "logs in $work"
[ $fail = 0 ] && echo "ALL PASSED" || { echo "FAILURES"; exit 1; }
