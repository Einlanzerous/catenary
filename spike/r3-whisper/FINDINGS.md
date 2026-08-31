# R3 — whisper.cpp on the R9700

**Gate:** IDEA-26 · **Status:** cleared · **Measured:** 2026-08-17

> *Exit criterion: a 60 s Opus clip transcribes at acceptable latency, with the working backend documented — the build flags, the ROCm/Mesa versions, and the measured wall-clock.*

**Working backend: Vulkan (RADV), `gfx1201`.** A 60 s voice note transcribes in **1.0 s end to end** with `small.en` — **59.6× realtime**, including the Opus decode.

> **Correction.** An earlier pass on this ticket recommended ROCm/HIP on the strength of it building cleanly first try. That recommendation was wrong. Building the Vulkan backend afterwards showed it is **4.8× faster** on the same model, and the HIP numbers had also inflated the CPU baseline. Both are corrected throughout. The ticket's own advice — try `GGML_VULKAN=1` first — was right, for a better reason than either of us had.

> **Second correction, CHRN-12 (2026-08-22). Every latency number in sections 1-6 is optimistic.**
> They were all timed with `-nt`, which suppresses timestamp tokens and thereby changes the
> *decode*, not merely the formatting. Correctly measured, `small.en` is **43.2×** realtime per
> invocation, not 59.6×. The backend conclusion (Vulkan) and the shape of the model curve both
> survive; the absolute figures do not. **See section 8, which supersedes section 1.**

---

## 1. Headline numbers

60 s clip, Opus 24 kbps mono (a realistic voice note — **182 KB**), decoded to 16 kHz mono WAV with ffmpeg. **Decode is timed separately and counted**, as the ticket requires. Median of 3, all three backends measured with one methodology after a warm-up run.

| model | backend | decode ms | inference ms | **total ms** | **× realtime** |
|---|---|---|---|---|---|
| base.en | **vulkan** | 152 | 625 | **777** | **77.2×** |
| base.en | hip | 152 | 3892 | 4044 | 14.8× |
| base.en | cpu | 152 | 2531 | 2683 | 22.4× |
| **small.en** | **vulkan** | 152 | 855 | **1007** | **59.6×** |
| small.en | hip | 152 | 4026 | 4178 | 14.4× |
| small.en | cpu | 152 | 7146 | 7298 | 8.2× |
| medium.en | **vulkan** | 152 | 1893 | **2045** | **29.3×** |
| medium.en | hip | 152 | 5129 | 5281 | 11.4× |
| medium.en | cpu | 152 | 25443 | 25595 | 2.3× |

**Note the row that should stop you: for `base.en`, HIP is slower than the CPU.** That is not a typo, and the next section is why.

## 2. Why HIP lost, and why it is not about the GPU

Two separate effects, both measured.

### A fixed ~3.6 s per-process initialisation tax

Isolated by transcribing a **1-second** clip, which is almost entirely fixed cost:

| backend | 1 s clip, `small.en` |
|---|---|
| HIP | **3611 ms** |
| Vulkan | **388 ms** |

HIP pays about **3.2 s of startup that Vulkan does not** — consistent with rocBLAS/hipBLAS kernel-library initialisation, which is large and eager. It is per *process*, so whisper.cpp invoked once per clip pays it every single time.

This also contaminated the first pass's **CPU** numbers, which were measured with `-ng` against the HIP build and so carried the ROCm init anyway:

| | via HIP build (first pass) | via Vulkan build (corrected) |
|---|---|---|
| base.en CPU | 5530 ms | **2531 ms** |
| small.en CPU | 10459 ms | **7146 ms** |

The CPU floor is considerably better than first reported. A backend's init tax showing up in a measurement of a *different* backend is a good argument for measuring everything through one binary.

### And Vulkan is ~3× faster at the actual compute

Startup is not the whole story. whisper's own encoder timer, which excludes process start and model load:

| backend | encode, per run |
|---|---|
| HIP | 88.80 ms |
| Vulkan | **29.34 ms** |

Decode 3.72 → 2.54 ms, batched decode 0.79 → 0.70 ms. Vulkan is ahead everywhere, not merely at startup.

The likely cause is that **RADV drives RDNA4's matrix cores and rocBLAS does not yet.** ggml reports `matrix cores: KHR_coopmat` on the Vulkan path, and `gfx1201` is new enough that rocBLAS's tuned-kernel coverage plausibly lags. That is a guess about mechanism; the measurement is not.

