"""Prototype of disagreement highlighting (ARCHITECTURE §11a).

Two models read the same page; words where they differ get highlighted for
review. With a truth file, also measure how well that works: of the primary
model's actual errors, how many sit on a flagged word (recall), and how many
flags are real errors (precision).

    python eval/disagree.py RUN_DIR_A RUN_DIR_B [--truth DIR] [--out REPORT.md]

RUN_DIR_* are ocr_bench output dirs (runs/<model>/<run>-<size>); A is the
primary reading shown to the user.
"""

from __future__ import annotations

import argparse
import difflib
from pathlib import Path

from metrics import normalize


def words(md: str) -> list[str]:
    return normalize(md).split()


def flagged_positions(a: list[str], b: list[str]) -> set[int]:
    """Indices in `a` whose word isn't matched identically in `b`."""
    sm = difflib.SequenceMatcher(a=a, b=b, autojunk=False)
    ok: set[int] = set()
    for blk in sm.get_matching_blocks():
        ok.update(range(blk.a, blk.a + blk.size))
    return set(range(len(a))) - ok


def highlight(a: list[str], b: list[str]) -> str:
    """A's text with disagreements shown as {A-words|B-words}."""
    sm = difflib.SequenceMatcher(a=a, b=b, autojunk=False)
    out: list[str] = []
    for op, i1, i2, j1, j2 in sm.get_opcodes():
        if op == "equal":
            out.extend(a[i1:i2])
        else:
            out.append("{" + " ".join(a[i1:i2]) + "|" + " ".join(b[j1:j2]) + "}")
    return " ".join(out)


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("a", type=Path)
    ap.add_argument("b", type=Path)
    ap.add_argument("--truth", type=Path)
    ap.add_argument("--out", type=Path)
    args = ap.parse_args()

    lines = [f"# Disagreement: `{args.a}` (primary) vs `{args.b}`", ""]
    tot = {"wrong": 0, "flagged": 0, "hit": 0, "words": 0}
    for fa in sorted(args.a.glob("*.md")):
        fb = args.b / fa.name
        if not fb.exists():
            continue
        a, b = words(fa.read_text()), words(fb.read_text())
        flags = flagged_positions(a, b)
        lines += [f"## {fa.stem}", "", highlight(a, b), ""]
        if args.truth and (ft := args.truth / fa.name).exists():
            wrong = flagged_positions(a, words(ft.read_text()))
            hit = len(wrong & flags)
            tot["wrong"] += len(wrong)
            tot["flagged"] += len(flags)
            tot["hit"] += hit
            tot["words"] += len(a)
            lines += [
                f"- words {len(a)}, wrong vs truth {len(wrong)}, flagged {len(flags)}, "
                f"wrong-and-flagged {hit}",
                f"- missed errors (wrong but both models agree): "
                f"{', '.join(a[i] for i in sorted(wrong - flags)) or 'none'}",
                "",
            ]
    if tot["words"]:
        recall = tot["hit"] / tot["wrong"] if tot["wrong"] else 1.0
        precision = tot["hit"] / tot["flagged"] if tot["flagged"] else 1.0
        lines[1:1] = [
            "",
            f"**Error recall {recall:.0%}** ({tot['hit']}/{tot['wrong']} of the primary's wrong "
            f"words were flagged); precision {precision:.0%}; flagged "
            f"{tot['flagged'] / tot['words']:.0%} of all words.",
        ]
    text = "\n".join(lines) + "\n"
    if args.out:
        args.out.write_text(text)
    print(text)


if __name__ == "__main__":
    main()
