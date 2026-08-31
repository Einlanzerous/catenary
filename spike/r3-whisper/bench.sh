#!/usr/bin/env bash
# R3 / IDEA-26 / CHRN-12 — whisper.cpp latency on the R9700 (gfx1201, RDNA4).
#
# Measures the two halves the user actually waits for, separately:
#   1. Opus decode  — ffmpeg, .opus -> 16 kHz mono s16 WAV, which is what
#      whisper wants. IDEA-26 is explicit that this counts.
#   2. Inference    — whisper-cli wall clock.
#
# Each combination gets a WARM-UP run that is timed and thrown away, then
# REPEATS timed runs, reported as a median. The first run of any model pays a
# cold page-cache cost that is not representative of a queue that has been up
# for a week.
#
# ---------------------------------------------------------------------------
# CHRN-12 changed four things. The first three the IDEA-26 correction implied
# but never landed in this file; the fourth is a defect in how the original
# numbers were taken.
#
#   1. Vulkan is a backend here, and the default. The original script only knew
#      about build-hip, which is why it could not reproduce the corrected
#      bench.csv sitting next to it.
#   2. CPU is measured through the VULKAN binary with -ng, never the HIP one.
#      Measuring the CPU path with the HIP build silently charges it ~3.2 s of
#      rocBLAS init, which inflated the first published CPU floor 2-3x.
#   3. The warm-up is explicit.
#   4. *** -nt (--no-timestamps) IS NO LONGER USED FOR TIMING. ***
#      The original script timed `-np -nt`. Suppressing timestamp tokens does
#      not just change formatting: it changes the decode. On voice60 it costs
#      45% of small.en's text and 12% of large-v3's, and it makes every model
#      18-63% faster because there is less to emit. Every number in the first
#      IDEA-26 table is therefore optimistic. Timestamps are now left on --
#      which is also what a real service wants, since segment boundaries are
#      the useful part -- and stripped afterwards when saving the transcript.
#      Set NT=1 to reproduce the old, biased path for comparison.
#
# Knobs: MODELS BACKENDS CLIP REPEATS OUTFILE WHISPER VULKAN_SDK NT
#        CLIPS_PER_PROC — >1 measures the amortised per-clip cost with the
#        model resident, i.e. what a long-running ASR service sees rather than
#        a CLI invoked once per file.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WHISPER="${WHISPER:-$HOME/tools/whisper.cpp}"
AUDIO="$HERE/audio"
OUT="$HERE/results"
REPEATS="${REPEATS:-3}"
CLIP="${CLIP:-voice60}"
MODELS="${MODELS:-base.en small.en medium.en large-v3}"
BACKENDS="${BACKENDS:-vulkan}"
CLIPS_PER_PROC="${CLIPS_PER_PROC:-1}"
NT="${NT:-0}"
OUTFILE="${OUTFILE:-$OUT/bench.csv}"
VULKAN_SDK="${VULKAN_SDK:-$HOME/tools/vulkan-sdk/1.4.357.1/x86_64}"
mkdir -p "$OUT"

command -v ffmpeg >/dev/null || { echo "ffmpeg missing"; exit 1; }

# Load guard. Even on the Vulkan path a lot of this is CPU-side -- the ffmpeg
# decode entirely, plus mel and tokenisation -- so a busy box quietly inflates
# every number. A CI runner at load 12 made large-v3 read 9.2x when the real
# figure on an idle box is materially better. Refuse rather than publish a
# contaminated median; MAXLOAD=99 to override.
MAXLOAD="${MAXLOAD:-3.0}"
_load=$(awk '{print $1}' /proc/loadavg)
if awk -v l="$_load" -v m="$MAXLOAD" 'BEGIN{exit !(l>m)}'; then
  echo "REFUSING: 1-min load average is $_load (max $MAXLOAD)." >&2
  echo "Benchmark numbers taken under load are not comparable. Wait, or set MAXLOAD." >&2
  exit 1
fi

# cli_for <backend> — which build serves this backend.
#   vulkan -> build-vk       hip -> build-hip       cpu -> build-vk -ng
cli_for() {
  case "$1" in
    hip) echo "$WHISPER/build-hip/bin/whisper-cli" ;;
    vulkan|cpu) echo "$WHISPER/build-vk/bin/whisper-cli" ;;
    *) echo "" ;;
  esac
}

now_ms() { date +%s%3N; }
median() { printf '%s\n' "$@" | sort -n | awk '{a[NR]=$1} END{ if(NR%2) print a[(NR+1)/2]; else print int((a[NR/2]+a[NR/2+1])/2) }'; }

