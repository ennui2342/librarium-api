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
