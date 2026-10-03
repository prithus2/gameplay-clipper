"""EDL -> MP4.

Rendering is deliberately two-stage. Stage 1 turns each clip into a normalised
intermediate at the target resolution, frame rate and pixel format, with all
per-clip effects already baked in. Stage 2 does nothing but chain transitions
and lay the music down.

Doing it in one monolithic filter_complex is possible and a nightmare to debug:
one bad expression and ffmpeg reports a parse error with no indication of which
of forty clips caused it. Staging also means stage 1 output is cacheable, which
matters because you will re-render the same montage many times while tuning.

xfade also *requires* identical width/height/fps/pix_fmt across inputs and will
silently produce garbage on variable-frame-rate sources. Game captures are
almost always VFR, so the `fps` filter in stage 1 is load-bearing, not cosmetic.
"""

from __future__ import annotations

import hashlib
import shutil
import subprocess
from dataclasses import dataclass
from pathlib import Path

from .edl import XFADE_ALIAS, ResolvedClip, ResolvedMontage
from .env import ffmpeg, probe_stream, video_encoder_args

GRADES = {
    "none": "",
    "punch": "eq=contrast=1.12:saturation=1.28:gamma=0.98,unsharp=5:5:0.6,vignette=PI/5",
    "teal": "colorbalance=rs=-0.08:bs=0.10:rm=-0.04:bm=0.06,eq=contrast=1.14:saturation=1.15,vignette=PI/5",
    "warm": "colorbalance=rs=0.08:bs=-0.06,eq=contrast=1.10:saturation=1.22,vignette=PI/6",
    "crush": "eq=contrast=1.30:saturation=1.45:gamma=0.92,unsharp=5:5:0.9,vignette=PI/4",
}


def _atempo_chain(speed: float) -> str:
    """atempo only accepts 0.5..100, so extreme ramps need decomposition."""
    if abs(speed - 1.0) < 1e-6:
        return "anull"
    parts, s = [], speed
    while s < 0.5:
        parts.append(0.5)
        s /= 0.5
    while s > 100.0:
        parts.append(100.0)
        s /= 100.0
    parts.append(s)
    return ",".join(f"atempo={p:.6f}" for p in parts)


def _esc(v: float) -> str:
    return f"{v:.6f}"


def _decay(clock: str, hit: float, rate: float, amp: float) -> str:
    """amp*exp(-rate*(clock-hit)) after the hit, exactly 0 before it.

    The obvious `exp(-rate*max(0,t-hit))` is wrong: max() floors the exponent
    at 0, so the term is 1 — full strength — for the entire clip *leading up
    to* the hit, which is the opposite of an impact effect. Gate on the sign
    of (clock-hit) instead. Commas are escaped for ffmpeg's option parser.
    """
    return (f"if(gte({clock}\\,{_esc(hit)})\\,"
            f"{_esc(amp)}*exp(-{_esc(rate)}*({clock}-{_esc(hit)}))\\,0)")