**So the ticket's instinct was right and its stated reason was incomplete.** It expected ROCm to be a *compatibility* risk ("a moving target", "garbage or a silent CPU fallback"). ROCm was in fact perfectly correct here — identical transcripts, genuine GPU use — it was simply **slow**. A backend that works and is 5× off the pace is a failure mode worth naming, because nothing about it looks like a failure.

## 3. Build recipes

Both backends built from whisper.cpp `1fe009caeda75f69bc864d6370b10674e45a92bd` (2026-08-14), ggml 0.20.0.

### Vulkan — the one to use

There is no `libvulkan-dev` on this box and no sudo, so the dev dependencies come from the LunarG SDK plus one symlink. No root required:

```bash
# Vulkan SDK (headers + glslc). ~330 MB, extracts anywhere.
curl -L -o vulkan_sdk.tar.xz https://sdk.lunarg.com/sdk/download/latest/linux/vulkan_sdk.tar.xz
mkdir -p ~/tools/vulkan-sdk && tar -xf vulkan_sdk.tar.xz -C ~/tools/vulkan-sdk
export VULKAN_SDK=$HOME/tools/vulkan-sdk/1.4.357.1/x86_64

# The system ships libvulkan.so.1 but not the .so dev symlink (that is
# libvulkan-dev's job). One local symlink stands in for the package.
mkdir -p ~/tools/vklib
ln -sf /usr/lib/x86_64-linux-gnu/libvulkan.so.1 ~/tools/vklib/libvulkan.so

cmake -S . -B build-vk -DCMAKE_BUILD_TYPE=Release -DGGML_VULKAN=1 \
  -DVulkan_INCLUDE_DIR=$VULKAN_SDK/include \
  -DVulkan_LIBRARY=$HOME/tools/vklib/libvulkan.so \
  -DVulkan_GLSLC_EXECUTABLE=$VULKAN_SDK/bin/glslc
cmake --build build-vk --config Release -j 12
```

At runtime: `export LD_LIBRARY_PATH=$VULKAN_SDK/lib:$LD_LIBRARY_PATH`.

ggml enumerates **two** Vulkan devices on this machine and picks the right one first, but do not rely on that — the Intel iGPU is device 1, and `GGML_VK_VISIBLE_DEVICES=0` pins it:

```
ggml_vulkan: 0 = AMD Radeon AI PRO R9700 (RADV GFX1201) (radv) | fp16: 1 | bf16: 1
             | warp size: 64 | matrix cores: KHR_coopmat
ggml_vulkan: 1 = Intel(R) UHD Graphics 630 (CML GT2)
```

RADV prints `WARNING: radv is not a conformant Vulkan implementation, testing use only` on every run. That is Mesa's standard notice about formal conformance certification, not a defect signal, and the transcripts are byte-identical to ROCm's.

### ROCm/HIP — kept for the record, not recommended

Configured and built with zero errors, first try. Worth documenting precisely because it *works*; it is just slower.

```bash
export PATH=/opt/rocm/bin:$PATH && export HIP_PATH=/opt/rocm
cmake -S . -B build-hip -DCMAKE_BUILD_TYPE=Release -DGGML_HIP=ON \
  -DAMDGPU_TARGETS=gfx1201 -DGPU_TARGETS=gfx1201 \
  -DCMAKE_C_COMPILER=/opt/rocm/llvm/bin/clang \
  -DCMAKE_CXX_COMPILER=/opt/rocm/llvm/bin/clang++
cmake --build build-hip --config Release -j 12
```

Both `AMDGPU_TARGETS` and `GPU_TARGETS` are set because whisper.cpp's CMake has moved between the names; naming the arch avoids building every gfx variant.

### Versions, so this is re-derivable

| | |
|---|---|
| GPU | AMD Radeon AI PRO R9700 · `gfx1201` (RDNA4, Navi 48) · 32 GB |
| **Mesa / RADV** | **25.2.8-0ubuntu0.24.04.2** · Vulkan API 1.4.318 |
| **Vulkan SDK** | **1.4.357.1** (glslc / shaderc v2026.3) |
| ROCm | 6.4.4-129 · HIP 6.4.43484 · AMD clang 19.0.0git (`roc-6.4.4`) |
| Kernel | 6.17.0-35-generic |
| whisper.cpp | `1fe009c`, ggml 0.20.0 |
| CPU (floor) | i7-10700K, 8C/16T |

