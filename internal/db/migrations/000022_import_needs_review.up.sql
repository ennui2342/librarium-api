-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 fireball1725
--
-- Supports the import worker's new "needs_review" outcome for ambiguous
-- title-fallback matches (title matches an existing book but the row's
-- author doesn't overlap it, or more than one book shares the title).
-- Rather than silently guessing "new edition of this book" vs. "new book"
-- — which previously risked attaching an edition to the wrong book with no
-- way to tell after the fact — the worker now stops and records the
-- candidate books it found, and a human resolves the row via
-- POST .../items/{id}/resolve.
--
-- candidates is separate from the existing raw_data column: raw_data is
-- the original CSV row, candidates is the worker's own findings about that
-- row, so conflating the two would blur "what came in" with "what the
-- worker concluded".

ALTER TABLE import_job_items ADD COLUMN candidates JSONB;
ALTER TABLE import_jobs ADD COLUMN needs_review_rows INT NOT NULL DEFAULT 0;