def _stage1_filter(rc: ResolvedClip, width: int, height: int, fps: int) -> tuple[str, str, str]:
    """Build the per-clip filter graph. Returns (filter_complex, vlabel, alabel)."""
    spec = rc.spec
    segs = spec.speed_segments()

    # Split the trimmed source into constant-speed segments, then concat. Speed
    # is constant inside a segment so setpts stays a simple division, which is
    # far more robust than one piecewise expression across the whole clip.
    vparts, aparts, chains = [], [], []
    src_cursor = rc.in_point
    for j, (frac, speed) in enumerate(segs):
        seg_src = frac * rc.render_dur * speed
        s0, s1 = src_cursor, src_cursor + seg_src
        chains.append(
            f"[0:v]trim=start={_esc(s0)}:end={_esc(s1)},"
            f"setpts=(PTS-STARTPTS)/{_esc(speed)}[v{j}]"
        )
        chains.append(
            f"[0:a]atrim=start={_esc(s0)}:end={_esc(s1)},"
            f"asetpts=PTS-STARTPTS,{_atempo_chain(speed)}[a{j}]"
        )
        vparts.append(f"[v{j}]")
        aparts.append(f"[a{j}]")
        src_cursor = s1

    n = len(segs)
    if n > 1:
        chains.append(f"{''.join(vparts)}concat=n={n}:v=1:a=0[vcat]")
        chains.append(f"{''.join(aparts)}concat=n={n}:v=0:a=1[acat]")
        v, a = "[vcat]", "[acat]"
    else:
        v, a = "[v0]", "[a0]"

    # Force CFR before anything time-dependent. zoompan reasons in frame
    # numbers, so a variable frame rate here desyncs every effect below.
    steps = [f"fps={fps}"]

    # Reframe 16:9 -> 9:16 as a hard crop centred on the LLM's crop_focus,
    # rather than the blur-pad the single-clip renderer uses. In a montage the
    # frame is on screen for well under a second; a blurred pillar wastes the
    # only real estate you have.
    fx = min(max(spec.crop_focus[0], 0.0), 1.0)
    fy = min(max(spec.crop_focus[1], 0.0), 1.0)
    ar = width / height
    steps.append(
        f"crop=w='min(iw,ih*{_esc(ar)})':h='min(ih,iw/{_esc(ar)})'"
        f":x='clip(iw*{_esc(fx)}-ow/2,0,iw-ow)':y='clip(ih*{_esc(fy)}-oh/2,0,ih-oh)'"
    )
    steps.append(f"scale={width}:{height}:flags=bicubic")
    steps.append("setsar=1")

    # Zoom punch and shake, both firing on the hit frame and decaying out.
    hit = rc.hit_out_t
    if spec.zoom_punch > 0 or spec.shake > 0:
        clock = f"(on/{fps})"
        z = f"1+{_decay(clock, hit, 6.0, spec.zoom_punch)}" if spec.zoom_punch > 0 else "1"
        if spec.shake > 0:
            sh = _decay(clock, hit, 8.0, spec.shake)
            xs, ys = f"+{sh}*sin(on*2.7)", f"+{sh}*cos(on*3.1)"
        else:
            xs = ys = ""
        steps.append(
            f"zoompan=z='{z}':x='iw/2-(iw/zoom/2){xs}':y='ih/2-(ih/zoom/2){ys}'"
            f":d=1:s={width}x{height}:fps={fps}"
        )

    if spec.flash:
        steps.append(f"eq=brightness='{_decay('t', hit, 22.0, 0.45)}':eval=frame")

    # A flash *cut* blooms on the incoming clip's first frames rather than on
    # its hit. Baking it into this clip means the boundary stays a true hard
    # cut — nothing is stolen from either side's slot to pay for the effect.
    if rc.in_effect == "flash":
        steps.append(f"eq=brightness='{_decay('t', 0.0, 30.0, 0.5)}':eval=frame")

    grade = GRADES.get("__perclip__", "")
    if grade:
        steps.append(grade)

    steps.append("format=yuv420p")
    chains.append(f"{v}{','.join(steps)}[vout]")
    chains.append(f"{a}aresample=48000,aformat=sample_fmts=fltp:channel_layouts=stereo[aout]")
    return ";".join(chains), "[vout]", "[aout]"


@dataclass
class RenderOpts:
    workdir: Path
    quality: int = 20
    preview: bool = False   # half resolution, faster preset
    keep_temp: bool = False


def _clip_cache_key(rc: ResolvedClip, width: int, height: int, fps: int, grade: str) -> str:
    spec = rc.spec
    blob = "|".join([
        spec.src, _esc(rc.in_point), _esc(rc.source_dur), _esc(rc.render_dur),
        str(spec.speed), _esc(spec.zoom_punch), _esc(spec.shake), str(spec.flash),
        str(spec.crop_focus), f"{width}x{height}@{fps}", grade, rc.in_effect,
        _esc(rc.hit_out_t),
    ])
    return hashlib.sha256(blob.encode()).hexdigest()[:16]