# decode_ms <in.opus> <out.wav> — time an Opus -> 16 kHz mono WAV decode.
decode_ms() {
  local t0 t1
  t0=$(now_ms)
  ffmpeg -y -hide_banner -loglevel error -i "$1" -ac 1 -ar 16000 -c:a pcm_s16le "$2" || return 1
  t1=$(now_ms)
  echo $((t1 - t0))
}

# run_ms <backend> <model> <wav> <n_clips> — time one whisper-cli process
# transcribing the clip n_clips times. Wall clock, stdout -> .last_raw.
run_ms() {
  local backend="$1" model="$2" wav="$3" n="$4"
  local cli; cli=$(cli_for "$backend")
  local args=(-m "$model")
  local i; for ((i=0;i<n;i++)); do args+=(-f "$wav"); done
  [ "$backend" = cpu ] && args+=(-ng)
  [ "$NT" = 1 ] && args+=(-nt)
  local t0 t1 rc
  t0=$(now_ms)
  (
    if [ "$backend" != hip ]; then
      export LD_LIBRARY_PATH="$VULKAN_SDK/lib:${LD_LIBRARY_PATH:-}"
      # ggml enumerates the Intel iGPU as a second device; pin the R9700
      # rather than trusting enumeration order.
      export GGML_VK_VISIBLE_DEVICES=0
    fi
    "$cli" "${args[@]}"
  ) >"$OUT/.last_raw" 2>"$OUT/.last_stderr"
  rc=$?
  t1=$(now_ms)
  [ $rc -eq 0 ] || { echo "-1"; return 1; }
  echo $((t1 - t0))
}

# load_ms — whisper's own model-load time from the last run's stderr.
load_ms() { sed -n 's/.*load time = *\([0-9.]*\) ms.*/\1/p' "$OUT/.last_stderr" | head -1; }

echo "model,backend,decode_ms,infer_ms,total_ms,realtime_x" > "$OUTFILE"

src="$AUDIO/$CLIP.opus"
[ -f "$src" ] || { echo "clip $CLIP missing at $src"; exit 1; }
wav="$AUDIO/$CLIP.16k.wav"

# Decode timed on its own. The same wav feeds every inference run below, so
# decode is measured once and added to each row rather than re-timed per model.
dec=()
for _ in $(seq "$REPEATS"); do dec+=("$(decode_ms "$src" "$wav")"); done
dec_med=$(median "${dec[@]}")

dur=$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$wav" | cut -d. -f1)
echo "clip=$CLIP duration=${dur}s decode_median=${dec_med}ms repeats=$REPEATS clips_per_proc=$CLIPS_PER_PROC nt=$NT" >&2

for model in $MODELS; do
  mpath="$WHISPER/models/ggml-$model.bin"
  [ -f "$mpath" ] || { echo "skip $model (no $mpath)" >&2; continue; }
  for backend in $BACKENDS; do
    cli=$(cli_for "$backend")
    [ -x "$cli" ] || { echo "skip $model/$backend (no $cli)" >&2; continue; }

    run_ms "$backend" "$mpath" "$wav" "$CLIPS_PER_PROC" >/dev/null   # warm-up, discarded

    runs=()
    for _ in $(seq "$REPEATS"); do runs+=("$(run_ms "$backend" "$mpath" "$wav" "$CLIPS_PER_PROC")"); done
    wall_med=$(median "${runs[@]}")

    if [ "$CLIPS_PER_PROC" -gt 1 ]; then
      # Amortised: strip the one-off model load, divide by the clip count.
      ld=$(load_ms); ld=${ld:-0}
      inf_med=$(awk -v w="$wall_med" -v l="$ld" -v n="$CLIPS_PER_PROC" 'BEGIN{printf "%d", (w-l)/n}')
    else
      inf_med=$wall_med
    fi

    total=$((dec_med + inf_med))
    rt=$(awk -v d="$dur" -v t="$total" 'BEGIN{ if(t>0) printf "%.1f", d*1000/t; else print "0" }')
    echo "$model,$backend,$dec_med,$inf_med,$total,$rt" | tee -a "$OUTFILE"
    # Save a timestamp-stripped transcript for eyeballing quality.
    sed 's/^\[[^]]*\] *//' "$OUT/.last_raw" > "$OUT/transcript-$CLIP-$model-$backend.txt" 2>/dev/null
  done
done

rm -f "$OUT/.last_raw" "$OUT/.last_stderr"
echo
echo "--- $OUTFILE ---"
column -s, -t < "$OUTFILE"
