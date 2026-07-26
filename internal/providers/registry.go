// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package providers

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Registry holds all registered metadata providers.
type Registry struct {
	mu        sync.RWMutex
	providers []MetadataProvider
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds a provider to the registry.
func (r *Registry) Register(p MetadataProvider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers = append(r.providers, p)
}

// All returns all registered providers (enabled or not).
func (r *Registry) All() []MetadataProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]MetadataProvider, len(r.providers))
	copy(out, r.providers)
	return out
}

// Configure updates a single provider's config by name.
func (r *Registry) Configure(name string, cfg map[string]string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.providers {
		if p.Info().Name == name {
			p.Configure(cfg)
			return
		}
	}
}

// BookISBNProviders returns all enabled providers with the book_isbn capability.
func (r *Registry) BookISBNProviders() []BookISBNProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []BookISBNProvider
	for _, p := range r.providers {
		if !p.Enabled() {
			continue
		}
		if bp, ok := p.(BookISBNProvider); ok {
			out = append(out, bp)
		}
	}
	return out
}

// SeriesSearchProviders returns all enabled providers with the series_name capability.
func (r *Registry) SeriesSearchProviders() []SeriesSearchProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []SeriesSearchProvider
	for _, p := range r.providers {
		if !p.Enabled() {
			continue
		}
		if sp, ok := p.(SeriesSearchProvider); ok {
			out = append(out, sp)
		}
	}
	return out
}

// LookupISBN queries all enabled BookISBNProviders concurrently and returns
// all non-nil results. Errors from individual providers are silently skipped.
func (r *Registry) LookupISBN(ctx context.Context, isbn string) []*BookResult {
	providers := r.BookISBNProviders()
	if len(providers) == 0 {
		return nil
	}

	type result struct {
		book *BookResult
	}

	ch := make(chan result, len(providers))
	for _, p := range providers {
		go func(bp BookISBNProvider) {
			book, err := bp.LookupByISBN(ctx, isbn)
			if err != nil || book == nil {
				ch <- result{}
				return
			}
			ch <- result{book: book}
		}(p)
	}

	var out []*BookResult
	for range providers {
		res := <-ch
		if res.book != nil {
			out = append(out, res.book)
		}
	}
	return out
}

// BookSearchProviders returns all enabled providers with the book_search capability.
func (r *Registry) BookSearchProviders() []BookSearchProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []BookSearchProvider
	for _, p := range r.providers {
		if !p.Enabled() {
			continue
		}
		if bp, ok := p.(BookSearchProvider); ok {
			out = append(out, bp)
		}
	}
	return out
}

// searchDeadline is how long SearchBooks waits for lagging providers once at
// least one provider has already returned results.  A slow provider (e.g. Open
// Library search timing out at 15 s) will not hold up results from fast ones.
// var, not const, so tests can shrink it rather than sleeping several real
// seconds per case.
var searchDeadline = 5 * time.Second

// SearchBooks queries all enabled BookSearchProviders concurrently and returns
// as soon as every provider has responded OR the deadline is reached.
func (r *Registry) SearchBooks(ctx context.Context, query string) []*BookResult {
	providers := r.BookSearchProviders()
	slog.InfoContext(ctx, "book search start", "query", query, "providers", len(providers))
	if len(providers) == 0 {
		return nil
	}

	type result struct {
		name  string
		items []*BookResult
	}

	ch := make(chan result, len(providers))
	for _, p := range providers {
		go func(bp BookSearchProvider) {
			name := bp.Info().Name
			items, err := bp.SearchBooks(ctx, query)
			if err != nil {
				slog.WarnContext(ctx, "book search provider error", "provider", name, "error", err)
				ch <- result{name: name}
				return
			}
			slog.InfoContext(ctx, "book search provider ok", "provider", name, "results", len(items))
			ch <- result{name: name, items: items}
		}(p)
	}

	// deadlineC stays nil (blocks forever in the select below) until the
	// first result arrives, matching searchDeadline's doc comment: lagging
	// providers get searchDeadline *after* a fast one has already
	// responded, not a flat searchDeadline from the start of the search.
	// Before any result exists there's no fast provider to protect, so we
	// wait unboundedly for the first one — each provider's own HTTP client
	// timeout is still the real worst-case bound, since every goroutine
	// below sends to ch when its call returns, success or error.
	var deadlineC <-chan time.Time

	var out []*BookResult
	remaining := len(providers)
	for remaining > 0 {
		select {
		case res := <-ch:
			out = append(out, res.items...)
			remaining--
			if deadlineC == nil {
				deadline := time.NewTimer(searchDeadline)
				defer deadline.Stop()
				deadlineC = deadline.C
			}
		case <-deadlineC:
			slog.InfoContext(ctx, "book search deadline reached, returning partial results",
				"query", query, "waiting_on", remaining, "results_so_far", len(out))
			slog.InfoContext(ctx, "book search done", "query", query, "total", len(out))
			return out
		case <-ctx.Done():
			return out
		}
	}

	slog.InfoContext(ctx, "book search done", "query", query, "total", len(out))
	return out
}

// ContributorProviders returns all enabled providers that implement ContributorProvider.
func (r *Registry) ContributorProviders() []ContributorProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []ContributorProvider
	for _, p := range r.providers {
		if !p.Enabled() {
			continue
		}
		if cp, ok := p.(ContributorProvider); ok {
			out = append(out, cp)
		}
	}
	return out
}

// SeriesVolumesProvider returns the first enabled provider with the given source name that implements SeriesVolumesProvider.
func (r *Registry) SeriesVolumesProvider(source string) SeriesVolumesProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.providers {
		if !p.Enabled() {
			continue
		}
		if p.Info().Name != source {
			continue
		}
		if svp, ok := p.(SeriesVolumesProvider); ok {
			return svp
		}
	}
	return nil
}

// SearchSeries queries all enabled SeriesSearchProviders concurrently.
func (r *Registry) SearchSeries(ctx context.Context, query string) []SeriesResult {
	providers := r.SeriesSearchProviders()
	if len(providers) == 0 {
		return nil
	}

	type result struct {
		items []SeriesResult
	}

	ch := make(chan result, len(providers))
	for _, p := range providers {
		go func(sp SeriesSearchProvider) {
			items, err := sp.SearchSeries(ctx, query)
			if err != nil {
				ch <- result{}
				return
			}
			ch <- result{items: items}
		}(p)
	}

	var out []SeriesResult
	for range providers {
		res := <-ch
		out = append(out, res.items...)
	}
	return out
}
