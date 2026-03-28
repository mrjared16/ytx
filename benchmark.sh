#!/bin/bash
# Benchmark script for ytx performance analysis
set -e

VIDEO_ID="${1:-SNES5Y-tYxM}"
ENGINES="${2:-bun}"

# Pool of video IDs for varied benchmarking
VIDEO_IDS=(SNES5Y-tYxM hbl2Cuw75oE dQw4w9WgXcQ 9bZkp7q19f0 kJQP7kiw5Fk)
video_idx=0

NEXT_VIDEO_ID=""

next_video_id() {
  NEXT_VIDEO_ID="${VIDEO_IDS[$video_idx]}"
  video_idx=$(((video_idx + 1) % ${#VIDEO_IDS[@]}))
}

echo "=============================================="
echo "YTX Performance Benchmark"
echo "Video ID: $VIDEO_ID"
echo "=============================================="
echo ""

run_benchmark() {
  local name="$1"
  local purge="$2"
  local mode="$3"
  local engine="$4"

  echo "--- $name ---"
  if [ "$purge" = "true" ]; then
    ./ytx cache purge >/dev/null 2>&1
    echo "Cache: PURGED (cold start)"
  else
    echo "Cache: WARM"
  fi

  next_video_id
  local vid="$NEXT_VIDEO_ID"
  echo "Video ID: $vid"

  if [ "$mode" = "video" ]; then
    result=$(./ytx video "$vid" --profile 2>&1)
  else
    result=$(./ytx music "$vid" --js-engine "$engine" --profile 2>&1)
  fi

  # Extract timings
  timings=$(echo "$result" | grep -o '"timings":{[^}]*}' | sed 's/"timings"://')
  if [ -n "$timings" ]; then
    echo "Timings: $timings"
  else
    echo "Error: $result" | head -c 200
  fi
  echo ""
}

# Build first
echo "Building ytx..."
make build-release
echo ""

# Video mode (no cipher needed)
echo "=============================================="
echo "VIDEO MODE (ANDROID_VR - no cipher)"
echo "=============================================="
run_benchmark "Video Cold" "true" "video" ""
run_benchmark "Video Warm" "false" "video" ""

# Music mode with each engine
for engine in $ENGINES; do
  echo "=============================================="
  echo "MUSIC MODE ($engine)"
  echo "=============================================="
  run_benchmark "Music Cold ($engine)" "true" "music" "$engine"
  run_benchmark "Music Warm ($engine)" "false" "music" "$engine"
  run_benchmark "Music Warm 2nd ($engine)" "false" "music" "$engine"
done

# Bulk test
echo "=============================================="
echo "BULK EXTRACTION (5 videos, warm cache)"
echo "=============================================="
./ytx cache purge >/dev/null 2>&1
next_video_id
./ytx music "$NEXT_VIDEO_ID" --js-engine bun >/dev/null 2>&1 # warm cache
echo "Running bulk extraction..."
time ./ytx music --bulk "$(
  IFS=,
  echo "${VIDEO_IDS[*]}"
)" --js-engine bun 2>&1 | wc -l
echo ""

echo "=============================================="
echo "BENCHMARK COMPLETE"
echo "=============================================="
