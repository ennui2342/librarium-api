// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

// Package providers defines the metadata provider plugin system.
// Providers expose optional capabilities (ISBN lookup, series search, etc.).
// Each provider is independently configurable via the instance settings table.
package providers

import (
	"context"
	"time"
)

// Capability names.
const (
	CapBookISBN      = "book_isbn"
	CapBookSearch    = "book_search"
	CapSeriesName    = "series_name"
	CapSeriesVolumes = "series_volumes"
	CapContributor   = "contributor"
)

// ConfigField describes a single config input the admin settings page should
// render for a provider. Mirrors internal/ai.ConfigField (kept as a separate
// type rather than a shared import so the two provider plugin systems stay
// independent) — needed for providers whose config is more than a single API
// key, e.g. a self-hosted mirror that only needs a base URL.
type ConfigField struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Type        string `json:"type"` // "password" | "text" | "url"
	Required    bool   `json:"required"`
	Placeholder string `json:"placeholder,omitempty"`
	HelpText    string `json:"help_text,omitempty"`
}

// ProviderInfo describes a provider's static metadata.
type ProviderInfo struct {
	Name         string
	DisplayName  string
	Description  string
	RequiresKey  bool
	Capabilities []string
	// HelpText is shown on the settings page to explain where to get the API key.
	HelpText string
	// HelpURL links to the page where users can obtain the API key.
	HelpURL string
	// ConfigFields declares the config inputs the settings page should render.
	// Optional — a provider that only needs the legacy single API key can
	// leave this nil and rely on RequiresKey; the settings page falls back
	// to the existing single-API-key form in that case.
	ConfigFields []ConfigField
}

// BookResult is a normalised book record returned by a BookISBNProvider.
type BookResult struct {
	Provider        string   `json:"provider"`
	ProviderDisplay string   `json:"provider_display"`
	Title           string   `json:"title"`
	Subtitle        string   `json:"subtitle"`
	Authors         []string `json:"authors"`
	Publisher       string   `json:"publisher"`
	PublishDate     string   `json:"publish_date"`
	ISBN10          string   `json:"isbn_10"`
	ISBN13          string   `json:"isbn_13"`
	Description     string   `json:"description"`
	CoverURL        string   `json:"cover_url"`
	Language        string   `json:"language"`
	PageCount       *int     `json:"page_count"`
	// Categories contains subject/genre tags from the provider (e.g. "Comics & Graphic Novels / Manga").
	// Used by the client to auto-detect the media type.
	Categories []string `json:"categories"`
	// Format is one of models.EditionFormat's canonical values (paperback |
	// hardcover | ebook | audiobook | digital), or "" when the provider
	// doesn't carry binding/format data. Only ISFDB populates this today —
	// Google Books/Hardcover/etc. don't expose per-edition binding info.
	Format string `json:"format"`
}

// SeriesResult is a normalised series record returned by a SeriesSearchProvider.
type SeriesResult struct {
	Provider         string   `json:"provider"`
	ProviderDisplay  string   `json:"provider_display"`
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	TotalCount       *int     `json:"total_count"`
	IsComplete       bool     `json:"is_complete"`
	CoverURL         string   `json:"cover_url"`
	ExternalID       string   `json:"external_id"`
	ExternalSource   string   `json:"external_source"`
	Status           string   `json:"status"`
	OriginalLanguage string   `json:"original_language"`
	PublicationYear  *int     `json:"publication_year"`
	Demographic      string   `json:"demographic"`
	Genres           []string `json:"genres"`
	URL              string   `json:"url"`
}

// MetadataProvider is the base interface all providers must implement.
type MetadataProvider interface {
	Info() ProviderInfo
	// Configure sets provider-specific config (api_key, etc.).
	// An empty map means "use defaults / no key required".
	Configure(cfg map[string]string)
	// Enabled reports whether the provider is active.
	Enabled() bool
}

// BookISBNProvider can look up a book by ISBN-10 or ISBN-13.
type BookISBNProvider interface {
	MetadataProvider
	LookupByISBN(ctx context.Context, isbn string) (*BookResult, error)
}

// BookSearchProvider can search for books by freetext query.
//
// Implementations must bound SearchBooks themselves, via an http.Client
// Timeout or equivalent. Registry.SearchBooks only starts its deadline once
// some provider has already responded, so before the first result the only
// things that can end the wait are a provider returning and the request
// context being cancelled. A provider that can hang indefinitely holds up
// every search that includes it.
type BookSearchProvider interface {
	MetadataProvider
	SearchBooks(ctx context.Context, query string) ([]*BookResult, error)
}

// DeepBookSearchProvider is an optional extension of BookSearchProvider for
// providers whose results are structured as one title with many editions
// rather than a flat list of distinct works — currently only ISFDB, where a
// classic can have hundreds of editions (Dracula: 348) and the specific one
// a caller wants can be arbitrarily far down that list. Plain SearchBooks
// callers (the quick "By Title" search) get that provider's normal,
// modest-depth results and a snappy response; a caller that specifically
// needs to find one known printing among many (Best Matches, ranking
// against a book's own already-known metadata) calls SearchBooksDeep
// instead and accepts a slower, more complete response in exchange. A
// provider with no meaningful notion of "many editions of one title" (Google
// Books, Hardcover, etc.) simply doesn't implement this — the registry
// falls back to plain SearchBooks for it either way, see
// Registry.SearchBooksDeepWithDeadline.
type DeepBookSearchProvider interface {
	BookSearchProvider
	SearchBooksDeep(ctx context.Context, query string) ([]*BookResult, error)
}

// SeriesSearchProvider can search for series by name.
type SeriesSearchProvider interface {
	MetadataProvider
	SearchSeries(ctx context.Context, query string) ([]SeriesResult, error)
}

// VolumeResult is a single volume record returned by a SeriesVolumesProvider.
type VolumeResult struct {
	Position    float64
	Title       string // empty for manga (no distinct per-volume titles)
	ReleaseDate string // "YYYY-MM-DD" or ""
	CoverURL    string
	ExternalID  string
}

// SeriesVolumesProvider can fetch per-volume metadata for a series.
type SeriesVolumesProvider interface {
	MetadataProvider
	FetchSeriesVolumes(ctx context.Context, externalID string) ([]VolumeResult, error)
}

// ContributorSearchResult is one candidate returned by a contributor name search.
type ContributorSearchResult struct {
	ExternalID string
	Name       string
	Bio        string // may be truncated
	PhotoURL   string
}

// ContributorWorkResult is one entry in a contributor's bibliography.
type ContributorWorkResult struct {
	Title       string
	ISBN13      string
	ISBN10      string
	PublishYear *int
	CoverURL    string
}

// ContributorData is the full enrichment payload for one contributor from a provider.
type ContributorData struct {
	Provider    string
	ExternalID  string
	Name        string
	Bio         string
	BornDate    *time.Time
	DiedDate    *time.Time
	Nationality string
	PhotoURL    string
	Works       []ContributorWorkResult
}

// ContributorProvider can look up and fetch contributor (author/narrator) profiles.
type ContributorProvider interface {
	MetadataProvider
	// SearchContributors returns candidates matching name, for the user to pick from.
	SearchContributors(ctx context.Context, name string) ([]*ContributorSearchResult, error)
	// FetchContributor fetches full profile + bibliography for the given provider-specific ID.
	FetchContributor(ctx context.Context, externalID string) (*ContributorData, error)
}
