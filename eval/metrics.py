"""CER/WER for OCR output against hand-corrected ground truth.

Both sides are normalized first so the score measures reading, not
formatting: Markdown markup, uncertainty markers and whitespace are
stripped; case is kept (a misread capital is still a misread).
"""

from __future__ import annotations

import re

_MARKUP = re.compile(r"^\s{0,12}(#{1,6}\s+|[-*+]\s+|\d+[.)]\s+)", re.M)
_UNCERTAIN = re.compile(r"\[\?([^\]]*)\]")
# Diagram descriptions are free text that legitimately differs between
# readers; score only the handwriting.
_DIAGRAM = re.compile(r"\[diagram:[^\]]*\]", re.I)
_ARROWS = re.compile(r"\s*(?:->|→|⟶|=>)\s*")


def normalize(md: str) -> str:
    t = md.replace("\r", "")
    t = re.sub(r"<!--.*?-->", " ", t, flags=re.S)
    t = _DIAGRAM.sub(" ", t)
    t = _ARROWS.sub(" -> ", t)
    t = _MARKUP.sub("", t)
    t = _UNCERTAIN.sub(r"\1", t)
    t = t.replace("**", "").replace("__", "")
    return " ".join(t.split())


def edit_distance(a: list[str] | str, b: list[str] | str) -> int:
    prev = list(range(len(b) + 1))
    for i, x in enumerate(a, 1):
        cur = [i]
        for j, y in enumerate(b, 1):
            cur.append(min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + (x != y)))
        prev = cur
    return prev[-1]


def cer(hyp: str, ref: str) -> float:
    h, r = normalize(hyp), normalize(ref)
    return edit_distance(h, r) / max(len(r), 1)


def wer(hyp: str, ref: str) -> float:
    h, r = normalize(hyp).split(), normalize(ref).split()
    return edit_distance(h, r) / max(len(r), 1)
