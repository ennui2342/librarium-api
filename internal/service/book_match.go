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
// Missing data is neutral, not penalized like a genuine mismatch — but
// "neutral" means exactly that: a field with no data on either side
// contributes a middle-of-the-road 0.5, not "skip it and don't count its
// weight at all." That distinction matters and was wrong in an earlier
// version of this function: normalizing only over the fields actually
// present let a candidate with *less* data systematically outscore, even
// tie, a candidate with *more* data that was a genuine, fuller match —
// found live 2026-08-03, where a completely blank-metadata "Neuromancer"
// candidate scored a perfect 1.0 (title+author only, nothing to drag the
// average down) while the actual correct 1993 edition — matching on
// title, author, format, *and* year — scored lower simply for having more
// fields available to (almost) match on. The fix: every field always
// counts its full weight in the denominator; an absent field contributes
// weight*0.5 to the numerator (better than a real mismatch's 0, worse
// than not counting against the total at all) rather than being excluded
// from both sides of the ratio.
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

	// neutralFieldScore is what an absent field contributes toward the
	// weighted sum — see the "missing data is neutral" doc above for why
	// this is 0.5 (a real, unresolved unknown) rather than 0 (a genuine
	// mismatch) or being excluded from the total entirely.
	const neutralFieldScore = 0.5

	type weighted struct {
		weight float64
		score  float64
	}
	fields := []weighted{
		{weightTitle, neutralFieldScore},
		{weightAuthor, neutralFieldScore},
		{weightFormat, neutralFieldScore},
		{weightYear, neutralFieldScore},
		{weightLanguage, neutralFieldScore},
		{weightPublisher, neutralFieldScore},
	}
	if s, ok := titleScore(known.Title, candidate.Title); ok {
		fields[0].score = s
	}
	if s, ok := authorScore(known.Authors, candidate.Authors); ok {
		fields[1].score = s
	}
	if s, ok := formatScore(known.Format, candidate.Format); ok {
		fields[2].score = s
	}
	if s, ok := yearScore(known.PublishYear, parseYear(candidate.PublishDate)); ok {
		fields[3].score = s
	}
	if s, ok := languageScore(known.Language, candidate.Language); ok {
		fields[4].score = s
	}
	if s, ok := publisherScore(known.Publisher, candidate.Publisher); ok {
		fields[5].score = s
	}

	var weightSum, scoreSum float64
	for _, f := range fields {
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

// iso639Alpha3 maps the common ISO 639-2 (3-letter) codes to their 639-1
// (2-letter) equivalent, for languages likely to actually show up in a
// personal library. Sources disagree on which standard they emit — found
// live 2026-08-03: ISFDB returns "eng" while Librarium's own edition
// records store "en", so an exact-string language comparison silently
// treated the *same* language as a mismatch. Not exhaustive (639 has ~7000
// entries via 639-3); falls back to plain string comparison for anything
// not listed here rather than failing closed.
var iso639Alpha3 = map[string]string{
	"eng": "en", "ger": "de", "deu": "de", "fre": "fr", "fra": "fr",
	"spa": "es", "ita": "it", "por": "pt", "dut": "nl", "nld": "nl",
	"rus": "ru", "jpn": "ja", "chi": "zh", "zho": "zh", "kor": "ko",
	"pol": "pl", "swe": "sv", "dan": "da", "nor": "no", "fin": "fi",
}

func normalizeLanguage(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if two, ok := iso639Alpha3[s]; ok {
		return two
	}
	return s
}

// languageScore is a case-insensitive match on language, normalized across
// the common ISO 639-1/639-2 code variants (see iso639Alpha3) since
// providers don't agree on which one they emit. Deliberately not fuzzy
// beyond that normalization — language codes don't benefit from partial-
// overlap scoring the way titles/authors do, and this stays a low-weight
// signal specifically because blank language data is common enough that it
// shouldn't swing a result much either way.
func languageScore(a, b string) (float64, bool) {
	na, nb := normalizeLanguage(a), normalizeLanguage(b)
	if na == "" || nb == "" {
		return 0, false
	}
	if na == nb {
		return 1, true
	}
	return 0, true
}

// publisherScore uses the same token-Jaccard approach as title/author
// rather than substring containment — found live 2026-08-03: ISFDB's
// "HarperCollins (UK)" vs. a book record's "HarperCollins Publishers"
// share no substring relationship in either direction (neither contains
// the other whole), even though they're clearly the same publisher.
// Jaccard over word tokens gives partial credit for the shared
// "harpercollins" token instead of scoring a real match as a hard zero.
// Still the least sophisticated of the fuzzy comparisons on purpose —
// publisher naming is the least consistent field across sources (imprint
// vs. parent company, regional qualifiers), hence the low weight.
func publisherScore(a, b string) (float64, bool) {
	ta, tb := publisherTokens(a), publisherTokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0, false
	}
	return jaccard(ta, tb), true
}

func publisherTokens(s string) []string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	return strings.Fields(b.String())
}