## 4. The GPU is genuinely being used

The failure mode IDEA-26 names — compiles, runs, silently falls back to CPU — was checked rather than assumed, on the HIP build:

```
ggml_cuda_init: found 1 ROCm devices (Total VRAM: 32624 MiB):
  Device 0: AMD Radeon AI PRO R9700, gfx1201 (0x1201), VMM: no, Wave Size: 32
```

`rocm-smi --showuse` sampled at 4 Hz peaks at **89%** during a `small.en` run. On the Vulkan side, ggml names the RADV device explicitly and the result is **4.8× faster than the CPU path in the same binary** — not a fallback.

## 5. Recommendations

- **Backend: Vulkan.** 4.8× HIP on `small.en`, and the only one whose fixed cost is small enough not to dominate short clips.
- **Model: `small.en`** — 1.0 s for a 60 s note. **`medium.en` is now genuinely cheap** at 2.0 s / 29× realtime, which the HIP numbers would not have suggested. If transcript quality ever disappoints, medium is affordable.
- **CPU fallback: `base.en`** at 22.4× realtime — a perfectly usable floor. Do **not** fall back to the same model at lower speed: `medium.en` on CPU is 2.3× realtime and effectively non-viable.
- **Keep the ROCm build around** but do not ship it. If a future ROCm ships tuned `gfx1201` kernels this is worth re-measuring — the comparison is two commands.

### Quality

`small.en`, verbatim and unedited, from the Spoken Wikipedia clip:

> *"…he was again appointed as Prime Minister by President Manuel Pinto da Costa following the dismissal of Patrice Trovada, who had lost his parliamentary majority."*

Proper nouns, dates and ordinals correct on hard material — an encyclopedia article read aloud is considerably harder than a voice note from someone you know. Vulkan and HIP transcripts are identical, so the speed costs nothing.

## 6. What this means for P4

- **Transcription is nowhere near a latency risk.** A voice note is transcribed in ~1/60th of its own duration. The async job queue exists for isolation and retry, not because the work is slow.
- **`Transcript.eta_sec`** (R4 wire schema) can be honest: `duration/60` plus queue depth, and will usually round to 0–1 s.
- **`engine: "whisper.cpp/small.en"`** is already a wire field, so a transcript records what produced it — which matters now that both the model *and the backend* are demonstrably knobs.
- **A long-lived worker would erase the init tax**, if the backend choice ever gets revisited: ~3.2 s of HIP's deficit is per-process. On Vulkan it barely matters (0.39 s), which is one more reason to take the simple path.
- **IDEA-21 reuse is free** — same binary, same box, nothing Catenary-specific.

## 7. Caveats

- Single-stream. **Concurrent transcription was not measured**; several notes landing together will contend. Do not read 59× realtime as throughput.
- `.en` models only. Multilingual `small` will be slower.
- Warm-cache Release medians, after a warm-up run per backend.
- The mechanism behind Vulkan's compute win (coopmat vs untuned rocBLAS) is inference, not measurement. The numbers stand regardless of the explanation.

## 8. CHRN-12 — `large-v3`, and a defect in every number above

**Measured 2026-08-22.** CHRN-12 asked for one number: `large-v3` on this rig, because the
Chronicle design canvas asserts `whisper.cpp · large-v3 · 6.4× realtime` and nobody had ever
measured it. Getting it required fixing the harness first, and that turned up a defect in the
numbers this document already published.

### 8.1 The defect: `-nt` is not a formatting flag

`bench.sh` timed `whisper-cli -np -nt`. `-nt` / `--no-timestamps` reads like an output-formatting
option. It is not — it suppresses timestamp tokens during decoding, and that changes what the
model emits.

Two separate consequences, both measured on a quiet box, median of 3:

**It makes everything look faster,** because there are fewer tokens to emit:

| model | `-nt` (as published) | timestamps on | understated by |
|---|---|---|---|
| base.en | 591 ms | 814 ms | 38% |
| small.en | 845 ms | 1216 ms | 44% |
| medium.en | 1849 ms | 2210 ms | 20% |
| **large-v3** | 2842 ms | 4866 ms | **71%** |

**And it can silently drop audio.** Transcript text length, timestamps stripped, both clips:

| clip | base.en | small.en | medium.en | large-v3 |
|---|---|---|---|---|
| voice60 | 1% | **45%** | -1% | 12% |
| jfk60 | -1% | 5% | 10% | -6% |

