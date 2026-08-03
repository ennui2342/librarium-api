// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package service

import (
	"strconv"
	"strings"

	"github.com/fireball1725/librarium-api/internal/models"
	"github.com/fireball1725/librarium-api/internal/providers"
)

// BookFields is the subset of a book's already-known metadata used to score
// provider search candidates against it — the "Best Matches" flow's whole
// premise is that this data already exists and is trustworthy, so search
// results should be ranked by similarity to it instead of presented as an
// undifferentiated list for a human to eyeball.
type BookFields struct {
	Title       string
	Authors     []string
	Publisher   string
	PublishYear *int
	Language    string
	Format      string // paperback | hardcover | ebook | audiobook | digital, or "" if unknown
	ISBN10      string
	ISBN13      string
}

// MatchBucket is a coarse confidence label, not a raw score — a percentage-
// looking number implies more rigor than a heuristic like this actually
// has. See docs/design note in ScoreCandidate for the threshold reasoning.
type MatchBucket string

const (
	MatchLikely   MatchBucket = "likely"
	MatchPossible MatchBucket = "possible"
	MatchOther    MatchBucket = "other"
)

// ScoredResult pairs a provider candidate with its computed match score.
type ScoredResult struct {
	Result *providers.BookResult `json:"result"`
	Score  float64               `json:"score"`
	Bucket MatchBucket           `json:"bucket"`
}

// Field weights, used when all fields are present. Relative order (title >
// author > format ≈ year > language ≈ publisher) reflects how much each
// field can be trusted, per real usage: title/author are structurally
// reliable once normalized; format and year meaningfully distinguish
// between a book's many editions once title/author narrow down the work;
// language and publisher are lower-trust — publisher naming is inconsistent
// across sources (imprint vs. parent company), and a blank/wrong language
// field is common enough that it shouldn't swing a result much.
//
// Deliberately excludes page count entirely — too contentious across
// sources (front matter, ARC vs. final counts) to be worth even a tiebreak.
const (
	weightTitle     = 0.35
	weightAuthor    = 0.25
	weightFormat    = 0.15
	weightYear      = 0.15
	weightLanguage  = 0.05
	weightPublisher = 0.05
)

// Bucket thresholds on the normalized [0,1] score. First pass, not tuned
// against real usage yet — expect to revisit once this has run against a
// few dozen real "which edition is this" searches.
const (
	bucketLikelyThreshold   = 0.72
	bucketPossibleThreshold = 0.40
)

// yearDecayWindow controls how fast the year-closeness score falls off:
// score reaches 0 once the candidate is this many years away from the
// known year. 15 years is generous on purpose — editions get reissued
// decades apart, and year is a ranking signal among otherwise-plausible
// candidates, not a hard cutoff.
const yearDecayWindow = 15.0

// ScoreCandidate compares a candidate provider result against a book's
// already-known fields and returns a normalized [0,1] score plus a bucket.
//
// Missing data is neutral, never penalized: a field is only scored when
// both sides have a value, and the final score is a weighted average over
// just the fields that were actually present — not a weighted sum against
// a fixed total, which would otherwise silently punish a candidate for a
// gap in a source's data (ISFDB's own data has plenty of these) as if it
// were a genuine mismatch.
//
// One short-circuit: an exact ISBN match (either ISBN-10 or ISBN-13, on
// either side) is a certain match — return immediately at the top score/bucket
// rather than let weighted fields dilute a case that isn't actually ambiguous.
// This is the minority case for this flow, though — you're usually here
// specifically because you don't have the ISBN yet.
func ScoreCandidate(known BookFields, candidate *providers.BookResult) ScoredResult {
	if isbnMatch(known, candidate) {
		return ScoredResult{Result: candidate, Score: 1, Bucket: MatchLikely}
	}

	type weighted struct {
		weight float64
		score  float64
	}
	var present []weighted

	if s, ok := titleScore(known.Title, candidate.Title); ok {
		present = append(present, weighted{weightTitle, s})
	}
	if s, ok := authorScore(known.Authors, candidate.Authors); ok {
		present = append(present, weighted{weightAuthor, s})
	}
	if s, ok := formatScore(known.Format, candidate.Format); ok {
		present = append(present, weighted{weightFormat, s})
	}
	if s, ok := yearScore(known.PublishYear, parseYear(candidate.PublishDate)); ok {
		present = append(present, weighted{weightYear, s})
	}
	if s, ok := stringExactScore(known.Language, candidate.Language); ok {
		present = append(present, weighted{weightLanguage, s})
	}
	if s, ok := publisherScore(known.Publisher, candidate.Publisher); ok {
		present = append(present, weighted{weightPublisher, s})
	}

	var weightSum, scoreSum float64
	for _, f := range present {
		weightSum += f.weight
		scoreSum += f.weight * f.score
	}

	var total float64
	if weightSum > 0 {
		total = scoreSum / weightSum
	}
	return ScoredResult{Result: candidate, Score: total, Bucket: bucketFor(total)}
}

func bucketFor(score float64) MatchBucket {
	switch {
	case score >= bucketLikelyThreshold:
		return MatchLikely
	case score >= bucketPossibleThreshold:
		return MatchPossible
	default:
		return MatchOther
	}
}

