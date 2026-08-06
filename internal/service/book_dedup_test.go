// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package service

import (
	"testing"

	"github.com/fireball1725/librarium-api/internal/providers"
)

func TestPublishYear(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"1973", "1973"},
		{"1973-00-00", "1973"},
		{"1973-08-15", "1973"},
		{"0000-00-00", ""},
		{"May 1973", "1973"},
		{"73", ""},
		// Known limitation, not fixed here: publishYear takes the first four
		// consecutive digits it finds, so a leading number that isn't the
		// year (an ordinal like "20th" here) shadows the real one. Harmless
		// today — this only narrows a dedup key, and an empty year just
		// falls back to matching on "" — but documented so a future change
		// to this parser doesn't silently alter dedup behavior unnoticed.
		{"20th Century Fox, 1973", ""},
	}
	for _, tc := range cases {
		if got := publishYear(tc.in); got != tc.want {
			t.Errorf("publishYear(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRankAndDeduplicateBooks_SameISBNDifferentPrintings documents a
// deliberate tradeoff, not a bug fix: Panther/Granada reused ISBN 0586028463
// across three separate print runs of "Camp Concentration" (1969, 1973,
// 1977), and ISBN dedup alone can't tell those apart. An earlier version of
// this fix folded year into the ISBN key to split cases like this one, but
// that traded a rare correctness win for a common regression — providers
// routinely disagree about publication year for the very same ISBN (original
// publication vs. printing in hand), so exact-year matching on the ISBN key
// turned single real editions into false duplicates far more often than it
// ever caught a genuine reprint. ISBN alone remains the merge key; distinct
// printings sharing one ISBN merge into a single result, same as before this
// PR. See internal/service/providers.go's bookKeys comment.
func TestRankAndDeduplicateBooks_SameISBNDifferentPrintings(t *testing.T) {
	mk := func(publisher, year string) *providers.BookResult {
		return &providers.BookResult{
			Provider:    "isfdb",
			Title:       "Camp Concentration",
			Authors:     []string{"Thomas M. Disch"},
			Publisher:   publisher,
			PublishDate: year + "-00-00",
			ISBN10:      "0586028463",
		}
	}
	results := []*providers.BookResult{
		mk("Panther", "1969"),
		mk("Panther", "1973"),
		mk("Panther / Granada", "1977"),
	}
	out := rankAndDeduplicateBooks(results, nil)
	if len(out) != 1 {
		t.Fatalf("got %d results, want 1 merged result — ISBN alone is the merge key, so same-ISBN printings merge regardless of year", len(out))
	}
}

// TestRankAndDeduplicateBooks_SameEditionNoISBNStillMerges guards against
// the fix above being too permissive: two providers reporting the exact same
// edition (same title/author/publisher/year) with no ISBN from either side
// should still merge into one result, not multiply.
func TestRankAndDeduplicateBooks_SameEditionNoISBNStillMerges(t *testing.T) {
	results := []*providers.BookResult{
		{Provider: "open_library", Title: "Camp Concentration", Authors: []string{"Thomas M. Disch"}, Publisher: "Doubleday", PublishDate: "1969"},
		{Provider: "isfdb", Title: "Camp Concentration", Authors: []string{"Thomas M. Disch"}, Publisher: "Doubleday", PublishDate: "1969-01-16"},
	}
	out := rankAndDeduplicateBooks(results, []string{"open_library", "isfdb"})
	if len(out) != 1 {
		t.Fatalf("got %d results, want 1 merged result for the same reported edition", len(out))
	}
}

// TestRankAndDeduplicateBooks_DifferentEditionsNoISBNSurvive is the original
// bug: many editions of the same work with no ISBN (common in ISFDB's older
// data) must NOT collapse into a single slot just because they share a
// title+author fingerprint — that discarded every edition but one.
func TestRankAndDeduplicateBooks_DifferentEditionsNoISBNSurvive(t *testing.T) {
	results := []*providers.BookResult{
		{Provider: "isfdb", Title: "Camp Concentration", Authors: []string{"Thomas M. Disch"}, Publisher: "Doubleday", PublishDate: "1969-01-16"},
		{Provider: "isfdb", Title: "Camp Concentration", Authors: []string{"Thomas M. Disch"}, Publisher: "Avon", PublishDate: "1971-06-00"},
	}
	out := rankAndDeduplicateBooks(results, nil)
	if len(out) != 2 {
		t.Fatalf("got %d results, want 2 distinct no-ISBN editions to survive", len(out))
	}
}