On `voice60`, `-nt` costs `small.en` **45% of its transcript** — it loses the entire middle of the
clip. On `jfk60` the same flag costs nothing. So the content loss is real but **clip-dependent and
not predictable**, while the timing bias is consistent.

That it is a decode change rather than a print-path bug is easy to confirm: the `-nt` output is not
a subset of the timestamped output. It capitalises differently (`After Army unrest` vs
`after army unrest`), which means different tokens were generated.

**Nothing about this looks like a failure** — exit 0, plausible transcript, faster. It is the same
class of mistake as the HIP one corrected above: a result that is wrong while looking entirely
healthy. Timestamps are now on by default in `bench.sh`, stripped after the fact when saving
transcripts; `NT=1` reproduces the old path.

### 8.2 Corrected numbers

Vulkan/RADV, `gfx1201`. 60 s Opus clip, decode timed separately and counted (**149 ms**),
warm-up discarded, median of 3, 1-min load average below 3.0.

**Per invocation** — one `whisper-cli` process per clip, as a CLI-driven pipeline would run:

| model | inference | **total** | **× realtime** | previously published |
|---|---|---|---|---|
| base.en | 790 ms | **939 ms** | **63.9×** | 77.2× |
| small.en | 1241 ms | **1390 ms** | **43.2×** | 59.6× |
| medium.en | 2195 ms | **2344 ms** | **25.6×** | 29.3× |
| **large-v3** | 4617 ms | **4766 ms** | **12.6×** | never measured |

**Model resident** — 4 clips through one process, one-off model load subtracted. This is what a
long-running ASR service sees, and it is the number Chronicle E3 should size against:

| model | per clip | **total** | **× realtime** |
|---|---|---|---|
| base.en | 636 ms | **785 ms** | **76.4×** |
| small.en | 858 ms | **1007 ms** | **59.6×** |
| medium.en | 1481 ms | **1630 ms** | **36.8×** |
| **large-v3** | 3130 ms | **3279 ms** | **18.3×** |

Model load is why the two tables differ, and it scales with the file: 145 ms for `base.en`,
1909 ms for `large-v3`'s 3.1 GB. Keeping the model resident is worth **45%** of `large-v3`'s
wall clock and only 16% of `base.en`'s.

**CPU floor**, re-measured through the Vulkan binary with `-ng`:

| model | total | × realtime | previously published |
|---|---|---|---|
| base.en | 4196 ms | **14.3×** | 22.4× |
| small.en | 12108 ms | **5.0×** | 8.2× |

### 8.3 The canvas figure was pessimistic, not optimistic

`large-v3` runs at **12.6×** realtime per invocation and **18.3×** resident. The canvas guessed
**6.4×**. The real number is 2-3× better than the guess, so no offline-UX decision that assumed
6.4× needs loosening — several could be tightened.

### 8.4 `large-v3` is not worth it here, and not because of speed

The ticket anticipated a "10× latency cost" over `small.en`. The actual cost is **3.3×**
(3279 ms vs 1007 ms resident) — far cheaper than feared. The problem is that there is no quality
gain to pay for.

Across four decodes of `voice60` per model, on the hardest token in the clip — the name
*Fradique de Menezes*:

| model | what it produced |
|---|---|
| base.en | `Fradique de Menzés` ×4 — **first name correct, all four times** |
| small.en | `Fradique`, then `Fredique` ×2, `Fredique de Menzas` |
| medium.en | `Fredique de Menzies` ×4 — consistent, wrong |
| **large-v3** | `Fredike de Menses`, `Fredike de Menzies`, **`Frédéric de Menses`**, `Fredike de Menzies` |

`large-v3` is the **least** consistent and produces the worst single miss (`Frédéric`, a French
substitution — plausibly its multilingual training asserting itself on a Portuguese name). It does
get `Manuel Pinto da Costa` right, where `base.en` says `de Costa` all four times. Every model
misses `Patrice Trovoada` identically.

**Reading:** on English speech the `.en` models are specifically tuned and `large-v3` is not
obviously better. Paying 3.3× for it is not supported by this evidence.

**Do not over-read this.** It is one 60 s clip of encyclopedic read-aloud speech, four decodes per
model, scored by eye on a handful of proper nouns. It is enough to refuse an unjustified 3.3× cost;
it is **not** a quality evaluation. That needs the labelled set CHRN E4 already calls for.

### 8.5 Contention invalidates these numbers, quietly

