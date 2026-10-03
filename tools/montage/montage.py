#!/usr/bin/env python
"""Beat-synced montage builder for gameplay-clipper.

    montage.py beats  <track.mp3>
    montage.py pool   [--game gow] [--status approved]
    montage.py plan   <track.mp3> -o plan.json [--game gow] [--pattern build]
    montage.py render <plan.json> -o out.mp4 [--preview]
    montage.py auto   <track.mp3> -o out.mp4 [--game gow] [--preview]

`plan` writes an editable EDL; `render` executes one. `auto` does both, which
is what you want for the first pass. Tune the JSON and re-render for the rest.
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

from mfx import arrange as arrange_mod
from mfx import beatgrid, pool
from mfx.edl import Montage, resolve
from mfx.env import probe_stream
from mfx.render import RenderOpts, render

REPO_ROOT = Path(__file__).resolve().parents[2]
DEFAULT_DB = REPO_ROOT / "out" / "clipper.db"


def _load_pool(args) -> list[pool.PoolClip]:
    db = Path(args.db)
    if not db.is_file():
        sys.exit(f"clip database not found: {db}\nRun the clipper first, or pass --db.")
    clips = pool.load(
        db, REPO_ROOT,
        game=args.game, archetype=args.archetype,
        status=args.status, min_score=args.min_score,
        limit=args.limit,
    )
    if not clips:
        sys.exit(
            f"no candidates matched (game={args.game}, archetype={args.archetype}, "
            f"status={args.status}, min_score={args.min_score}).\n"
            f"Try `montage.py pool --status ''` to see everything in the database."
        )
    return clips


def cmd_beats(args) -> None:
    g = beatgrid.analyse(args.track, refresh=args.refresh)
    bars = len(g.downbeats)
    print(f"{Path(g.path).name}")
    print(f"  tempo     {g.tempo:.1f} BPM")
    print(f"  duration  {g.duration:.1f}s")
    print(f"  beats     {len(g.beats)}  ({bars} bars detected)")
    if g.beats:
        print(f"  first 8   {', '.join(f'{b:.2f}' for b in g.beats[:8])}")
    strong = sorted(range(len(g.energy)), key=lambda i: g.energy[i], reverse=True)[:5]
    if strong:
        print(f"  strongest beats at {', '.join(f'{g.beats[i]:.1f}s' for i in sorted(strong))}")


def cmd_pool(args) -> None:
    clips = _load_pool(args)
    print(f"{len(clips)} candidate(s)\n")
    print(f"{'id':>5}  {'game':<6} {'archetype':<12} {'score':>5} {'win':>6} {'hit':>8}  title")
    for c in clips:
        score = c.llm_score if c.llm_score is not None else c.det_score
        print(f"{c.candidate_id:>5}  {c.game:<6} {c.archetype:<12} {score:>5.1f} "
              f"{c.window:>5.0f}s {c.hit_s:>7.1f}s  {(c.title or '')[:44]}")


def cmd_plan(args) -> Montage:
    clips = _load_pool(args)
    grid = beatgrid.analyse(args.track, refresh=args.refresh)
    m = arrange_mod.arrange(
        clips, grid,
        music=str(Path(args.track).resolve()),
        pattern=args.pattern,
        max_clips=args.max_clips,
        grade=args.grade,
        start_beat=args.start_beat,
        seed=args.seed,
        use_proxy=not args.full_res,
    )
    if args.out_plan:
        dst = Path(args.out_plan)
        dst.parent.mkdir(parents=True, exist_ok=True)
        dst.write_text(m.to_json())
        print(f"wrote {args.out_plan}  ({len(m.clips)} clips)")
    return m


def _resolve_and_report(m: Montage, refresh: bool = False):
    grid = beatgrid.analyse(m.music, refresh=refresh)
    rm = resolve(m, grid, probe_stream)
    print(f"{len(rm.clips)} clips, {rm.total_dur:.1f}s @ {grid.tempo:.0f} BPM")
    for c in rm.clips:
        tr = c.out_transition.type if c.out_transition else "-"
        print(f"  {c.index:>2}. {c.slot_start:>6.2f}s +{c.slot_dur:>5.2f}s "
              f"({c.spec.slot_beats}b)  in={c.in_point:>8.2f}s  {tr:<12} "
              f"{(c.spec.label or '')[:32]}")
    for w in rm.warnings:
        print(f"  warning: {w}", file=sys.stderr)
    return rm


def cmd_render(args) -> None:
    m = Montage.from_json(args.plan)
    rm = _resolve_and_report(m)
    out = Path(args.out)
    opts = RenderOpts(
        workdir=Path(args.workdir) if args.workdir else out.parent / ".montage_work",
        quality=args.quality, preview=args.preview, keep_temp=args.keep_temp,
    )
    render(rm, out, opts)
    print(f"\nwrote {out}  ({probe_stream(out)['duration']:.1f}s)")


def cmd_auto(args) -> None:
    m = cmd_plan(args)
    rm = _resolve_and_report(m)
    out = Path(args.out)
    opts = RenderOpts(
        workdir=Path(args.workdir) if args.workdir else out.parent / ".montage_work",
        quality=args.quality, preview=args.preview, keep_temp=args.keep_temp,
    )
    render(rm, out, opts)
    print(f"\nwrote {out}  ({probe_stream(out)['duration']:.1f}s)")


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest="cmd", required=True)

    def pool_args(sp):
        sp.add_argument("--db", default=str(DEFAULT_DB))
        sp.add_argument("--game")
        sp.add_argument("--archetype")
        sp.add_argument("--status", default="approved")
        sp.add_argument("--min-score", type=float, default=0.0)
        sp.add_argument("--limit", type=int)

    def plan_args(sp):
        sp.add_argument("track")
        sp.add_argument("--pattern", default="build", choices=list(arrange_mod.PATTERNS))
        sp.add_argument("--max-clips", type=int, default=8)
        sp.add_argument("--grade", default="punch")
        sp.add_argument("--start-beat", type=int, default=0)
        sp.add_argument("--seed", type=int, default=0)
        sp.add_argument("--full-res", action="store_true", help="use originals, not proxies")
        sp.add_argument("--refresh", action="store_true", help="re-run beat analysis")

    def render_args(sp):
        sp.add_argument("-o", "--out", required=True)
        sp.add_argument("--preview", action="store_true", help="half resolution, fast preset")
        sp.add_argument("--quality", type=int, default=20)
        sp.add_argument("--workdir")
        sp.add_argument("--keep-temp", action="store_true")

    sp = sub.add_parser("beats"); sp.add_argument("track")
    sp.add_argument("--refresh", action="store_true"); sp.set_defaults(func=cmd_beats)

    sp = sub.add_parser("pool"); pool_args(sp); sp.set_defaults(func=cmd_pool)

    sp = sub.add_parser("plan"); pool_args(sp); plan_args(sp)
    sp.add_argument("-o", "--out-plan", required=True); sp.set_defaults(func=cmd_plan)

    sp = sub.add_parser("render"); sp.add_argument("plan"); render_args(sp)
    sp.set_defaults(func=cmd_render)

    sp = sub.add_parser("auto"); pool_args(sp); plan_args(sp); render_args(sp)
    sp.add_argument("--out-plan"); sp.set_defaults(func=cmd_auto)

    args = p.parse_args()
    try:
        args.func(args)
    except (RuntimeError, ValueError, FileNotFoundError) as e:
        sys.exit(f"error: {e}")


if __name__ == "__main__":
    main()
