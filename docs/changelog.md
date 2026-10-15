# Changelog

Decisions that change where her notes go or how they're processed
(ARCHITECTURE §3.4 asks for these to be recorded).

- 2026-10-15: She agreed to OpenRouter for both stages: narration sends page
  transcripts, OCR sends the page photos, to whichever provider OpenRouter
  routes to, with `data_collection = "deny"`. Not switched yet (no API key);
  both stages are still local until the switch is made in worker.toml.
- 2026-10-15: Narration is written and stored per page (ARCHITECTURE §3.3,
  migration 0002).
