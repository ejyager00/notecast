# Changelog

Decisions that change where her notes go or how they're processed
(ARCHITECTURE §3.4 asks for these to be recorded).

- 2026-10-15: She agreed to OpenRouter for both stages: narration sends page
  transcripts, OCR sends the page photos, to whichever provider OpenRouter
  routes to, with `data_collection = "deny"`. Not switched yet (no API key);
  both stages are still local until the switch is made in worker.toml.
- 2026-10-15: Narration is written and stored per page (ARCHITECTURE §3.3,
  migration 0002).
- 2026-10-15: Switched both stages to OpenRouter. Eval on her 8-page note
  (3 pages against draft truth, all 8 against her corrected transcripts, and
  16 hard terms checked by hand), against the local qwen3-vl-8b on the CPU:

  | OCR | CER vs draft truth | hard terms right | s/page | $/page |
  |---|---|---|---|---|
  | qwen3-vl-8b 1536, local | 3.3% | 5/15 | ~400 | — |
  | GLM 5.3 Flash 1536 | 1.5% | 10/15 | 10 | 0.0015 |
  | GLM 5.3 Flash 2048 | 1.5% | 7/15 | 15 | 0.0023 |
  | Qwen3.8 27B 1536 | 1.9% | 9/15 | 27 | 0.0021 |
  | Qwen3.8 27B 2048 | 1.7% | 12/15 | 22 | 0.0035 |

  Her corrected transcripts favour the local model (they *are* its output
  minus her fixes); against them all cloud runs scored 2.0–2.4% CER to its
  1.3%, mostly from letter-level differences, while the cloud models got
  right several terms the local model misread and her review missed. OCR:
  Qwen3.8 27B at 2048. Narration (whole note): GLM 5.3 Flash 25 s, $0.003,
  most faithful; Qwen3.8 27B 240 s, $0.02, added small facts. Narration: GLM
  5.3 Flash. About 3¢ and 4 minutes per 8-page note, against ~70 CPU
  minutes. Cloud models never mark `[?word]` or `[illegible]`, so the review
  screen flags less; one run each, so the closer differences are noise.
