// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package imports

import (
	"testing"
	"time"
)

func TestReadStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		// ── Read / completed ──────────────────────────────────────
		{"goodreads/storygraph: read", "read", "read"},
		{"libib: Completed", "Completed", "read"},
		{"capitalised + extra whitespace: Finished", "  FINISHED  ", "read"},

		// ── Currently reading ─────────────────────────────────────
		{"goodreads: currently-reading", "currently-reading", "reading"},
		{"storygraph: currently-reading", "currently-reading", "reading"},
		{"libib: In Progress", "In Progress", "reading"},
		{"libib: in progress (lowercase)", "in progress", "reading"},

		// ── Did not finish ────────────────────────────────────────
		{"storygraph: did-not-finish", "did-not-finish", "did_not_finish"},
		{"libib: Abandoned", "Abandoned", "did_not_finish"},
		{"shorthand: dnf", "DNF", "did_not_finish"},

		// ── Unread / to-read / on hold ────────────────────────────
		{"goodreads: to-read", "to-read", "unread"},
		{"libib: Not Begun", "Not Begun", "unread"},
		{"libib: On Hold", "On Hold", "unread"},

		// ── Empty / unrecognised → "" ─────────────────────────────
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
		{"unknown value", "in flight", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ReadStatus(c.in)
			if got != c.want {
				t.Errorf("ReadStatus(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestRating(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		in        string
		wantValue int
		wantOK    bool
	}{
		// ── Libib (0–5 with halves) — the actual values from the
		// user's Libib export ──────────────────────────────────────
		{"libib 5.0", "5.0", 10, true},
		{"libib 4.5", "4.5", 9, true},
		{"libib 3.0", "3.0", 6, true},
		{"libib 1.5", "1.5", 3, true},
		{"libib 0.5", "0.5", 1, true},

		// ── Goodreads (1–5 whole stars) ───────────────────────────
		{"goodreads 5", "5", 10, true},
		{"goodreads 1", "1", 2, true},

		// ── StoryGraph (1–5 quarter precision) — round half-up ───
		{"storygraph 3.25 → rounds to 3.5 → 7", "3.25", 7, true},
		{"storygraph 4.75 → rounds to 5 → 10", "4.75", 10, true},

		// ── Empty / zero / out-of-range → no rating ──────────────
		{"empty", "", 0, false},
		{"zero (unrated)", "0", 0, false},
		{"zero with decimal", "0.0", 0, false},
		{"out of range high", "5.5", 0, false},
		{"out of range negative", "-1", 0, false},
		{"non-numeric", "not-a-number", 0, false},

		// ── Format quirks ─────────────────────────────────────────
		{"with whitespace", "  4.0  ", 8, true},
		{"with stars suffix", "3.0 stars", 6, true},
		{"with /5 suffix", "4/5", 8, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := Rating(c.in)
			if ok != c.wantOK || got != c.wantValue {
				t.Errorf("Rating(%q) = (%d, %v), want (%d, %v)",
					c.in, got, ok, c.wantValue, c.wantOK)
			}
		})
	}
}