def render_clips(rm: ResolvedMontage, opts: RenderOpts, *, log=print) -> list[Path]:
    m = rm.montage
    width, height, fps = m.width, m.height, m.fps
    if opts.preview:
        width, height = width // 2, height // 2

    grade = GRADES.get(m.grade, GRADES["punch"])
    GRADES["__perclip__"] = grade

    opts.workdir.mkdir(parents=True, exist_ok=True)
    outputs: list[Path] = []

    for rc in rm.clips:
        key = _clip_cache_key(rc, width, height, fps, grade)
        dst = opts.workdir / f"clip{rc.index:03d}_{key}.mp4"
        outputs.append(dst)
        if _usable(dst):
            log(f"  [{rc.index + 1}/{len(rm.clips)}] cached  {dst.name}")
            continue
        dst.unlink(missing_ok=True)

        fc, vlab, alab = _stage1_filter(rc, width, height, fps)
        # Render to a sidecar and rename on success, so an interrupted or
        # failed encode can never be picked up as a cache hit next run.
        tmp = dst.with_suffix(".part.mp4")
        tmp.unlink(missing_ok=True)
        cmd = [
            ffmpeg(), "-hide_banner", "-loglevel", "error", "-y",
            "-ss", _esc(max(0.0, rc.in_point - 2.0)),
            "-i", rc.spec.src,
            "-filter_complex", fc,
            "-map", vlab, "-map", alab,
            *video_encoder_args(opts.quality, "veryfast" if opts.preview else "medium"),
            "-c:a", "aac", "-b:a", "192k",
            # A few frames of tail beyond what the timeline needs. xfade
            # discards anything past offset+duration, so the guard is free, and
            # it means a one-frame rounding difference can never starve the
            # chain and silently drop the rest of the montage.
            "-t", _esc(rc.render_dur + 4.0 / fps),
            str(tmp),
        ]
        # The -ss above is a fast seek that lands up to 2s early; the trim
        # filters use absolute source timestamps, so re-base them.
        shift = max(0.0, rc.in_point - 2.0)
        if shift:
            cmd[cmd.index("-filter_complex") + 1] = _rebase(fc, shift)

        log(f"  [{rc.index + 1}/{len(rm.clips)}] {Path(rc.spec.src).name} "
            f"@{rc.in_point:.2f}s -> {rc.render_dur:.2f}s")
        _run(cmd)
        if not _usable(tmp):
            raise RuntimeError(
                f"clip {rc.index} produced an unreadable file — the source may be "
                f"shorter than the requested range ({rc.in_point:.2f}s + {rc.source_dur:.2f}s)"
            )
        tmp.replace(dst)

    return outputs


def _usable(path: Path) -> bool:
    """A file counts as usable only if ffprobe can read a duration from it."""
    if not path.is_file() or path.stat().st_size < 1024:
        return False
    try:
        return probe_stream(path)["duration"] > 0
    except Exception:
        return False


def _rebase(fc: str, shift: float) -> str:
    """Shift absolute trim timestamps to be relative to a fast-seek input."""
    import re

    def sub(m):
        return f"{m.group(1)}={float(m.group(2)) - shift:.6f}"

    return re.sub(r"\b(start|end)=(\d+\.?\d*)", sub, fc)


