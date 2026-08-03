// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package providers

import (
	"context"
	"testing"
	"time"
)

// fakeSearchProvider is a BookSearchProvider whose SearchBooks call blocks
// for `delay` before returning a single result named after the provider.
type fakeSearchProvider struct {
	name  string
	delay time.Duration
}

func (p *fakeSearchProvider) Info() ProviderInfo {
	return ProviderInfo{Name: p.name, DisplayName: p.name, Capabilities: []string{CapBookSearch}}
}
func (p *fakeSearchProvider) Configure(map[string]string) {}
func (p *fakeSearchProvider) Enabled() bool               { return true }
func (p *fakeSearchProvider) SearchBooks(ctx context.Context, query string) ([]*BookResult, error) {
	select {
	case <-time.After(p.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return []*BookResult{{Provider: p.name, Title: p.name}}, nil
}

// withSearchDeadline temporarily shrinks the package-level deadline so tests
// don't have to sleep several real seconds per case, restoring it after.
func withSearchDeadline(t *testing.T, d time.Duration) {
	t.Helper()
	orig := searchDeadline
	searchDeadline = d
	t.Cleanup(func() { searchDeadline = orig })
}

// Regression test for the bug found live 2026-07-26: the deadline timer was
// started unconditionally at the top of SearchBooks, so if the *fastest*
// provider itself took longer than searchDeadline, the deadline could fire
// before any result at all arrived — the opposite of the doc comment's
// intent ("waits for lagging providers once at least one provider has
// already returned results").
func TestSearchBooks_DeadlineStartsAfterFirstResult_NotAtSearchStart(t *testing.T) {
	withSearchDeadline(t, 50*time.Millisecond)

	r := NewRegistry()
	// Both providers are individually slower than searchDeadline on their
	// own — under the old flat-from-start timer, the deadline would fire
	// at 50ms, before either had a chance to respond, yielding zero results.
	r.Register(&fakeSearchProvider{name: "slow-first", delay: 60 * time.Millisecond})
	r.Register(&fakeSearchProvider{name: "slow-second", delay: 100 * time.Millisecond})

	start := time.Now()
	out := r.SearchBooks(context.Background(), "query")
	elapsed := time.Since(start)

	if len(out) != 2 {
		t.Fatalf("got %d results, want 2 (both providers, since the deadline should only start counting after the first response) — results: %+v", len(out), out)
	}
	// Sanity bound: should finish well before the two providers' delays
	// summed, proving they ran concurrently rather than serially.
	if elapsed > 200*time.Millisecond {
		t.Fatalf("took %s, expected well under 200ms for two concurrent providers", elapsed)
	}
}

func TestSearchBooks_DropsStragglerPastGracePeriodAfterFirstResult(t *testing.T) {
	withSearchDeadline(t, 50*time.Millisecond)

	r := NewRegistry()
	r.Register(&fakeSearchProvider{name: "fast", delay: 0})
	// Arrives 150ms after the fast provider — well past fast's 0+50ms grace
	// window — so it must be dropped even though the fix removed the flat
	// from-search-start timer.
	r.Register(&fakeSearchProvider{name: "straggler", delay: 150 * time.Millisecond})

	out := r.SearchBooks(context.Background(), "query")

	if len(out) != 1 || out[0].Provider != "fast" {
		t.Fatalf("got %+v, want exactly the fast provider's result — the straggler should have been dropped", out)
	}
}

// Regression test for the bug found live 2026-08-03: a real ISFDB query
// answered in ~1s in isolation but was still getting cut off by the
// default 5s searchDeadline when racing five other providers concurrently
// as part of the "Best Matches" flow — the exact case that flow depends on
// ISFDB for. SearchBooksWithDeadline must honor a longer, explicitly-passed
// deadline without touching the package-level default used by plain
// SearchBooks (the "By Title" tab), which should stay snappy.
func TestSearchBooksWithDeadline_HonorsExplicitDeadlineIndependentlyOfDefault(t *testing.T) {
	// Package default stays small — if SearchBooksWithDeadline secretly
	// fell back to it instead of the explicit argument, this straggler
	// would still get dropped and the test would fail.
	withSearchDeadline(t, 50*time.Millisecond)

	r := NewRegistry()
	r.Register(&fakeSearchProvider{name: "fast", delay: 0})
	// Would be dropped under the 50ms default (same shape as the
	// straggler test above), but 300ms is comfortably inside an explicit
	// 500ms deadline.
	r.Register(&fakeSearchProvider{name: "isfdb-like-straggler", delay: 300 * time.Millisecond})

	out := r.SearchBooksWithDeadline(context.Background(), "query", 500*time.Millisecond)

	if len(out) != 2 {
		t.Fatalf("got %d results, want 2 — the straggler should survive under the explicit 500ms deadline: %+v", len(out), out)
	}

	// Meanwhile plain SearchBooks (no explicit deadline) must still use the
	// small package default and drop the same straggler — proving the two
	// entry points are genuinely independent, not just that a long enough
	// explicit deadline papers over a shared bug.
	r2 := NewRegistry()
	r2.Register(&fakeSearchProvider{name: "fast", delay: 0})
	r2.Register(&fakeSearchProvider{name: "isfdb-like-straggler", delay: 300 * time.Millisecond})
	defaultOut := r2.SearchBooks(context.Background(), "query")
	if len(defaultOut) != 1 || defaultOut[0].Provider != "fast" {
		t.Fatalf("plain SearchBooks got %+v, want only the fast provider under the small package default", defaultOut)
	}
}

// fakeDeepSearchProvider implements DeepBookSearchProvider — SearchBooks and
// SearchBooksDeep return distinguishably different results so tests can
// prove which one actually got called.
type fakeDeepSearchProvider struct {
	name string
}

func (p *fakeDeepSearchProvider) Info() ProviderInfo {
	return ProviderInfo{Name: p.name, DisplayName: p.name, Capabilities: []string{CapBookSearch}}
}
func (p *fakeDeepSearchProvider) Configure(map[string]string) {}
func (p *fakeDeepSearchProvider) Enabled() bool               { return true }
func (p *fakeDeepSearchProvider) SearchBooks(ctx context.Context, query string) ([]*BookResult, error) {
	return []*BookResult{{Provider: p.name, Title: "shallow"}}, nil
}
func (p *fakeDeepSearchProvider) SearchBooksDeep(ctx context.Context, query string) ([]*BookResult, error) {
	return []*BookResult{{Provider: p.name, Title: "deep"}}, nil
}

// Regression test for the live finding 2026-08-03: giving Best Matches a
// longer deadline wasn't enough on its own — the plain "By Title" search
// and Best Matches both went through the same ISFDB SearchBooks call, so
// making that call fetch enough depth for Best Matches (Dracula: 348
// editions) also made the quick search path fetch the same huge batch on
// its much tighter deadline. SearchBooksDeepWithDeadline must call
// SearchBooksDeep on a capable provider — proving the two paths are
// genuinely separate, not just differently timed.
func TestSearchBooksDeepWithDeadline_CallsDeepVariantOnCapableProviders(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeDeepSearchProvider{name: "isfdb-like"})
	r.Register(&fakeSearchProvider{name: "shallow-only", delay: 0}) // no SearchBooksDeep at all

	deep := r.SearchBooksDeepWithDeadline(context.Background(), "query", time.Second)
	var gotDeep, gotShallowFallback bool
	for _, r := range deep {
		if r.Provider == "isfdb-like" && r.Title == "deep" {
			gotDeep = true
		}
		if r.Provider == "shallow-only" {
			gotShallowFallback = true
		}
	}
	if !gotDeep {
		t.Errorf("expected the DeepBookSearchProvider-capable provider's SearchBooksDeep result, got %+v", deep)
	}
	if !gotShallowFallback {
		t.Errorf("expected the non-capable provider to still contribute via its plain SearchBooks, got %+v", deep)
	}

	// The plain (non-deep) entry point must never call SearchBooksDeep, even
	// on a provider that implements it — that's the whole point of keeping
	// the two paths separate.
	r2 := NewRegistry()
	r2.Register(&fakeDeepSearchProvider{name: "isfdb-like"})
	shallow := r2.SearchBooksWithDeadline(context.Background(), "query", time.Second)
	if len(shallow) != 1 || shallow[0].Title != "shallow" {
		t.Errorf("plain SearchBooksWithDeadline must call SearchBooks, not SearchBooksDeep, even on a capable provider — got %+v", shallow)
	}
}
