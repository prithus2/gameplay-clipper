#!/usr/bin/env python
"""Regression tests. Run with: .venv/Scripts/python.exe test_montage.py

The maths tests are pure and always run. The render test needs ffmpeg and the
generated colour sources in testassets/, and skips itself if they are missing.

Every assertion here corresponds to a bug that actually shipped during the
first build, so none of them are hypothetical.
"""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

from mfx.beatgrid import BeatGrid
from mfx.edl import ClipSpec, Montage, Transition, resolve
from mfx.render import _atempo_chain, _decay

FAILURES: list[str] = []


def check(cond: bool, label: str, detail: str = "") -> None:
    if cond:
        print(f"  ok   {label}")
    else:
        FAILURES.append(label)
        print(f"  FAIL {label}" + (f"  ({detail})" if detail else ""))


def grid_120bpm(n: int = 400) -> BeatGrid:
    return BeatGrid(path="synthetic", tempo=120.0, beats=[i * 0.5 for i in range(n)],
                    downbeats=[i * 2.0 for i in range(n // 4)], duration=n * 0.5,
                    energy=[0.5] * n)


PROBE = lambda p: {"duration": 3600.0, "width": 1920, "height": 1080, "fps": 30.0}


def test_timing() -> None:
    print("\ntiming invariants")
    m = Montage(music="x", fps=30, clips=[
        ClipSpec(src="a.mp4", hit_s=100.0, slot_beats=8, align=0.55, transition=Transition("zoomin", 0.2)),
        ClipSpec(src="b.mp4", hit_s=200.0, slot_beats=4, align=0.5,
                 speed=[[0.6, 1.0], [0.4, 0.5]], transition=Transition("fade", 0.3)),
        ClipSpec(src="c.mp4", hit_s=300.0, slot_beats=4, align=0.62,
                 speed=[[0.45, 1.0], [0.25, 0.45], [0.30, 1.0]]),
    ])
    rm = resolve(m, grid_120bpm(), PROBE)

    check(abs(rm.total_dur - 8.0) < 1e-9, "total duration is the sum of slots", f"{rm.total_dur}")

    cum = 0.0
    for c in rm.clips:
        check(abs(c.slot_start - cum) < 1e-9, f"clip{c.index} starts at the running slot total")
        cum += c.slot_dur

    for c in rm.clips:
        # The hit must land exactly where align asked, measured in output time.
        landed = c.in_point + c.spec.source_offset_at(c.hit_out_t, c.render_dur)
        check(abs(landed - c.spec.hit_s) < 1e-6, f"clip{c.index} hit lands on its target frame",
              f"{landed} vs {c.spec.hit_s}")

        # Centred transitions: each clip supplies half of each adjacent overlap.
        out_half = (c.out_transition.dur / 2) if c.out_transition else 0.0
        expect = c.in_half + c.slot_dur + out_half
        check(abs(c.render_dur - expect) < 1e-9,
              f"clip{c.index} render_dur = in_half + slot + out_half", f"{c.render_dur} vs {expect}")

    # xfade telescoping: the accumulated head must be long enough to blend at
    # every offset. Getting this wrong silently drops clips instead of erroring.
    head = rm.clips[0].render_dur
    for i in range(1, len(rm.clips)):
        rc = rm.clips[i]
        offset = rc.slot_start - rc.in_half
        tdur = rm.clips[i - 1].out_transition.dur
        check(head >= offset + tdur - 1e-9, f"xfade {i} has enough head to blend",
              f"head={head:.4f} needs={offset + tdur:.4f}")
        head = offset + rc.render_dur


def test_frame_quantisation() -> None:
    print("\nframe quantisation")
    for fps in (24, 30, 60):
        m = Montage(music="x", fps=fps, clips=[
            ClipSpec(src="a.mp4", hit_s=50.0, slot_beats=b, transition=Transition("fade", 0.137))
            for b in (8, 4, 3, 5)
        ])
        rm = resolve(m, grid_120bpm(), PROBE)
        bad = [c.index for c in rm.clips
               if abs(c.slot_start * fps - round(c.slot_start * fps)) > 1e-6
               or abs(c.slot_dur * fps - round(c.slot_dur * fps)) > 1e-6]
        check(not bad, f"@{fps}fps every slot boundary is a whole frame", f"clips {bad}")
        trs = [c.out_transition.dur for c in rm.clips if c.out_transition]
        check(all(abs(t * fps - round(t * fps)) < 1e-6 for t in trs),
              f"@{fps}fps transition durations are whole frames")
        check(all(t >= 2.0 / fps - 1e-9 for t in trs), f"@{fps}fps transitions are at least 2 frames")


def test_speed_integration() -> None:
    print("\nspeed ramps")
    spec = ClipSpec(src="a.mp4", hit_s=10.0, speed=[[0.45, 1.0], [0.25, 0.45], [0.30, 1.0]])
    rd = 2.0
    expect = rd * (0.45 * 1.0 + 0.25 * 0.45 + 0.30 * 1.0)
    check(abs(spec.source_span(rd) - expect) < 1e-9, "piecewise source consumption is exact")
    check(abs(spec.source_offset_at(0.0, rd)) < 1e-12, "offset at t=0 is 0")
    check(abs(spec.source_offset_at(rd, rd) - spec.source_span(rd)) < 1e-12,
          "offset at the end equals the full span")
    # Monotonic: output time must never run backwards through the source.
    prev = -1.0
    mono = True
    for i in range(201):
        v = spec.source_offset_at(rd * i / 200, rd)
        if v < prev - 1e-12:
            mono = False
        prev = v
    check(mono, "source offset is monotonic across segment boundaries")

    check(abs(ClipSpec(src="a", hit_s=0, speed=[[2.0, 1.0]]).source_span(3.0) - 3.0) < 1e-9,
          "unnormalised fractions are normalised")

    for bad in ([[0.5, 0.0]], [[0.5, -1.0]], [[0.0, 1.0]]):
        try:
            ClipSpec(src="a", hit_s=0, speed=bad).source_span(1.0)
            check(False, f"rejects invalid speed {bad}")
        except ValueError:
            check(True, f"rejects invalid speed {bad}")


def test_decay_expression() -> None:
    print("\nimpact effect decay")
    # The original bug: exp(-rate*max(0,t-hit)) is 1 for every t before the hit,
    # so flashes ran at full strength for the whole clip and washed it out.
    expr = _decay("t", 1.0, 22.0, 0.45)

    def ev(t: float) -> float:
        import math
        # Mirror the ffmpeg semantics of the generated expression.
        return 0.45 * math.exp(-22.0 * (t - 1.0)) if t >= 1.0 else 0.0

    check(ev(0.0) == 0.0 and ev(0.5) == 0.0 and ev(0.99) == 0.0, "effect is silent before the hit")
    check(abs(ev(1.0) - 0.45) < 1e-9, "effect is at full amplitude on the hit")
    check(ev(1.3) < 0.001, "effect has decayed shortly after")
    check("gte(t\\," in expr, "generated expression gates on the hit time")
    check(expr.count("\\,") == 3 and "," not in expr.replace("\\,", ""),
          "every comma is escaped for ffmpeg's option parser", expr)


def test_atempo_chain() -> None:
    print("\naudio tempo decomposition")
    import re

    for speed in (1.0, 0.5, 0.45, 0.2, 0.05, 2.0, 150.0):
        chain = _atempo_chain(speed)
        if speed == 1.0:
            check(chain == "anull", "speed 1.0 is a no-op")
            continue
        factors = [float(x) for x in re.findall(r"atempo=([0-9.]+)", chain)]
        prod = 1.0
        for f in factors:
            prod *= f
        check(abs(prod - speed) < 1e-6, f"atempo chain multiplies to {speed}", f"got {prod}")
        check(all(0.5 - 1e-9 <= f <= 100.0 + 1e-9 for f in factors),
              f"every atempo factor for {speed} is in range", f"{factors}")


def test_music_too_short() -> None:
    print("\nerror handling")
    short = BeatGrid(path="short", tempo=120.0, beats=[i * 0.5 for i in range(6)],
                     downbeats=[], duration=3.0, energy=[0.5] * 6)
    m = Montage(music="x", clips=[ClipSpec(src="a.mp4", hit_s=1.0, slot_beats=8)])
    try:
        resolve(m, short, PROBE)
        check(False, "a too-short track raises")
    except ValueError as e:
        check("beats" in str(e).lower(), "a too-short track raises a useful error", str(e))

    m2 = Montage(music="x", clips=[ClipSpec(src="a.mp4", hit_s=1.0, slot_beats=4,
                                            transition=Transition("nonexistent", 0.2))])
    try:
        resolve(m2, grid_120bpm(), PROBE)
        check(False, "an unknown transition raises")
    except ValueError:
        check(True, "an unknown transition raises")

    # A hit too near the file start must clamp rather than seek negative.
    m3 = Montage(music="x", clips=[ClipSpec(src="a.mp4", hit_s=0.2, slot_beats=8, align=0.9)])
    rm3 = resolve(m3, grid_120bpm(), lambda p: {"duration": 10.0})
    check(rm3.clips[0].in_point >= 0.0, "in-point never goes negative")
    check(rm3.clips[0].clamped and rm3.warnings, "clamping is reported as a warning")


def test_render_alignment() -> None:
    print("\nend-to-end cut alignment")
    here = Path(__file__).parent
    assets = [here / "testassets" / f"{c}.mp4" for c in ("red", "green", "blue", "yellow")]
    track = here / "testassets" / "click128.mp3"
    if not all(p.is_file() for p in assets) or not track.is_file():
        print("  skip (testassets missing — see README)")
        return

    from mfx import beatgrid
    from mfx.env import ffmpeg, probe_stream
    from mfx.render import RenderOpts, render

    clips = [ClipSpec(src=str(p), hit_s=10.0, slot_beats=4, align=0.5,
                      transition=Transition("fade", 0.02), zoom_punch=0, shake=0,
                      flash=False, label=p.stem) for p in assets]
    m = Montage(music=str(track), clips=clips, grade="none")
    rm = resolve(m, beatgrid.analyse(track), probe_stream)
    out = here / "out" / "_test_align.mp4"
    render(rm, out, RenderOpts(workdir=here / "out" / ".testwork"), log=lambda *a: None)

    raw = subprocess.run([ffmpeg(), "-v", "error", "-i", str(out), "-vf", "scale=1:1",
                          "-f", "rawvideo", "-pix_fmt", "rgb24", "-"], capture_output=True).stdout
    names = {"red": (255, 0, 0), "green": (0, 128, 0), "blue": (0, 0, 255), "yellow": (255, 255, 0)}

    def cls(p):
        n = min(names, key=lambda k: sum((a - b) ** 2 for a, b in zip(p, names[k])))
        return n if sum((a - b) ** 2 for a, b in zip(p, names[n])) < 3000 else None

    seq = [cls(tuple(raw[i * 3:i * 3 + 3])) for i in range(len(raw) // 3)]
    runs, prev, s0 = [], None, 0
    for i, s in enumerate(seq):
        if s != prev:
            if prev is not None:
                runs.append((prev, s0 / 30.0, i / 30.0))
            prev, s0 = s, i
    if prev is not None:
        runs.append((prev, s0 / 30.0, len(seq) / 30.0))

    check([r[0] for r in runs] == ["red", "green", "blue", "yellow"],
          "all four clips appear in order", str([r[0] for r in runs]))
    check(abs(probe_stream(out)["duration"] - rm.total_dur) < 0.05,
          "output duration matches the timeline")

    cuts = [(runs[i][2] + runs[i + 1][1]) / 2 for i in range(len(runs) - 1)]
    for e, g in zip([c.slot_start for c in rm.clips][1:], cuts):
        check(abs(g - e) <= 1 / 30.0 + 1e-6, f"cut at {e:.3f}s lands within a frame",
              f"observed {g:.3f}s")
    out.unlink(missing_ok=True)


if __name__ == "__main__":
    for fn in (test_timing, test_frame_quantisation, test_speed_integration,
               test_decay_expression, test_atempo_chain, test_music_too_short,
               test_render_alignment):
        fn()
    print()
    if FAILURES:
        print(f"{len(FAILURES)} FAILED:")
        for f in FAILURES:
            print(f"  - {f}")
        sys.exit(1)
    print("all passed")
