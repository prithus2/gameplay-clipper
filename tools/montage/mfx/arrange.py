"""Turning a pool of clips into an arrangement.

This is the taste layer, and it is deliberately the only module with opinions
in it. Everything it decides — order, slot lengths, which transition, where to
ramp — comes out as a plain EDL, so an LLM arranger can replace this file
wholesale without touching the renderer.

The one structural idea worth stating: a montage should accelerate. Long slots
early give the viewer time to read the scene; short slots later read as
intensity. Putting your best clip last and your second best first is an old
mixtape trick and it survives contact with 20-second vertical video.
"""

from __future__ import annotations

import random

from .beatgrid import BeatGrid
from .edl import CUT, FLASH, WHIP, ClipSpec, Montage, Transition
from .pool import PoolClip, prefer_proxy

# Slot patterns in beats. Index into these cyclically when a montage needs
# more clips than the pattern has entries.
PATTERNS = {
    "build":  [8, 8, 4, 4, 4, 4, 2, 2, 2, 2],
    "steady": [4],
    "rapid":  [2],
    "wave":   [8, 4, 2, 2, 4, 8, 4, 2, 2, 2],
}


def _slot_for(pattern: list[int], i: int, n: int) -> int:
    if len(pattern) == 1:
        return pattern[0]
    # Stretch the pattern across the montage so a 4-clip edit still arcs.
    return pattern[min(int(i / max(n - 1, 1) * (len(pattern) - 1)), len(pattern) - 1)]


def arrange(
    clips: list[PoolClip],
    grid: BeatGrid,
    *,
    music: str,
    pattern: str = "build",
    max_clips: int = 10,
    grade: str = "punch",
    start_beat: int = 0,
    seed: int = 0,
    use_proxy: bool = True,
) -> Montage:
    if not clips:
        raise ValueError("no clips to arrange")

    rng = random.Random(seed)
    pat = PATTERNS.get(pattern)
    if pat is None:
        raise ValueError(f"unknown pattern {pattern!r}; choose from {', '.join(PATTERNS)}")

    ranked = sorted(clips, key=lambda c: c.llm_score or c.det_score, reverse=True)[:max_clips]
    if not ranked:
        raise ValueError("no clips survived ranking")

    # Best clip closes the montage, second best opens it, the rest fill the
    # middle in descending order so quality sags in the middle, not at the ends.
    hero = ranked[0]
    rest = ranked[1:]
    ordered = ([rest[0]] if rest else []) + rest[1:][::-1] + [hero]

    n = len(ordered)
    specs: list[ClipSpec] = []
    for i, c in enumerate(ordered):
        slot = _slot_for(pat, i, n)
        is_hero = c is hero
        # Energy of the beat this clip *ends* on decides how hard we hit it.
        end_beat = start_beat + sum(_slot_for(pat, k, n) for k in range(i + 1))
        energy = grid.energy_at_beat(end_beat)

        if is_hero:
            # Ramp into the payoff, then snap back. This is the single most
            # recognisable "edited" gesture there is, and with hard cuts
            # everywhere it is doing most of the work.
            speed = [[0.40, 1.0], [0.28, 0.35], [0.32, 1.0]]
            align = 0.62
        elif slot <= 2:
            speed = [[1.0, 1.0]]
            align = 0.5
        else:
            speed = [[0.68, 1.0], [0.32, 0.6]] if energy > 0.55 else [[1.0, 1.0]]
            align = 0.55

        # Hard cut by default. An accent gets used only every few cuts, so it
        # still registers as an accent — a montage where every boundary is a
        # transition just reads as mush.
        if i == n - 1:
            transition = None
        elif i > 0 and i % 3 == 0 and energy > 0.5:
            transition = Transition(type=rng.choice([FLASH, WHIP]),
                                    dur=0.0 if slot <= 2 else 0.1)
        else:
            transition = Transition(type=CUT, dur=0.0)

        specs.append(ClipSpec(
            src=prefer_proxy(c) if use_proxy else c.src,
            hit_s=c.hit_s,
            slot_beats=slot,
            align=align,
            speed=speed,
            transition=transition,
            # Energy now has to come from within the frame, so push the
            # in-clip effects harder than when transitions were carrying it.
            zoom_punch=0.22 if (energy > 0.55 or is_hero) else 0.12,
            shake=8.0 if is_hero else (4.0 if energy > 0.6 else 0.0),
            flash=is_hero,
            crop_focus=list(c.crop_focus),
            label=c.title or f"{c.game}/{c.archetype}#{c.candidate_id}",
        ))

    return Montage(
        music=music,
        clips=specs,
        music_start_beat=start_beat,
        grade=grade,
        title=f"{ordered[0].game}-{pattern}",
    )