def render_final(rm: ResolvedMontage, clips: list[Path], out: Path,
                 opts: RenderOpts, *, log=print) -> Path:
    m = rm.montage
    n = len(clips)
    inputs: list[str] = []
    for p in clips:
        inputs += ["-i", str(p)]
    inputs += ["-ss", _esc(rm.music_offset), "-i", m.music]

    chains: list[str] = []

    # Trim every input to exactly its timeline length before joining. Stage 1
    # renders a guard tail, which xfade would ignore but concat would not.
    #
    # settb is not cosmetic: concat emits AVTB (1/1000000) while a raw decoded
    # link carries the container's timebase, and xfade hard-errors when its two
    # inputs disagree. Mixing concat and xfade in one chain makes that
    # collision reachable, so every link is pinned to AVTB up front.
    for i, rc in enumerate(rm.clips):
        chains.append(
            f"[{i}:v]trim=duration={_esc(rc.render_dur)},setpts=PTS-STARTPTS,"
            f"settb=AVTB[vt{i}]"
        )

    # Video: hard cuts concatenate, accents cross-fade.
    #
    # xfade(A,B,offset=O) emits A for [0,O), blends for [O,O+t), then B.
    # Accents are centred on the beat, so the blend starts half a transition
    # early: O = beat(i) - t/2. edl.resolve sizes each clip to supply exactly
    # that half, so the accumulated head is always beat(i)+t_in/2 long — which
    # is what xfade needs — whether the previous join was a concat or a blend.
    if n == 1:
        vlab = "[vt0]"
    else:
        cur = "[vt0]"
        for i in range(1, n):
            rc = rm.clips[i]
            tr = rm.clips[i - 1].out_transition
            lab = f"[vx{i}]"
            if tr is None or tr.is_hard:
                chains.append(f"{cur}[vt{i}]concat=n=2:v=1:a=0{lab}")
            else:
                ttype = XFADE_ALIAS.get(tr.type, tr.type)
                chains.append(
                    f"{cur}[vt{i}]xfade=transition={ttype}:duration={_esc(tr.dur)}"
                    f":offset={_esc(rc.slot_start - rc.in_half)}{lab}"
                )
            cur = lab
        vlab = cur

    # Audio mirrors the video join-for-join so game audio stays locked to
    # picture. Both acrossfade and concat derive their length from their
    # inputs, so the atrim above is what keeps the guard tail from drifting
    # audio a few frames later per clip.
    for i, rc in enumerate(rm.clips):
        chains.append(
            f"[{i}:a]atrim=duration={_esc(rc.render_dur)},asetpts=PTS-STARTPTS,"
            f"asettb=AVTB[at{i}]"
        )

    if n == 1:
        galab = "[at0]"
    else:
        cur = "[at0]"
        for i in range(1, n):
            tr = rm.clips[i - 1].out_transition
            lab = f"[ax{i}]"
            if tr is None or tr.is_hard:
                # A butt-join clicks on a waveform discontinuity; a 1ms ramp
                # is inaudible and removes it without softening the cut.
                chains.append(f"{cur}[at{i}]acrossfade=d=0.001:c1=tri:c2=tri{lab}")
            else:
                chains.append(f"{cur}[at{i}]acrossfade=d={_esc(tr.dur)}:c1=tri:c2=tri{lab}")
            cur = lab
        galab = cur

    total = _esc(rm.total_dur)
    chains.append(f"{galab}asplit=2[gmix][gkey]")
    chains.append(
        f"[{n}:a]atrim=duration={total},asetpts=PTS-STARTPTS,"
        f"volume={_esc(m.music_gain_db)}dB[mus]"
    )
    # Duck the music under game audio rather than the other way round: the
    # impacts are what sell the hit, the music is the bed.
    chains.append(
        "[mus][gkey]sidechaincompress=threshold=0.03:ratio=6:attack=15:release=300[musduck]"
    )
    chains.append(
        f"[musduck][gmix]amix=inputs=2:duration=first:normalize=0,"
        f"volume=1.4,loudnorm=I=-14:TP=-1.5:LRA=11[aout]"
    )
    chains.append(f"{vlab}trim=duration={total},setpts=PTS-STARTPTS[vout]")

    out.parent.mkdir(parents=True, exist_ok=True)
    cmd = [
        ffmpeg(), "-hide_banner", "-loglevel", "error", "-y",
        *inputs,
        "-filter_complex", ";".join(chains),
        "-map", "[vout]", "-map", "[aout]",
        *video_encoder_args(opts.quality, "veryfast" if opts.preview else "medium"),
        "-c:a", "aac", "-b:a", "192k",
        "-movflags", "+faststart",
        str(out),
    ]
    log(f"  composing {n} clips -> {out.name} ({rm.total_dur:.1f}s)")
    _run(cmd)
    return out


def _run(cmd: list[str]) -> None:
    res = subprocess.run(cmd, capture_output=True, text=True)
    if res.returncode != 0:
        tail = (res.stderr or "").strip().splitlines()
        raise RuntimeError(
            "ffmpeg failed:\n" + "\n".join(tail[-15:] or ["<no stderr>"])
        )


def render(rm: ResolvedMontage, out: Path, opts: RenderOpts, *, log=print) -> Path:
    log("stage 1: per-clip render")
    clips = render_clips(rm, opts, log=log)
    log("stage 2: transitions + music")
    result = render_final(rm, clips, out, opts, log=log)
    if not opts.keep_temp:
        shutil.rmtree(opts.workdir, ignore_errors=True)
    return result
