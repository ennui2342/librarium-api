// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package service

import (
	"testing"

	"github.com/fireball1725/librarium-api/internal/providers"
)

func intPtr(n int) *int { return &n }

// Real Neuromancer editions pulled directly from the ISFDB mirror
// (2026-08-03) — the case that motivated this feature. pub_id 186689 is
// the specific edition a real search for "Neuromancer" failed to surface
// (buried behind 16 older editions once the adapter's per-title cap was
// hit). Format values are already mapped through the same vocabulary
// mapISFDBBinding produces (paperback/hardcover), since these tests work
// at the providers.BookResult level.
var (
	neuromancer1993PB = &providers.BookResult{ // pub_id 186689 — the target edition
		Title: "Neuromancer", Authors: []string{"William Gibson"},
		PublishDate: "1993", Format: "paperback", ISBN10: "0586066454",
	}
	neuromancer1984PBa = &providers.BookResult{ // pub_id 23643
		Title: "Neuromancer", Authors: []string{"William Gibson"},
		PublishDate: "1984-07", Format: "paperback", ISBN10: "0441569560",
	}
	neuromancer1984PBb = &providers.BookResult{ // pub_id 23645
		Title: "Neuromancer", Authors: []string{"William Gibson"},
		PublishDate: "1984-08", Format: "paperback", ISBN10: "0441569579",
	}
	neuromancer1984HC = &providers.BookResult{ // pub_id 23644
		Title: "Neuromancer", Authors: []string{"William Gibson"},
		PublishDate: "1984-10", Format: "hardcover", ISBN10: "057503470X",
	}
	neuromancer2003TP = &providers.BookResult{ // pub_id 672839 (tp -> paperback)
		Title: "Neuromancer", Authors: []string{"William Gibson"},
		PublishDate: "2003", Format: "paperback", ISBN10: "8585887907",
	}
	neuromancer2001HC = &providers.BookResult{ // pub_id 1121056
		Title: "Neuromancer", Authors: []string{"William Gibson"},
		PublishDate: "2001", Format: "hardcover", ISBN10: "9536337541",
	}
	unrelatedBook = &providers.BookResult{
		Title: "The Languages of Pao", Authors: []string{"Jack Vance"},
		PublishDate: "1958", Format: "paperback",
	}
)

func TestScoreCandidate_ISBNShortCircuit(t *testing.T) {
	t.Parallel()

	known := BookFields{
		Title: "Something Else Entirely", Authors: []string{"Nobody Real"},
		ISBN13: "9780316466400",
	}
	// Deliberately mismatched on every fuzzy field but ISBN — a real ISBN
	// match should still short-circuit to a certain match.
	candidate := &providers.BookResult{
		Title: "A Totally Different Title", Authors: []string{"Someone Else"},
		ISBN13: "9780316466400", Format: "hardcover",
	}
	got := ScoreCandidate(known, candidate)
	if got.Score != 1 || got.Bucket != MatchLikely {
		t.Fatalf("ISBN match should short-circuit to (1, likely), got (%v, %v)", got.Score, got.Bucket)
	}
}