func TestDate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		in     string
		wantOK bool
		// We don't pin the exact time.Time on success — just check
		// the parsed Y/M/D matches expectations to keep timezone
		// noise out.
		wantY, wantM, wantD int
	}{
		// ── Libib (ISO 8601, full date) — actual format from the
		// user's export. ──────────────────────────────────────────
		{"libib 2022-04-11", "2022-04-11", true, 2022, 4, 11},
		{"libib 2005-11-01 (publish_date sample)", "2005-11-01", true, 2005, 11, 1},

		// ── Goodreads (slashes) ───────────────────────────────────
		{"goodreads 2024/03/15", "2024/03/15", true, 2024, 3, 15},

		// ── StoryGraph (also ISO) ─────────────────────────────────
		{"storygraph 2024-03-15", "2024-03-15", true, 2024, 3, 15},

		// ── Month-only / year-only fallbacks ──────────────────────
		{"month-only 2017-09", "2017-09", true, 2017, 9, 1},
		{"year-only 2005", "2005", true, 2005, 1, 1},

		// ── ISO datetime (some exports include time) ──────────────
		{"datetime", "2024-03-15 10:30:00", true, 2024, 3, 15},

		// ── Long-form English ─────────────────────────────────────
		{"long form", "March 15, 2024", true, 2024, 3, 15},

		// ── Unparseable / empty ───────────────────────────────────
		{"empty", "", false, 0, 0, 0},
		{"garbage", "not a date", false, 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := Date(c.in)
			if ok != c.wantOK {
				t.Fatalf("Date(%q) ok = %v, want %v", c.in, ok, c.wantOK)
			}
			if !ok {
				return
			}
			if got.Year() != c.wantY || int(got.Month()) != c.wantM || got.Day() != c.wantD {
				t.Errorf("Date(%q) = %v, want %d-%02d-%02d",
					c.in, got.Format(time.RFC3339), c.wantY, c.wantM, c.wantD)
			}
		})
	}
}

func TestBool(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"true", "TRUE", "yes", "Yes", "y", "1", "favorite", "favourite"} {
		got, ok := Bool(in)
		if !ok || !got {
			t.Errorf("Bool(%q) = (%v, %v), want (true, true)", in, got, ok)
		}
	}
	for _, in := range []string{"false", "no", "n", "0", ""} {
		got, ok := Bool(in)
		if !ok || got {
			t.Errorf("Bool(%q) = (%v, %v), want (false, true)", in, got, ok)
		}
	}
	// Unrecognised values are reported as "couldn't parse" so callers
	// can decide whether to default-true or default-false themselves.
	got, ok := Bool("maybe")
	if ok || got {
		t.Errorf("Bool(\"maybe\") = (%v, %v), want (false, false)", got, ok)
	}
}

// Test cases below are drawn from a real Goodreads-to-Librarium import run
// (2026-08-03) where 13 books were silently duplicated: 10 had no ISBN on
// either side (import_worker.go's duplicate check is ISBN-gated and never
// engaged), and 2-3 had a different ISBN because the row represented a
// different edition/printing of a book already in the library. See
// BookRepo.FindByNormalizedTitleInLibrary for the repository-side lookup
// this function feeds.
func TestNormalizeTitle(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain title", "Vacuum Flowers", "vacuum flowers"},
		{"leading number", "36 Streets", "36 streets"},
		{"apostrophe", "I'm Waiting for You and Other Stories", "im waiting for you and other stories"},
		{
			"slash, parens, and a trailing date",
			"Eye for Eye/the Tunesmith (Tor Science Fiction Double) by Orson Scott Card (1990-10-06)",
			"eye for eyethe tunesmith tor science fiction double by orson scott card 19901006",
		},
		{"series suffix with hash and parens", "Children of Memory (Children of Time, #3)", "children of memory children of time 3"},
		{"mid-word case difference collapses", "THe Last THeorem", "the last theorem"},
		{"different case, same title, must match", "The Last Theorem", "the last theorem"},
		{"trailing period abbreviation", "Vol. 8", "vol 8"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NormalizeTitle(c.in)
			if got != c.want {
				t.Errorf("NormalizeTitle(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}

	// The whole point: two independently-supplied titles for the same
	// work must normalize to the same key regardless of case/punctuation
	// differences between the CSV row and what's already stored.
	if NormalizeTitle("THe Last THeorem") != NormalizeTitle("The Last Theorem") {
		t.Error("NormalizeTitle should be case-insensitive: \"THe Last THeorem\" and \"The Last Theorem\" must match")
	}
	if NormalizeTitle("Vacuum Flowers") != NormalizeTitle("vacuum flowers") {
		t.Error("NormalizeTitle should be case-insensitive")
	}
}
