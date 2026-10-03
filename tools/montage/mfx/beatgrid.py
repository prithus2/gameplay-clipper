"""Beat grid extraction.

Everything in the montage timeline is expressed in beats, never seconds, so
this module is the clock the whole renderer runs on. Analysis is cached next
to the track because librosa import alone costs a couple of seconds.
"""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass, asdict
from pathlib import Path

CACHE_DIR = Path(__file__).resolve().parent.parent / ".cache" / "beatgrid"


@dataclass
class BeatGrid:
    path: str
    tempo: float
    beats: list[float]        # every detected beat, seconds
    downbeats: list[float]    # subset of beats estimated to start a bar
    duration: float
    energy: list[float]       # per-beat RMS, normalised 0..1

    def beat_index_at(self, t: float) -> int:
        import bisect
        return max(0, bisect.bisect_left(self.beats, t) - 1)

    def energy_at_beat(self, i: int) -> float:
        if not self.energy:
            return 0.5
        return self.energy[min(i, len(self.energy) - 1)]

    def to_json(self) -> str:
        return json.dumps(asdict(self), indent=2)


def _cache_key(path: Path) -> Path:
    st = path.stat()
    h = hashlib.sha256(f"{path.resolve()}|{st.st_size}|{int(st.st_mtime)}".encode()).hexdigest()[:16]
    return CACHE_DIR / f"{h}.json"


def analyse(path: str | Path, *, refresh: bool = False) -> BeatGrid:
    path = Path(path)
    if not path.is_file():
        raise FileNotFoundError(f"music track not found: {path}")

    cache = _cache_key(path)
    if cache.is_file() and not refresh:
        return BeatGrid(**json.loads(cache.read_text()))

    import numpy as np
    import librosa

    y, sr = librosa.load(str(path), mono=True)
    duration = float(len(y) / sr)

    onset_env = librosa.onset.onset_strength(y=y, sr=sr)
    tempo, beat_frames = librosa.beat.beat_track(onset_envelope=onset_env, sr=sr, units="frames")
    beats = librosa.frames_to_time(beat_frames, sr=sr).tolist()
    tempo = float(np.atleast_1d(tempo)[0])

    # Per-beat onset strength, used both for downbeat estimation and to let the
    # arranger put the loudest clips on the loudest beats.
    strengths = onset_env[beat_frames] if len(beat_frames) else np.array([])
    if strengths.size:
        lo, hi = float(strengths.min()), float(strengths.max())
        span = (hi - lo) or 1.0
        energy = ((strengths - lo) / span).tolist()
    else:
        energy = []

    # Downbeats: assume 4/4 and pick the phase whose beats carry the most
    # onset energy on average. Crude, but reliable on the electronic music
    # these edits are usually cut to, and it fails soft (worst case you get
    # bar lines one beat off, not a broken grid).
    downbeats: list[float] = []
    if len(beats) >= 4 and strengths.size:
        phase = max(range(4), key=lambda p: float(strengths[p::4].mean()))
        downbeats = beats[phase::4]

    grid = BeatGrid(
        path=str(path),
        tempo=tempo,
        beats=beats,
        downbeats=downbeats,
        duration=duration,
        energy=energy,
    )

    cache.parent.mkdir(parents=True, exist_ok=True)
    cache.write_text(grid.to_json())
    return grid
