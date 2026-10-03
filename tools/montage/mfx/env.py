"""Locating the ffmpeg toolchain.

The repo ships a Linux static build under .local/bin which cannot execute on
Windows, so we never trust that path. Resolution order is PATH, then the
winget package directory, then MFX_FFMPEG.
"""

from __future__ import annotations

import os
import shutil
import subprocess
from functools import lru_cache
from pathlib import Path

_WINGET_PACKAGES = Path(os.environ.get("LOCALAPPDATA", "")) / "Microsoft" / "WinGet" / "Packages"


def _winget_candidates(exe: str):
    if not _WINGET_PACKAGES.is_dir():
        return
    for pkg in _WINGET_PACKAGES.glob("Gyan.FFmpeg*"):
        yield from pkg.glob(f"*/bin/{exe}")


@lru_cache(maxsize=None)
def _resolve(exe: str) -> str:
    override = os.environ.get(f"MFX_{exe.split('.')[0].upper()}")
    if override and Path(override).is_file():
        return override

    found = shutil.which(exe)
    if found:
        return found

    for cand in _winget_candidates(exe):
        if cand.is_file():
            return str(cand)

    raise RuntimeError(
        f"{exe} not found. Install with:  winget install --id Gyan.FFmpeg --exact\n"
        f"or point MFX_{exe.split('.')[0].upper()} at the binary."
    )


def ffmpeg() -> str:
    return _resolve("ffmpeg.exe" if os.name == "nt" else "ffmpeg")


def ffprobe() -> str:
    return _resolve("ffprobe.exe" if os.name == "nt" else "ffprobe")


@lru_cache(maxsize=1)
def has_nvenc() -> bool:
    """Actually open the encoder, don't just look for it in -encoders.

    ffmpeg lists h264_nvenc whenever it was compiled with support, regardless
    of whether the installed driver can serve it. ffmpeg 9.0 needs nvenc API
    13.1 (driver 610+) and fails at encoder-open time on anything older, which
    surfaces as a confusing mid-render crash. A one-frame trial encode is the
    only honest test.
    """
    try:
        res = subprocess.run(
            [ffmpeg(), "-hide_banner", "-loglevel", "error",
             "-f", "lavfi", "-i", "testsrc2=size=256x256:rate=30",
             "-frames:v", "1", "-c:v", "h264_nvenc", "-f", "null", "-"],
            capture_output=True, text=True, timeout=60,
        )
    except Exception:
        return False
    return res.returncode == 0


def video_encoder_args(cq: int = 20, preset_cpu: str = "medium") -> list[str]:
    """Prefer NVENC when present. Falls back to x264 so the tool stays portable."""
    if has_nvenc():
        return ["-c:v", "h264_nvenc", "-preset", "p5", "-rc", "vbr", "-cq", str(cq), "-b:v", "0"]
    return ["-c:v", "libx264", "-preset", preset_cpu, "-crf", str(cq)]


def probe_duration(path: str | Path) -> float:
    out = subprocess.run(
        [ffprobe(), "-v", "error", "-show_entries", "format=duration",
         "-of", "default=noprint_wrappers=1:nokey=1", str(path)],
        capture_output=True, text=True, check=True,
    ).stdout.strip()
    return float(out)


def probe_stream(path: str | Path) -> dict:
    """Width/height/fps/duration for the first video stream."""
    import json

    out = subprocess.run(
        [ffprobe(), "-v", "error", "-select_streams", "v:0",
         "-show_entries", "stream=width,height,r_frame_rate,duration",
         "-show_entries", "format=duration", "-of", "json", str(path)],
        capture_output=True, text=True, check=True,
    ).stdout
    data = json.loads(out)
    st = (data.get("streams") or [{}])[0]
    num, _, den = (st.get("r_frame_rate") or "30/1").partition("/")
    fps = float(num) / float(den or 1)
    dur = st.get("duration") or data.get("format", {}).get("duration") or 0.0
    return {
        "width": int(st.get("width") or 0),
        "height": int(st.get("height") or 0),
        "fps": fps,
        "duration": float(dur),
    }