The first CHRN-12 run landed while a CI runner had the box at load 12.8 and read **9.2×** for
`large-v3` against 12.6× on an idle machine — a 27% error, with nothing in the output to suggest
anything was wrong. Even on the GPU path the ffmpeg decode, mel and tokenisation are CPU-side.
`bench.sh` now refuses to run above a 1-min load average of 3.0 (`MAXLOAD` overrides).

### 8.6 The complete matrix

The HIP column and the two missing CPU rows were retaken on the corrected path (CHRN-12
follow-up). Same clip, decode counted (~150 ms), warm-up discarded, median of 3, 1-min load
average below 2.2 at the start of every phase.

**Per invocation** — one process per clip:

| model | vulkan | hip | cpu |
|---|---|---|---|
| base.en | **63.9×** | 13.4× | 14.3× |
| small.en | **43.2×** | 12.6× | 5.0× |
| medium.en | **25.6×** | 10.1× | 1.4× |
| large-v3 | **12.6×** | 7.1× | **0.6×** |

**Model resident** — 4 clips per process, one-off load subtracted:

| model | vulkan | hip | Vulkan's lead |
|---|---|---|---|
| base.en | **76.4×** | 36.7× | 2.08× |
| small.en | **59.6×** | 30.6× | 1.95× |
| medium.en | **36.8×** | 20.2× | 1.82× |
| large-v3 | **18.3×** | 15.2× | **1.20×** |

Three things fall out of this that the first pass could not have seen.

**Vulkan's advantage is mostly a per-process artefact, and it nearly vanishes for big models.**
Section 2 established HIP's ~3.6 s rocBLAS init tax and correctly called it per-process. What was
never measured is what happens when you stop paying it per clip. Resident, HIP's deficit collapses
from 4.8× to **1.2×–2.1×**, and it narrows monotonically as the model grows — because the fixed tax
is amortised against more compute. At `large-v3` the two backends are within 20% of each other.

**Vulkan still wins every single cell**, so the recommendation does not change. But *"4.8× faster
than HIP"* is a per-invocation number and should not be quoted at a service that holds its model
resident. For that deployment the honest figure is roughly **2×**, and for `large-v3` it is 1.2×.

**CPU beats HIP on `base.en`, and this was not an artefact of the bad methodology.** 14.3× against
13.4×. Section 2 flagged this as the row that should stop you; it survives correct measurement.

**The CPU floor collapses above `small.en`.** `medium.en` is 1.4× realtime and `large-v3` is
**0.6×** — 101 seconds to transcribe a 60-second clip, i.e. slower than listening to it. The
"fall back to `base.en`, never to the same model slower" rule is not a preference; above `small.en`
the CPU path stops being a fallback at all.

**Control.** The Vulkan column was re-run in the same session as the HIP phases, to confirm the
two are comparable rather than separated by a change in machine state: 61.9× / 42.9× / 24.5× /
12.1× against the 63.9× / 43.2× / 25.6× / 12.6× reported in 8.2. Within 4%, all slightly low, with
the Opus decode at 164 ms against 149 ms — the box had drifted busier by the last phase. So the
cross-backend comparison above is sound, and if anything it understates Vulkan by a few percent.

Still not retaken: HIP and CPU on the `-nt`-vs-timestamps comparison in section 8.1 (measured on
Vulkan only) — the flag's effect is a decoding property, not a backend one, so there is no reason
to expect it to differ, but it was not checked.


```
spike/r3-whisper/
  bench.sh                     harness — Vulkan default, warm-up, load guard,
                               timestamps on (CHRN-12); NT=1 for the old path
  audio/voice60.opus           60 s natural speech, Opus 24k mono
  audio/jfk60.opus             60 s looped jfk.wav, second sample
  audio/tiny1s.wav             1 s clip, used to isolate fixed init cost
  results/bench.csv            section 1 table — SUPERSEDED, measured with -nt
  results/bench-chrn12.csv     section 8.2, per invocation
  results/bench-chrn12-resident.csv  section 8.2, model resident
  results/bench-chrn12-cpu.csv       section 8.2, CPU floor
  results/bench-chrn12-hip.csv       section 8.6, HIP per invocation
  results/bench-chrn12-hip-resident.csv  section 8.6, HIP resident
  results/bench-chrn12-cpu-rest.csv  section 8.6, medium.en + large-v3 on CPU
  results/transcript-*.txt     every transcript produced, all backends
```
