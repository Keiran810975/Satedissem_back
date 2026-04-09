#!/usr/bin/env bash
set -euo pipefail

# Usage:
#   tools/benchmark_algorithms.sh [topo_file] [fragments_csv] [per_case_timeout_sec]
# Example:
#   tools/benchmark_algorithms.sh topofile/intervals1024.json 20,40,80,160 120

TOPO_FILE="${1:-topofile/intervals1024.json}"
FRAGMENTS_CSV="${2:-20,40,80,160}"
PER_CASE_TIMEOUT_SEC="${3:-120}"
ALGORITHMS=("epidemic" "satdissem" "rlnc" "fl_gossip")

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BIN_PATH="/tmp/sat-sim-bench"
OUT_DIR="$ROOT_DIR/tools/benchmarks"
STAMP="$(date +%Y%m%d_%H%M%S)"
OUT_CSV="$OUT_DIR/benchmark_${STAMP}.csv"

mkdir -p "$OUT_DIR"

if [[ ! -f "$ROOT_DIR/$TOPO_FILE" ]]; then
  echo "[ERROR] Topology file not found: $ROOT_DIR/$TOPO_FILE" >&2
  exit 1
fi

if ! [[ "$PER_CASE_TIMEOUT_SEC" =~ ^[0-9]+$ ]] || [[ "$PER_CASE_TIMEOUT_SEC" -le 0 ]]; then
  echo "[ERROR] per_case_timeout_sec must be a positive integer, got: $PER_CASE_TIMEOUT_SEC" >&2
  exit 1
fi

echo "[INFO] Building simulator binary..."
(cd "$ROOT_DIR" && go build -o "$BIN_PATH" .)

echo "algorithm,fragments,status,logical_time,events,wall_clock,redundancy_rate,timeout_sec" > "$OUT_CSV"

IFS=',' read -r -a FRAGMENTS <<< "$FRAGMENTS_CSV"

for frag in "${FRAGMENTS[@]}"; do
  frag_trimmed="$(echo "$frag" | xargs)"
  for alg in "${ALGORITHMS[@]}"; do
    echo "[RUN] alg=$alg fragments=$frag_trimmed topo=$TOPO_FILE timeout=${PER_CASE_TIMEOUT_SEC}s"

    OUTPUT_FILE="$(mktemp)"
    CMD_EXIT=0
    START_TS="$(date +%s)"

    set +e
    if command -v gtimeout >/dev/null 2>&1; then
      gtimeout "${PER_CASE_TIMEOUT_SEC}s" \
        "$BIN_PATH" -topo-file "$ROOT_DIR/$TOPO_FILE" -fragments "$frag_trimmed" -scheduler "$alg" \
        >"$OUTPUT_FILE" 2>&1
      CMD_EXIT=$?
    elif command -v timeout >/dev/null 2>&1; then
      timeout "${PER_CASE_TIMEOUT_SEC}s" \
        "$BIN_PATH" -topo-file "$ROOT_DIR/$TOPO_FILE" -fragments "$frag_trimmed" -scheduler "$alg" \
        >"$OUTPUT_FILE" 2>&1
      CMD_EXIT=$?
    else
      "$BIN_PATH" -topo-file "$ROOT_DIR/$TOPO_FILE" -fragments "$frag_trimmed" -scheduler "$alg" \
        >"$OUTPUT_FILE" 2>&1 &
      CMD_PID=$!
      ELAPSED=0
      while kill -0 "$CMD_PID" 2>/dev/null; do
        if (( ELAPSED >= PER_CASE_TIMEOUT_SEC )); then
          kill -TERM "$CMD_PID" 2>/dev/null || true
          sleep 1
          kill -KILL "$CMD_PID" 2>/dev/null || true
          wait "$CMD_PID" 2>/dev/null || true
          CMD_EXIT=124
          break
        fi
        sleep 1
        ELAPSED=$((ELAPSED + 1))
      done
      if (( CMD_EXIT != 124 )); then
        wait "$CMD_PID"
        CMD_EXIT=$?
      fi
    fi
    set -e

    END_TS="$(date +%s)"
    CASE_WALL_SEC=$((END_TS - START_TS))
    OUTPUT="$(cat "$OUTPUT_FILE")"
    rm -f "$OUTPUT_FILE"

    if (( CMD_EXIT == 124 )); then
      echo "[TIMEOUT] alg=$alg fragments=$frag_trimmed exceeded ${PER_CASE_TIMEOUT_SEC}s, mark as INCOMPLETE"
      echo "$alg,$frag_trimmed,TIMEOUT,\"\",,\">=${PER_CASE_TIMEOUT_SEC}s\",\"\",$PER_CASE_TIMEOUT_SEC" >> "$OUT_CSV"
      continue
    fi

    if (( CMD_EXIT != 0 )); then
      echo "[ERROR] alg=$alg fragments=$frag_trimmed exited with code $CMD_EXIT"
      echo "$alg,$frag_trimmed,ERROR,\"\",,\"${CASE_WALL_SEC}s\",\"\",$PER_CASE_TIMEOUT_SEC" >> "$OUT_CSV"
      continue
    fi

    STATUS_LINE="$(echo "$OUTPUT" | grep 'Status:' | head -n 1 || true)"
    if [[ "$STATUS_LINE" == *"COMPLETE"* ]]; then
      STATUS="COMPLETE"
    elif [[ "$STATUS_LINE" == *"INCOMPLETE"* ]]; then
      STATUS="INCOMPLETE"
    else
      STATUS="UNKNOWN"
    fi

    LOGICAL_TIME="$(echo "$OUTPUT" | grep 'Completion time (logical):' | sed 's/.*Completion time (logical): //' | head -n 1 || true)"
    if [[ -z "$LOGICAL_TIME" ]]; then
      LOGICAL_TIME="$(echo "$OUTPUT" | grep 'Final sim time (logical):' | sed 's/.*Final sim time (logical): //' | head -n 1 || true)"
    fi

    EVENTS="$(echo "$OUTPUT" | grep 'Total events processed:' | sed 's/.*Total events processed: //' | head -n 1 || true)"
    WALL="$(echo "$OUTPUT" | grep 'Wall-clock time:' | sed 's/.*Wall-clock time: //' | head -n 1 || true)"
    REDUNDANCY="$(echo "$OUTPUT" | grep 'Redundancy rate:' | sed 's/.*Redundancy rate: //' | sed 's/ (duplicate packets \/ total received)//' | head -n 1 || true)"

    echo "$alg,$frag_trimmed,$STATUS,\"$LOGICAL_TIME\",$EVENTS,\"$WALL\",\"$REDUNDANCY\",$PER_CASE_TIMEOUT_SEC" >> "$OUT_CSV"
  done
done

echo "[DONE] Benchmark saved: $OUT_CSV"
