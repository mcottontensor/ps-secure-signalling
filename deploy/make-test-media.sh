#!/usr/bin/env bash
# Generates media for cmd/teststreamer: three VP8 simulcast layers (largest first, keyframe every
# second, like a live encoder) and an Opus tone. Needs ffmpeg with libvpx and libopus.
set -euo pipefail
out=${1:-testmedia}
secs=${SECS:-20}
mkdir -p "$out"
i=0
for spec in 1280x720:2500k 640x360:800k 320x180:250k; do
  size=${spec%%:*}; rate=${spec##*:}
  ffmpeg -hide_banner -loglevel error -y -f lavfi -i "testsrc2=size=${size}:rate=30" -t "$secs" \
    -c:v libvpx -b:v "$rate" -g 30 -keyint_min 30 -deadline realtime -cpu-used 8 -auto-alt-ref 0 \
    "$out/video_$i.ivf"
  i=$((i + 1))
done
ffmpeg -hide_banner -loglevel error -y -f lavfi -i "sine=frequency=440:sample_rate=48000" -t "$secs" \
  -c:a libopus -ac 2 -page_duration 20000 "$out/audio.ogg"
echo "wrote $out"
