# montage — beat-synced edits from the clipper's candidate pool

The Go pipeline finds *moments*. This turns a pool of them into a single
beat-synced vertical edit: multiple clips from different parts of a session,
cut on the beat, with transitions, speed ramps, impact effects and a music bed.

It reads `out/clipper.db` directly, so anything already approved in the review
UI is available as montage material with no re-detection.

## Setup

```bash
winget install --id Gyan.FFmpeg --exact
```

Then, from this directory:

```bash
python -m venv .venv && .venv/Scripts/python.exe -m pip install librosa soundfile numpy
```

ffmpeg is located via PATH, then the winget package directory, then
`MFX_FFMPEG`. The `.local/bin/ffmpeg` in the repo root is a **Linux** ELF
binary and is deliberately never used.

## Use

```bash
.venv/Scripts/python.exe montage.py auto track.mp3 --game gow -o out/reel.mp4
```

Add `--preview` for a half-resolution pass while you are tuning — it is several
times faster and every decision is still visible.

Other commands:

| command | does |
|---|---|
| `beats <track>` | tempo, beat count, where the strongest beats are |
| `pool` | what is available to cut from, with inferred hit times |
| `plan <track> -o plan.json` | write an EDL without rendering |
| `render <plan.json> -o out.mp4` | render an EDL you have edited |
| `auto` | plan + render in one go |

The two-step `plan` → edit JSON → `render` loop is the intended workflow once
you care about the result. `auto` is for the first pass.

## How it decides things

`arrange.py` is the only module with opinions in it. It ranks the pool, puts
the best clip last and the second best first, and accelerates the cut rate
across the montage (`--pattern build`, the default; also `steady`, `rapid`,
`wave`).

**The edit is built on hard cuts.** This is the important editorial decision
and the first version got it wrong. Transitions are not what makes a montage
feel professional — energy comes from cutting on the beat, speed ramps into
impacts, and zoom punch. A cross-dissolve long enough to perceive is just two
shots ghosting through each other, and anything with a recognisable *shape*
(circle, wipe, pixelate, squeeze, slide) reads as a slideshow no matter how
short you make it.

So the arranger picks from exactly three things:

| | |
|---|---|
| `cut` | true hard cut — the default, and most boundaries |
| `flash` | hard cut plus a white bloom on the incoming frames |
| `whip` | blur-through, the closest built-in to a whip pan |

An accent fires roughly every third cut and only on a high-energy beat, so it
still registers as an accent. All of ffmpeg's `xfade` types remain valid in a
hand-written EDL — they are simply never chosen automatically.

Everything it decides comes out as a plain EDL. To replace the taste layer —
with an LLM arranger, or your own heuristics — emit the same JSON. Nothing
downstream of `arrange.py` makes an editorial choice, and nothing in
`arrange.py` knows ffmpeg exists.

## The timing model

This is the part worth understanding before changing anything.

Each clip owns a **slot** measured in beats. Transitions are **centred** on the
beat rather than started on it, so the incoming shot lands *on* the beat
instead of arriving a transition-length late. That means each clip has to
supply half of each adjacent overlap:

```
render_dur(i)   = t_in/2 + slot(i) + t_out/2
xfade offset(i) = beat(i) - t_in/2
```

which telescopes exactly — the accumulated head is always `beat(i) + t_in/2`
long, precisely what `xfade` needs to blend at that offset.

Three constraints make this survive contact with ffmpeg, all of which were
found by rendering and measuring rather than by reasoning:

- **Beat times are snapped to frame boundaries.** ffmpeg emits whole frames, so
  an unquantised slot leaves each clip a fraction of a frame short, `xfade`
  starves, and every clip after the first is silently dropped. Costs at most
  half a frame of beat accuracy.
- **Transition durations are snapped to an *even* number of frames.** Each clip
  supplies `t/2`; with an odd frame count the halves do not add back up to `t`
  and the chain runs one frame short — the same failure, harder to see.
- **Stage 1 renders a few frames of guard tail**, and stage 2 trims every input
  back to its exact timeline length. `xfade` would ignore the tail (it works
  from an absolute offset) but `concat` and `acrossfade` derive their length
  from their inputs, so without the trim the montage drifts a few frames later
  per clip.
- **Every link is pinned to `AVTB`.** `concat` emits `1/1000000` while a raw
  decoded link carries the container's timebase, and `xfade` hard-errors when
  its two inputs disagree. Mixing hard cuts and accents in one chain makes that
  collision reachable.

Verified end-to-end in `test_montage.py` by rendering solid-colour sources and
measuring where the colour actually changes: every cut lands within half a
frame of its beat.

## Rendering

Two stages. Stage 1 normalises each clip to target resolution/fps/pixel format
with per-clip effects baked in; stage 2 only chains transitions and lays down
music. One monolithic filter graph would be a nightmare to debug — a bad
expression reports a parse error with no indication which of forty clips caused
it — and staging makes stage 1 cacheable, which matters because you will
re-render the same montage many times.

Per-clip: piecewise-constant speed ramps (`setpts` + an `atempo` chain), a hard
9:16 crop centred on the LLM's `crop_focus`, zoom punch and shake decaying out
of the hit frame, a flash, then the grade.

Impact effects gate on `gte(t, hit)`. The obvious `exp(-rate*max(0,t-hit))` is
wrong — `max` floors the exponent at 0, so the term is 1 for the *entire clip
before* the hit and the flash washes the whole thing out.

Final audio ducks the music under the game audio via `sidechaincompress`, then
normalises to -14 LUFS.

Grades: `punch` (default), `teal`, `warm`, `crush`, `none`.

## NVENC

Used when available, x264 otherwise. The check runs a real one-frame trial
encode rather than grepping `-encoders`: ffmpeg lists `h264_nvenc` whenever it
was *compiled* with support, regardless of driver. ffmpeg 9.0 needs nvenc API
13.1 (driver 610+) and otherwise fails mid-render with a confusing error.

## Tests

```bash
.venv/Scripts/python.exe test_montage.py
```

Maths tests always run. The end-to-end alignment test needs the generated
colour sources in `testassets/`; it skips itself if they are absent. Regenerate
them with `ffmpeg -f lavfi -i color=c=red:s=1920x1080:r=30:d=20 ...` per colour,
plus a click track.

Every assertion corresponds to a bug that shipped during the first build.

## Known limits

- **The pool is the ceiling.** The database currently holds only GoW
  `cutscene` candidates, so montages built today are boss-fight cinematics.
  Kill/fatality archetypes for `valo` and `mk` need a detection run before this
  produces the fast-cut edits those games are suited to.
- **`hit_s` is inferred**, not stored — it is the median time of the
  highest-confidence detector signals in the window. Persisting a real hit
  timestamp at detection time would be more accurate.
- **No whip pan.** `xfade` has no directional motion blur; `hblur` and
  `distance` are the closest stand-ins.
- **No captions.** `pkg/signal/asr.go` already computes and discards a
  timestamped transcript, so burned-in captions are mostly plumbing.
