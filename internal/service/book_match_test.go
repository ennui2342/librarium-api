// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package service

import (
	"math"
	"testing"

	"github.com/fireball1725/librarium-api/internal/providers"
)

func intPtr(n int) *int { return &n }

func approxEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

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
		Publisher: "HarperCollins (UK)", Language: "eng",
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
	// Not a clean 1.0: `known` here doesn't set Language/Publisher, so those
	// two fields count as neutral (0.5) rather than a full match — title
	// (0.35) + author (0.25) + format (0.15) + year (0.15) all perfect,
	// plus 0.5 * (language's 0.05 + publisher's 0.05) = 0.95 total.
	if !approxEqual(target.Score, 0.95) {
		t.Errorf("exact title+author+year+format match (language/publisher unset on known) should score 0.95, got %v", target.Score)
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

// Regression test for the live scoring shortfall found 2026-08-03: the
// real book record's publisher/language ("HarperCollins Publishers", "en")
// don't literally match ISFDB's for the same real edition ("HarperCollins
// (UK)", "eng") — different naming convention and a different ISO 639
// code length respectively. Before fixing publisherScore/languageScore,
// this scored 0.900 (title+author+format+year matched, publisher+language
// both scored as hard mismatches) while a *different*, completely blank-
// metadata candidate scored a perfect 1.000 by having no conflicting
// signal at all — the wrong edition would have ranked above the right one.
func TestScoreCandidate_RealPublisherAndLanguageVariants(t *testing.T) {
	t.Parallel()

	// The actual stored fields for this book, queried live.
	known := BookFields{
		Title:       "Neuromancer",
		Authors:     []string{"William Gibson"},
		PublishYear: intPtr(1993),
		Format:      "paperback",
		Publisher:   "HarperCollins Publishers",
		Language:    "en",
	}

	got := ScoreCandidate(known, neuromancer1993PB) // Publisher: "HarperCollins (UK)", Language: "eng"
	// Not a clean 1.0 either: publisher scores partial credit (Jaccard over
	// shared "harpercollins" token, not a full string match) — title(0.35)
	// + author(0.25) + format(0.15) + year(0.15) + language(0.05, fixed by
	// ISO 639 normalization) all perfect, publisher(0.05 * ~0.333) partial
	// = ~0.967. The two real regressions this test guards are: (a) this
	// must score meaningfully high (language/publisher shouldn't read as
	// mismatches), and (b) — the actual live bug — it must beat a
	// blank-metadata candidate, checked below.
	if got.Score < 0.9 {
		t.Errorf("HarperCollins (UK)/eng should score highly against HarperCollins Publishers/en (normalization, not a mismatch), got %v", got.Score)
	}
	if got.Bucket != MatchLikely {
		t.Errorf("expected likely bucket, got %v (score %v)", got.Bucket, got.Score)
	}

	// A blank-metadata candidate with the same title+author must NOT
	// outscore the real edition just because it has nothing to conflict
	// with — that was the actual live failure mode.
	blank := &providers.BookResult{Title: "Neuromancer", Authors: []string{"William Gibson"}}
	blankScore := ScoreCandidate(known, blank)
	if blankScore.Score > got.Score {
		t.Errorf("blank-metadata candidate (%v) outscored the real matching edition (%v) — a title+author-only match should never beat a fuller genuine match", blankScore.Score, got.Score)
	}
}

func TestLanguageScore_ISO639CodeLengthVariants(t *testing.T) {
	t.Parallel()
	cases := []struct{ a, b string }{
		{"en", "eng"}, {"eng", "en"}, {"de", "ger"}, {"de", "deu"}, {"fr", "fre"},
	}
	for _, c := range cases {
		score, ok := languageScore(c.a, c.b)
		if !ok || score != 1 {
			t.Errorf("languageScore(%q, %q) = (%v, %v), want (1, true)", c.a, c.b, score, ok)
		}
	}
}

func TestPublisherScore_SharedTokenGetsPartialCredit(t *testing.T) {
	t.Parallel()
	score, ok := publisherScore("HarperCollins (UK)", "HarperCollins Publishers")
	if !ok {
		t.Fatal("expected both sides present")
	}
	if score <= 0 || score >= 1 {
		t.Errorf("expected partial credit for the shared \"harpercollins\" token, got %v (want strictly between 0 and 1)", score)
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
	// title(0.35) + author(0.25) both perfect and present = 0.60, plus the
	// 4 absent fields (format/year/language/publisher, weight 0.40
	// combined) each contributing their neutral 0.5 = +0.20 -> 0.80.
	if !approxEqual(got.Score, 0.8) {
		t.Errorf("title+author exact match with everything else absent should score 0.8 (neutral, not skipped), got %v", got.Score)
	}

	// The actual point of "neutral, not skipped": a candidate that's
	// present-but-wrong on those same 4 fields must score *lower* than one
	// that's simply silent on them — gaps in a source's data shouldn't
	// read as being just as bad as an active mismatch.
	wrong := &providers.BookResult{
		Title: "Neuromancer", Authors: []string{"William Gibson"},
		PublishDate: "1958", Format: "hardcover", Publisher: "Totally Different Press", Language: "fr",
	}
	gotWrong := ScoreCandidate(known, wrong)
	if gotWrong.Score >= got.Score {
		t.Errorf("actively mismatched fields (%v) should score lower than the same fields simply absent (%v)", gotWrong.Score, got.Score)
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

	// Same shape as TestScoreCandidate_MissingFieldsAreNeutral: title+author
	// present and perfect (0.60), the other 4 fields absent and neutral
	// (+0.20) = 0.80.
	if !approxEqual(withBlankFormat.Score, 0.8) {
		t.Errorf("blank candidate format should be neutral (0.8, not skipped/zeroed), got %v", withBlankFormat.Score)
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
