"""PCM to podcast-ready MP3 (ARCHITECTURE §3.5)."""

from __future__ import annotations

import json
import shutil
import subprocess
import tempfile
from pathlib import Path

import soundfile as sf
from mutagen.id3 import CHAP, CTOC, ID3, TALB, TIT2, TPE1, CTOCFlags

from .tts import Rendered

# Constant bitrate: podcast apps seek accurately in CBR MP3, less so in VBR.
BITRATE = "64k"
OUT_RATE = 44100


def encode_mp3(r: Rendered, dest: Path, title: str, album: str | None = None) -> None:
    ffmpeg = shutil.which("ffmpeg")
    if ffmpeg is None:
        raise RuntimeError("ffmpeg not found")
    with tempfile.TemporaryDirectory(prefix="notecast-audio-") as d:
        wav = Path(d, "in.wav")
        sf.write(wav, r.pcm, r.sample_rate, subtype="PCM_16")
        tmp = Path(d, "out.mp3")
        subprocess.run(
            [
                ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
                "-i", str(wav),
                "-af", "loudnorm=I=-16:TP=-1.5:LRA=11",
                "-ar", str(OUT_RATE), "-ac", "1",
                "-c:a", "libmp3lame", "-b:a", BITRATE,
                str(tmp),
            ],
            check=True,
            timeout=900,
        )  # fmt: skip
        write_tags(tmp, r, title, album)
        shutil.move(tmp, dest)


def write_tags(path: Path, r: Rendered, title: str, album: str | None) -> None:
    tags = ID3()
    tags.add(TIT2(encoding=3, text=title))
    tags.add(TPE1(encoding=3, text="notecast"))
    if album:
        tags.add(TALB(encoding=3, text=album))
    end_ms = int(r.seconds * 1000)
    ids = [f"chp{i}" for i in range(len(r.chapters))]
    tags.add(
        CTOC(
            element_id="toc",
            flags=CTOCFlags.TOP_LEVEL | CTOCFlags.ORDERED,
            child_element_ids=ids,
            sub_frames=[TIT2(encoding=3, text="Chapters")],
        )
    )
    for i, ch in enumerate(r.chapters):
        start = int(ch.start * 1000)
        end = int(r.chapters[i + 1].start * 1000) if i + 1 < len(r.chapters) else end_ms
        tags.add(
            CHAP(
                element_id=ids[i],
                start_time=start,
                end_time=end,
                sub_frames=[TIT2(encoding=3, text=ch.title)],
            )
        )
    tags.save(path, v2_version=3)


def chapters_json(r: Rendered) -> str:
    """Podcasting 2.0 JSON chapters (served alongside the MP3)."""
    return json.dumps(
        {
            "version": "1.2.0",
            "chapters": [{"startTime": round(c.start, 2), "title": c.title} for c in r.chapters],
        },
        indent=2,
    )
