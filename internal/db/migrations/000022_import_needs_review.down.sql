-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 fireball1725

ALTER TABLE import_jobs DROP COLUMN IF EXISTS needs_review_rows;
ALTER TABLE import_job_items DROP COLUMN IF EXISTS candidates;
