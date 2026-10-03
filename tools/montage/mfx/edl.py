"""The edit decision list, and the timing maths that resolves it.

This module is the contract between judgement and execution. An arranger (a
human, a heuristic, or an LLM) produces a Montage describing *intent* — which
clip, how many beats, which transition, how hard to ramp. Nothing here knows
anything about ffmpeg, and nothing in render.py makes an editorial decision.

The invariant everything else depends on:

    rendered duration of clip i == slot_i + transition_to_next_i

Because xfade consumes `transition` seconds of overlap, giving each clip that
much extra tail makes the timeline advance by exactly slot_i per clip. So slot
boundaries land on beat times no matter how many transitions are chained.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field, asdict
from pathlib import Path

from .beatgrid import BeatGrid

# A montage is built on HARD CUTS. Transitions are a rare accent, not the
# thing that makes the edit — energy comes from speed ramps, zoom punch and
# impact effects. Any transition with a recognisable *shape* (circle, wipe,
# pixelate, squeeze, slide) reads as a slideshow no matter how short it is,
# and a cross-dissolve long enough to see is just two shots ghosting.
#
# So the vocabulary the arranger draws from is deliberately tiny:
CUT = "cut"        # true hard cut — no blend at all, the default
FLASH = "flash"    # hard cut plus a white bloom on the incoming frame
WHIP = "whip"      # blur-through, the closest built-in to a whip pan

# Types that produce no blend; the chain concatenates instead of cross-fading.
HARD = {CUT, FLASH}

# What the arranger is allowed to choose. Everything else stays reachable by
# hand-editing an EDL, but is never selected automatically.
ACCENTS = [FLASH, WHIP]

# Everything ffmpeg's xfade can do, kept valid for hand-written EDLs.
XFADE_TYPES = [
    "fade", "fadeblack", "fadewhite", "dissolve", "pixelize", "radial",
    "hblur", "distance", "zoomin", "squeezeh", "squeezev", "circleclose",
    "circleopen", "slideleft", "slideright", "slideup", "slidedown",
    "wipeleft", "wiperight", "smoothleft", "smoothright", "hlslice", "hrslice",
]

TRANSITIONS = [CUT, FLASH, WHIP] + XFADE_TYPES

# whip is not an xfade primitive; hblur is the nearest thing ffmpeg ships.
XFADE_ALIAS = {WHIP: "hblur"}


@dataclass
class Transition:
    type: str = CUT
    dur: float = 0.0

    @property
    def is_hard(self) -> bool:
        return self.type in HARD

    def validate(self) -> None:
        if self.type not in TRANSITIONS:
            raise ValueError(f"unknown transition {self.type!r}; choose from {', '.join(TRANSITIONS)}")
        if not 0.0 <= self.dur <= 2.0:
            raise ValueError(f"transition duration {self.dur} out of range 0..2s")


@dataclass
class ClipSpec:
    src: str
    hit_s: float
    slot_beats: int = 4
    align: float = 0.55           # where in the slot the hit lands, 0..1
    speed: list[list[float]] = field(default_factory=lambda: [[1.0, 1.0]])
    transition: Transition | None = None
    zoom_punch: float = 0.0       # extra scale that decays after the hit
    shake: float = 0.0            # pixels of camera shake at the hit
    flash: bool = False           # white bloom on the hit frame
    crop_focus: list[float] = field(default_factory=lambda: [0.5, 0.5])
    label: str = ""

    def speed_segments(self) -> list[tuple[float, float]]:
        segs = [(float(f), float(s)) for f, s in self.speed]
        total = sum(f for f, _ in segs)
        if total <= 0:
            raise ValueError(f"speed fractions for {self.label or self.src} sum to {total}")
        if any(s <= 0 for _, s in segs):
            raise ValueError(f"non-positive speed in {self.label or self.src}")
        return [(f / total, s) for f, s in segs]

    def source_offset_at(self, out_t: float, render_dur: float) -> float:
        """Source seconds consumed by the time output reaches `out_t`."""
        remaining, consumed = out_t, 0.0
        for frac, speed in self.speed_segments():
            seg_out = frac * render_dur
            if remaining <= seg_out:
                return consumed + remaining * speed
            consumed += seg_out * speed
            remaining -= seg_out
        return consumed

    def source_span(self, render_dur: float) -> float:
        return self.source_offset_at(render_dur, render_dur)


@dataclass
class Montage:
    music: str
    clips: list[ClipSpec]
    music_start_beat: int = 0
    width: int = 1080
    height: int = 1920
    fps: int = 30
    grade: str = "punch"
    music_gain_db: float = 0.0
    duck_db: float = -9.0
    title: str = "montage"

    @staticmethod
    def from_json(path: str | Path) -> "Montage":
        raw = json.loads(Path(path).read_text())
        clips = []
        for c in raw.pop("clips", []):
            tr = c.pop("transition", None)
            clips.append(ClipSpec(transition=Transition(**tr) if tr else None, **c))
        return Montage(clips=clips, **raw)

    def to_json(self) -> str:
        d = asdict(self)
        for c in d["clips"]:
            if c["transition"] is None:
                c.pop("transition")
        return json.dumps(d, indent=2)


@dataclass
class ResolvedClip:
    spec: ClipSpec
    index: int
    slot_start: float      # timeline seconds, on a beat
    slot_dur: float        # timeline seconds this clip advances the montage
    render_dur: float      # slot_dur + outgoing transition overlap
    in_point: float        # source seconds
    source_dur: float      # source seconds consumed
    hit_out_t: float       # where the hit sits within render_dur
    out_transition: Transition | None
    in_half: float = 0.0   # half the incoming overlap this clip supplies
    in_effect: str = ""    # effect baked into this clip's first frames ("flash")
    clamped: bool = False  # True when the source was too short to honour align


@dataclass
class ResolvedMontage:
    montage: Montage
    clips: list[ResolvedClip]
    grid: BeatGrid
    total_dur: float
    music_offset: float

    @property
    def warnings(self) -> list[str]:
        return [
            f"clip {c.index} ({c.spec.label or Path(c.spec.src).name}) was clamped — "
            f"source too short to place the hit at align={c.spec.align}"
            for c in self.clips if c.clamped
        ]


def resolve(montage: Montage, grid: BeatGrid, probe) -> ResolvedMontage:
    """Turn beat counts into concrete source in/out points.

    `probe` is a callable returning {'duration': float} for a path — injected so
    this module stays free of ffmpeg imports and stays unit-testable.
    """
    beats = grid.beats
    if not beats:
        raise ValueError(f"no beats detected in {grid.path}")

    start = montage.music_start_beat
    needed = start + sum(c.slot_beats for c in montage.clips) + 1
    if needed > len(beats):
        raise ValueError(
            f"montage needs {needed} beats but {Path(grid.path).name} only has "
            f"{len(beats)} (~{grid.duration:.0f}s at {grid.tempo:.0f} BPM). "
            f"Shorten slots, drop clips, or use a longer track."
        )

    # Snap the whole grid to frame boundaries before any arithmetic.
    #
    # ffmpeg emits whole frames, so a slot of 1.8776s at 30fps becomes 56
    # frames (1.8667s) — fractionally shorter than the xfade offset computed
    # from the unquantised beat time. The chain then runs out of input and
    # silently drops every clip after the first. Quantising costs at most half
    # a frame of beat accuracy (16ms at 30fps, well below perception) and makes
    # the timing exact instead of approximately right.
    fps = montage.fps
    q = lambda t: round(t * fps) / fps

    # Transitions are *centred* on the beat, not started on it. Starting a
    # 0.3s dissolve on the beat means the incoming shot only fully arrives
    # 0.3s late, which reads as sloppy against the music. Centring means each
    # clip has to supply half the incoming overlap and half the outgoing one:
    #
    #     render_dur(i) = t_in/2 + slot(i) + t_out/2
    #     xfade offset(i) = beat(i) - t_in/2
    #
    # which telescopes exactly: the accumulated head is always beat(i)+t_in/2
    # long, precisely what xfade needs to blend at that offset.
    last = len(montage.clips) - 1
    trs: list[Transition | None] = []
    for i, spec in enumerate(montage.clips):
        # Validate every transition, including the trailing clip's unused one,
        # so a typo is reported instead of silently ignored.
        if spec.transition:
            spec.transition.validate()
        if i == last:
            trs.append(None)
            continue
        tr = spec.transition or Transition(CUT, 0.0)
        if tr.is_hard:
            # No blend: the clips are concatenated and the boundary is a true
            # cut. Any effect (a flash) is baked into the incoming clip, so
            # nothing is stolen from either side's slot.
            tr.dur = 0.0
        else:
            # Snap to an EVEN number of frames, minimum two. Each clip supplies
            # half of the overlap, so an odd frame count would make in_half +
            # out_half fail to add back up to the transition duration, and the
            # chain would run one frame short — the same starvation failure as
            # unquantised beats, just harder to see.
            tr.dur = max(2, round(tr.dur * fps / 2) * 2) / fps
        spec.transition = tr
        trs.append(tr)

    resolved: list[ResolvedClip] = []
    cursor = start
    for i, spec in enumerate(montage.clips):
        slot_start = q(beats[cursor] - beats[start])
        slot_end = q(beats[cursor + spec.slot_beats] - beats[start])
        slot_dur = slot_end - slot_start

        out_tr = trs[i]
        # Exact halves — transition durations are even frame counts above.
        in_half = (trs[i - 1].dur / 2) if i > 0 and trs[i - 1] else 0.0
        out_half = (out_tr.dur / 2) if out_tr else 0.0
        render_dur = in_half + slot_dur + out_half

        hit_out_t = in_half + max(0.0, min(spec.align, 1.0)) * slot_dur
        lead = spec.source_offset_at(hit_out_t, render_dur)
        source_dur = spec.source_span(render_dur)
        in_point = spec.hit_s - lead

        src_total = float(probe(spec.src)["duration"])
        clamped = False
        if in_point < 0:
            in_point, clamped = 0.0, True
        if in_point + source_dur > src_total:
            in_point, clamped = max(0.0, src_total - source_dur), True

        resolved.append(ResolvedClip(
            spec=spec, index=i,
            slot_start=slot_start,
            slot_dur=slot_dur, render_dur=render_dur,
            in_point=in_point, source_dur=source_dur,
            hit_out_t=hit_out_t, out_transition=out_tr,
            in_half=in_half, clamped=clamped,
            in_effect=(trs[i - 1].type if i > 0 and trs[i - 1] and trs[i - 1].type == FLASH else ""),
        ))
        cursor += spec.slot_beats

    return ResolvedMontage(
        montage=montage,
        clips=resolved,
        grid=grid,
        total_dur=q(beats[cursor] - beats[start]),
        music_offset=beats[start],
    )