func isbnMatch(known BookFields, candidate *providers.BookResult) bool {
	if known.ISBN13 != "" && candidate.ISBN13 != "" && known.ISBN13 == candidate.ISBN13 {
		return true
	}
	if known.ISBN10 != "" && candidate.ISBN10 != "" && known.ISBN10 == candidate.ISBN10 {
		return true
	}
	return false
}

// titleScore is a graded version of fuzzyTitleMatch (ai_suggestions_parser.go):
// Jaccard similarity over the same normalized token set, instead of a
// boolean equal-or-substring check. Reuses normalizeTitle so "Saga Volume 1"
// vs "Saga, Vol. 1" and series-numbering suffixes behave identically to the
// existing hallucination-check code path.
func titleScore(a, b string) (float64, bool) {
	na, nb := normalizeTitle(a), normalizeTitle(b)
	if na == "" || nb == "" {
		return 0, false
	}
	return jaccard(strings.Fields(na), strings.Fields(nb)), true
}

// authorScore is a graded version of fuzzyAuthorMatch: Jaccard similarity
// over the union of every author's name tokens on each side (via the same
// authorTokens helper), rather than requiring every token on the shorter
// side to appear on the longer one. Handles multi-author books reasonably —
// a partial-author match (one of two co-authors) scores as partial overlap
// instead of failing outright.
func authorScore(a, b []string) (float64, bool) {
	ta := flattenAuthorTokens(a)
	tb := flattenAuthorTokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0, false
	}
	return jaccard(ta, tb), true
}

func flattenAuthorTokens(names []string) []string {
	var out []string
	for _, n := range names {
		out = append(out, authorTokens(n)...)
	}
	return out
}

// jaccard returns |intersection| / |union| over two token lists, treated as sets.
func jaccard(a, b []string) float64 {
	setA := make(map[string]struct{}, len(a))
	for _, t := range a {
		setA[t] = struct{}{}
	}
	setB := make(map[string]struct{}, len(b))
	for _, t := range b {
		setB[t] = struct{}{}
	}
	if len(setA) == 0 || len(setB) == 0 {
		return 0
	}
	inter := 0
	for t := range setA {
		if _, ok := setB[t]; ok {
			inter++
		}
	}
	union := len(setA) + len(setB) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// formatScore compares two edition formats after normalizing both through
// models.NormalizeEditionFormat. The empty-string check has to happen
// *before* normalizing — NormalizeEditionFormat defaults an unrecognized
// or empty input to "paperback" (a UI/import convenience elsewhere in the
// codebase), which would otherwise silently treat "we don't know the
// format" as "it's a paperback" and wrongly score it as a match against
// any real paperback candidate.
func formatScore(a, b string) (float64, bool) {
	if strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" {
		return 0, false
	}
	if models.NormalizeEditionFormat(a) == models.NormalizeEditionFormat(b) {
		return 1, true
	}
	return 0, true
}

// yearScore decays linearly from 1 (exact match) to 0 at yearDecayWindow
// years apart, rather than requiring exact equality — a year is useful for
// ranking editions relative to each other, not as a strict filter, since
// source data on publish year is often off by one and editions genuinely
// do get reissued decades apart.
func yearScore(known, candidate *int) (float64, bool) {
	if known == nil || candidate == nil {
		return 0, false
	}
	diff := *known - *candidate
	if diff < 0 {
		diff = -diff
	}
	score := 1 - float64(diff)/yearDecayWindow
	if score < 0 {
		score = 0
	}
	return score, true
}

// parseYear extracts a *int year from a BookResult's freeform PublishDate
// string, reusing the same leading-4-digit extraction (and "0000" rejection)
// that rankAndDeduplicateBooks already relies on for merge-key purposes.
func parseYear(publishDate string) *int {
	y := publishYear(publishDate)
	if y == "" {
		return nil
	}
	n, err := strconv.Atoi(y)
	if err != nil {
		return nil
	}
	return &n
}

// stringExactScore is a simple case-insensitive exact-match score, used for
// language: 1 on an exact match, 0 on a genuine mismatch, "not present" when
// either side is blank. Deliberately not fuzzy — language codes/names don't
// benefit from partial-overlap scoring the way titles/authors do, and this
// stays a low-weight signal specifically because blank language data is
// common enough that it shouldn't swing a result much either way.
func stringExactScore(a, b string) (float64, bool) {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return 0, false
	}
	if a == b {
		return 1, true
	}
	return 0, true
}

// publisherScore is intentionally the least sophisticated of the fuzzy
// comparisons — publisher naming is the least consistent field across
// sources (imprint vs. parent company, "Tor" vs "Tor Books" vs "Tom
// Doherty Associates"), so this is low-weight by design and only needs to
// catch the easy cases: exact match after normalization, or one name
// containing the other.
func publisherScore(a, b string) (float64, bool) {
	na, nb := normalizeBookToken(a), normalizeBookToken(b)
	if na == "" || nb == "" {
		return 0, false
	}
	if na == nb || strings.Contains(na, nb) || strings.Contains(nb, na) {
		return 1, true
	}
	return 0, true
}
