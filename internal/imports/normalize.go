// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

// Package normalize collects pure-function CSV-to-Librarium converters
// shared between the import service and worker. Lives outside the
// worker package so it can be unit-tested without spinning up Postgres
// or River.
//
// Source-aware mapping is intentionally absent: the normalizers are
// broad whitelists that recognise values from Goodreads, StoryGraph,
// and Libib in a single pass. Their value sets are mostly disjoint
// (Libib uses Title-Case English; Goodreads uses kebab-case shelf
// names) so disambiguation falls out for free, and the importer
// doesn't need a source hint at this layer.
package imports

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ReadStatus maps a CSV value to one of Librarium's canonical
// `user_book_interactions.read_status` values: `unread`, `reading`,
// `read`, `did_not_finish`. Returns "" when the input doesn't match
// any known status — caller should treat that as "leave existing
// status alone" rather than overwriting with an empty value.
func ReadStatus(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		return ""
	}
	switch v {
	// ── Read / completed ──────────────────────────────────────────
	// Goodreads + StoryGraph: "read"
	// Libib: "Completed", and historically the user-rating-only rows
	// implicitly meant "completed", though that decision is left to
	// the caller (we only mark `read` when the field is explicit).
	case "read", "completed", "finished":
		return "read"

	// ── Currently reading ─────────────────────────────────────────
	// Goodreads + StoryGraph: "currently-reading"
	// Libib: "In Progress"
	case "reading", "currently-reading", "currently reading", "in progress":
		return "reading"

	// ── Did not finish ────────────────────────────────────────────
	// StoryGraph: "did-not-finish"
	// Libib: "Abandoned"
	case "did_not_finish", "did-not-finish", "abandoned", "dnf":
		return "did_not_finish"

	// ── Unread / to-read / on hold ────────────────────────────────
	// Goodreads + StoryGraph: "to-read"
	// Libib: "Not Begun", "On Hold"
	// All collapse to `unread` — Librarium tracks "want to read" via
	// shelves rather than a distinct status. Importers that want to
	// preserve "want to read" should also create a shelf and add the
	// book to it; that's a layer above this normalizer.
	case "unread", "to-read", "to read", "not begun", "want to read", "on hold":
		return "unread"
	}
	return ""
}

// Rating converts a CSV rating to Librarium's 1–10 integer scale.
// Sources differ: Libib uses 0–5 with half-step precision (`5.0`,
// `4.5`), Goodreads uses 1–5 whole, StoryGraph 1–5 quarter-step
// (`3.25`). We accept any numeric in [0, 5] and double + round to
// produce 1–10. Returns (value, true) on success; (0, false) when
// the input is empty, unparseable, or out of range. A rating of 0
// is also treated as "no rating" since both Libib and StoryGraph use
// 0 to mean "unrated" rather than literal zero stars.
func Rating(raw string) (int, bool) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return 0, false
	}
	// Strip a trailing " stars" / "/5" if a source ever emits one.
	v = strings.TrimSuffix(v, " stars")
	v = strings.TrimSuffix(v, "/5")
	v = strings.TrimSpace(v)

	var f float64
	if _, err := fmt.Sscanf(v, "%f", &f); err != nil {
		return 0, false
	}
	if f <= 0 || f > 5 {
		return 0, false
	}
	// Round half-up to the nearest 0.5, then double to get 1–10.
	half := int((f * 2) + 0.5)
	if half < 1 || half > 10 {
		return 0, false
	}
	return half, true
}

// Date parses a CSV date in any of the formats the three target
// trackers emit. Goodreads writes `2024/03/15`; StoryGraph writes
// `2024-03-15`; Libib uses `2024-03-15` and sometimes `2024-03` for
// month-only entries. Year-only is also accepted because "added"
// timestamps in older exports can degrade to that.
func Date(raw string) (time.Time, bool) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return time.Time{}, false
	}
	// Goodreads' format uses `/` separators; everything else is `-`.
	v = strings.ReplaceAll(v, "/", "-")
	for _, layout := range []string{
		"2006-01-02",
		"2006-01-02 15:04:05",
		"2006-01",
		"2006",
		"January 2, 2006",
		"Jan 2, 2006",
	} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// Bool maps common CSV truthy/falsy values to a Go bool. Returns
// (value, true) when recognised, (false, false) when unparseable.
// Used for the optional `is_favorite` import field — Goodreads and
// StoryGraph don't emit one, but a generic CSV authored against
// Librarium's own export could.
func Bool(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "yes", "y", "1", "favorite", "favourite":
		return true, true
	case "false", "no", "n", "0", "":
		// Empty string is "no" rather than "unspecified" because the
		// CSV column simply not having a marker in a row is the
		// universal way of saying "not a favourite".
		return false, true
	}
	return false, false
}

var titlePunctRe = regexp.MustCompile(`[[:punct:]]`)

// NormalizeTitle collapses a title to a comparison key: lowercased with
// all punctuation stripped. Used as a fallback duplicate check when
// ISBN-based dedup can't run — either the CSV row has no ISBN at all
// (common for older/small-press titles Goodreads doesn't carry ISBN data
// for), or the ISBN doesn't match any existing edition (e.g. the row
// represents a different edition/printing of a book already in the
// library). Without this, internal/workers/import_worker.go's ISBN-gated
// duplicate check never engages for those rows and silently creates a
// second Book record for the same work every time.
//
// The importer's repository-side lookup (BookRepo.FindByNormalizedTitleInLibrary)
// must normalize stored titles with the exact same rule (lower + strip
// [[:punct:]]) for the two sides to agree — see that method's SQL.
func NormalizeTitle(title string) string {
	return strings.ToLower(titlePunctRe.ReplaceAllString(title, ""))
}

