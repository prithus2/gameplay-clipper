"""The candidate pool.

Reads the clip database that gameplay-clipper already produces. A montage is
just a query over this table plus an ordering, which is why this layer is thin:
the expensive work (detection, scoring, human review) already happened.
"""

from __future__ import annotations

import json
import sqlite3
import statistics
from dataclasses import dataclass
from pathlib import Path


@dataclass
class PoolClip:
    candidate_id: int
    session_id: str
    game: str
    archetype: str
    src: str            # absolute path to the source recording
    start_s: float
    end_s: float
    hit_s: float        # the moment the payoff lands — what gets put on a beat
    det_score: float
    llm_score: float | None
    title: str | None
    crop_focus: tuple[float, float]

    @property
    def window(self) -> float:
        return self.end_s - self.start_s


def _infer_hit(signals_json: str | None, start: float, end: float) -> float:
    """Locate the payoff frame inside a candidate window.

    The detector stores every signal it fired with a timestamp. The highest
    confidence signal is the best available proxy for "the moment"; when
    several tie we take their median so a burst of kill-feed hits collapses to
    the middle of the burst rather than its leading edge.
    """
    fallback = start + (end - start) * 0.6
    if not signals_json:
        return fallback
    try:
        events = json.loads(signals_json) or []
    except (json.JSONDecodeError, TypeError):
        return fallback

    times: list[float] = []
    best = -1.0
    for ev in events:
        if not isinstance(ev, dict):
            continue
        t, conf = ev.get("T"), ev.get("Confidence", 0)
        if t is None:
            continue
        try:
            t, conf = float(t), float(conf or 0)
        except (TypeError, ValueError):
            continue
        if conf > best:
            best, times = conf, [t]
        elif conf == best:
            times.append(t)

    if not times:
        return fallback
    hit = statistics.median(times)
    # Signal times are relative to the recording, so they should already sit
    # inside the window; clamp defensively rather than trusting that.
    return min(max(hit, start), end)


def load(
    db_path: str | Path,
    repo_root: str | Path,
    *,
    game: str | None = None,
    archetype: str | None = None,
    status: str = "approved",
    min_score: float = 0.0,
    limit: int | None = None,
) -> list[PoolClip]:
    db_path, repo_root = Path(db_path), Path(repo_root)
    con = sqlite3.connect(db_path)
    con.row_factory = sqlite3.Row

    sql = [
        "SELECT c.*, s.game, s.input_path FROM candidate c",
        "JOIN session s ON s.id = c.session_id",
        "WHERE 1=1",
    ]
    args: list = []
    if status:
        sql.append("AND c.status = ?")
        args.append(status)
    if game:
        sql.append("AND s.game = ?")
        args.append(game)
    if archetype:
        sql.append("AND c.archetype = ?")
        args.append(archetype)
    sql.append("ORDER BY COALESCE(c.llm_score, c.det_score) DESC")
    if limit:
        sql.append("LIMIT ?")
        args.append(limit)

    out: list[PoolClip] = []
    for row in con.execute(" ".join(sql), args):
        score = row["llm_score"] if row["llm_score"] is not None else row["det_score"]
        if score is not None and score < min_score:
            continue

        src = Path(row["input_path"])
        if not src.is_absolute():
            src = repo_root / src

        focus = (0.5, 0.5)
        if row["llm_metadata_json"]:
            try:
                meta = json.loads(row["llm_metadata_json"]) or {}
                cf = meta.get("crop_focus") or {}
                focus = (float(cf.get("x", 0.5)), float(cf.get("y", 0.5)))
            except (json.JSONDecodeError, TypeError, ValueError):
                pass

        out.append(PoolClip(
            candidate_id=row["id"],
            session_id=row["session_id"],
            game=row["game"],
            archetype=row["archetype"],
            src=str(src),
            start_s=float(row["start_s"]),
            end_s=float(row["end_s"]),
            hit_s=_infer_hit(row["signals_fired_json"], float(row["start_s"]), float(row["end_s"])),
            det_score=float(row["det_score"]),
            llm_score=float(row["llm_score"]) if row["llm_score"] is not None else None,
            title=row["llm_title"],
            crop_focus=focus,
        ))

    con.close()
    return out


def prefer_proxy(clip: PoolClip) -> str:
    """Use the 720p proxy when one exists.

    Iterating a montage against an 11 GB webm is miserable; the proxies the
    clipper already generates cut render time by an order of magnitude. Swap
    back to the original only for the final master.
    """
    src = Path(clip.src)
    proxy = src.with_name(f"{src.stem}_proxy.mp4")
    return str(proxy) if proxy.is_file() else str(src)