// The real motivating case: given what a user already knows about their
// physical copy (title, author, year, format — no ISBN, since that's what
// they're trying to find), the specific 1993 paperback edition should rank
// strictly first among the real candidate pool, and land in the "likely"
// bucket. Other genuine editions of the same book should still score
// reasonably (they ARE plausible matches — ranking, not rejection, is the
// point), but strictly below the exact year+format match. An unrelated book
// that only coincidentally shares an author-search result should score far
// lower than any real Neuromancer edition.
func TestScoreCandidate_RealNeuromancerEditions(t *testing.T) {
	t.Parallel()

	known := BookFields{
		Title:       "Neuromancer",
		Authors:     []string{"William Gibson"},
		PublishYear: intPtr(1993),
		Format:      "paperback",
	}

	candidates := []*providers.BookResult{
		neuromancer1993PB, neuromancer1984PBa, neuromancer1984PBb,
		neuromancer1984HC, neuromancer2003TP, neuromancer2001HC, unrelatedBook,
	}
	scores := make(map[*providers.BookResult]ScoredResult, len(candidates))
	for _, c := range candidates {
		scores[c] = ScoreCandidate(known, c)
	}

	target := scores[neuromancer1993PB]
	if target.Score != 1 {
		t.Errorf("exact title+author+year+format match should score 1, got %v", target.Score)
	}
	if target.Bucket != MatchLikely {
		t.Errorf("exact match should bucket as likely, got %v", target.Bucket)
	}

	for _, other := range []*providers.BookResult{
		neuromancer1984PBa, neuromancer1984PBb, neuromancer1984HC,
		neuromancer2003TP, neuromancer2001HC,
	} {
		if scores[other].Score >= target.Score {
			t.Errorf("candidate %q (%s, %s) scored %v, want strictly less than target's %v",
				other.Title, other.PublishDate, other.Format, scores[other].Score, target.Score)
		}
	}

	// Same-format, closer-year editions should outrank same-title
	// candidates with a mismatched format or a larger year gap.
	if scores[neuromancer1984PBa].Score <= scores[neuromancer1984HC].Score {
		t.Errorf("1984 paperback (%v) should outrank 1984 hardcover (%v) given known format=paperback",
			scores[neuromancer1984PBa].Score, scores[neuromancer1984HC].Score)
	}

	if scores[unrelatedBook].Score >= scores[neuromancer1984HC].Score {
		t.Errorf("unrelated book (%v) should score below even the worst real Neuromancer candidate (%v)",
			scores[unrelatedBook].Score, scores[neuromancer1984HC].Score)
	}
	if scores[unrelatedBook].Bucket == MatchLikely {
		t.Errorf("unrelated book should not bucket as likely, got score %v", scores[unrelatedBook].Score)
	}
}

// A candidate missing format/year/language/publisher entirely shouldn't be
// penalized for the gaps — score should be computed from whatever fields
// are actually present on both sides (title + author here), not diluted by
// treating the missing fields as zero.
func TestScoreCandidate_MissingFieldsAreNeutral(t *testing.T) {
	t.Parallel()

	known := BookFields{
		Title:       "Neuromancer",
		Authors:     []string{"William Gibson"},
		PublishYear: intPtr(1993),
		Format:      "paperback",
		Publisher:   "Ace Books",
		Language:    "en",
	}
	sparse := &providers.BookResult{
		Title: "Neuromancer", Authors: []string{"William Gibson"},
		// No PublishDate, Format, Publisher, or Language at all.
	}
	got := ScoreCandidate(known, sparse)
	if got.Score != 1 {
		t.Errorf("title+author exact match with everything else absent should still score 1 (neutral on gaps), got %v", got.Score)
	}
}

// Regression test: models.NormalizeEditionFormat defaults an empty/unknown
// input to "paperback", which would silently turn "we don't know the
// candidate's format" into a false match against any paperback known-format.
// formatScore must treat blank as absent *before* normalizing.
func TestScoreCandidate_BlankFormatIsNotPaperback(t *testing.T) {
	t.Parallel()

	known := BookFields{Title: "Neuromancer", Authors: []string{"William Gibson"}, Format: "paperback"}
	candidate := &providers.BookResult{Title: "Neuromancer", Authors: []string{"William Gibson"}, Format: ""}

	withBlankFormat := ScoreCandidate(known, candidate)

	candidate.Format = "hardcover" // a real, explicit mismatch
	withRealMismatch := ScoreCandidate(known, candidate)

	if withBlankFormat.Score != 1 {
		t.Errorf("blank candidate format should be neutral (score still 1 from title+author alone), got %v", withBlankFormat.Score)
	}
	// A real mismatch should score lower than treating the field as absent —
	// if these were equal, formatScore would be conflating "unknown" with
	// "known and different", which is exactly the bug this guards against.
	if withRealMismatch.Score >= withBlankFormat.Score {
		t.Errorf("explicit format mismatch (%v) should score lower than blank/absent format (%v)",
			withRealMismatch.Score, withBlankFormat.Score)
	}
}

func TestBucketFor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		score float64
		want  MatchBucket
	}{
		{1.0, MatchLikely},
		{bucketLikelyThreshold, MatchLikely},
		{bucketLikelyThreshold - 0.01, MatchPossible},
		{bucketPossibleThreshold, MatchPossible},
		{bucketPossibleThreshold - 0.01, MatchOther},
		{0, MatchOther},
	}
	for _, c := range cases {
		if got := bucketFor(c.score); got != c.want {
			t.Errorf("bucketFor(%v) = %v, want %v", c.score, got, c.want)
		}
	}
}
