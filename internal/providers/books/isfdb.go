// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package books

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/fireball1725/librarium-api/internal/providers"
)

// ISFDBProvider looks up books via a self-hosted mirror of the Internet
// Speculative Fiction Database (ISFDB). Unlike the other book providers,
// ISFDB has no public API — the site is Cloudflare-protected and its own
// weekly backups are only distributed as a MySQL dump on Google Drive — so
// this provider talks to a small adapter service that fronts your own
// mirror of that data, configured via base_url rather than an API key.
//
// ISFDB's real strength is depth: older, small-press, and non-US editions
// that Open Library / Google Books / Hardcover often don't carry at all,
// plus unusually strong series and contributor (pseudonym-aware) data.
type ISFDBProvider struct {
	base
	client  *http.Client
	baseURL string
}

func NewISFDBProvider() *ISFDBProvider {
	return &ISFDBProvider{
		base:   base{enabled: false}, // disabled until a base_url is configured
		client: &http.Client{Timeout: 20 * time.Second},
	}
}

func (p *ISFDBProvider) Info() providers.ProviderInfo {
	return providers.ProviderInfo{
		Name:        "isfdb",
		DisplayName: "ISFDB",
		Description: "Internet Speculative Fiction Database. Deep bibliographic data for SFF — especially older, small-press, and non-US editions other providers miss. Requires a self-hosted mirror (ISFDB has no public API).",
		RequiresKey: false,
		Capabilities: []string{
			providers.CapBookISBN,
			providers.CapBookSearch,
			providers.CapSeriesName,
			providers.CapSeriesVolumes,
			providers.CapContributor,
		},
		HelpText: "ISFDB doesn't offer a public API. Point this at the base URL of your own ISFDB mirror adapter (MariaDB import of ISFDB's weekly backup, fronted by a small JSON API) — leave disabled if you don't run one. Reference implementation: github.com/ennui2342/isfdb-adapter.",
		HelpURL:  "https://github.com/ennui2342/isfdb-adapter",
		ConfigFields: []providers.ConfigField{
			{
				Key:         "base_url",
				Label:       "Mirror base URL",
				Type:        "url",
				Required:    true,
				Placeholder: "http://isfdb-adapter:8080",
				HelpText:    "Base URL of your self-hosted ISFDB mirror adapter.",
			},
		},
	}
}

func (p *ISFDBProvider) Configure(cfg map[string]string) {
	p.baseURL = strings.TrimRight(cfg["base_url"], "/")
	if v, ok := cfg["enabled"]; ok {
		p.enabled = v != "false" && p.baseURL != ""
	} else {
		p.enabled = p.baseURL != ""
	}
}

// ─── wire types (adapter JSON shapes) ──────────────────────────────────────────

type isfdbBook struct {
	Title       string   `json:"title"`
	Subtitle    string   `json:"subtitle"`
	Authors     []string `json:"authors"`
	Publisher   string   `json:"publisher"`
	PublishDate string   `json:"publish_date"`
	ISBN10      string   `json:"isbn_10"`
	ISBN13      string   `json:"isbn_13"`
	Description string   `json:"description"`
	CoverURL    string   `json:"cover_url"`
	Language    string   `json:"language"`
	PageCount   *int     `json:"page_count"`
	Categories  []string `json:"categories"`
	// Binding is ISFDB's raw pub_ptype ("hc", "tp", "pb", "ebook", "audio CD",
	// etc.) — mapped to providers.BookResult's canonical Format via
	// mapISFDBBinding rather than passed through as-is.
	Binding string `json:"binding"`
}

// mapISFDBBinding maps ISFDB's pub_ptype vocabulary onto
// providers.BookResult's canonical format values (paperback | hardcover |
// ebook | audiobook | digital). ISFDB's vocabulary is print-binding-shape
// focused (digest/pulp/bedsheet/octavo/quarto/A4/A5/tabloid are all
// physical trim sizes, not separate "kinds" of book) — anything print but
// not explicitly hardcover buckets into "paperback" as the closest
// available canonical value. "unknown"/"other"/anything unrecognized maps
// to "" (absent), not a default guess — see formatScore's comment on why
// guessing here would be actively harmful to matching.
func mapISFDBBinding(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "hc":
		return "hardcover"
	case "tp", "pb", "digest", "pulp", "bedsheet", "octavo", "quarto",
		"a4", "a5", "tabloid", "ph", "webzine", "dos":
		return "paperback"
	case "ebook":
		return "ebook"
	case "audio cassette", "audio cd", "audio lp", "audio mp3 cd", "audio mp3 dvd",
		"audio mp3 usb drive", "digital audio download", "digital audio player":
		return "audiobook"
	default:
		return ""
	}
}

type isfdbSeries struct {
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	TotalCount       *int     `json:"total_count"`
	IsComplete       bool     `json:"is_complete"`
	CoverURL         string   `json:"cover_url"`
	ExternalID       string   `json:"external_id"`
	Status           string   `json:"status"`
	OriginalLanguage string   `json:"original_language"`
	PublicationYear  *int     `json:"publication_year"`
	Demographic      string   `json:"demographic"`
	Genres           []string `json:"genres"`
	URL              string   `json:"url"`
}

type isfdbVolume struct {
	Position    float64 `json:"position"`
	Title       string  `json:"title"`
	ReleaseDate string  `json:"release_date"`
	CoverURL    string  `json:"cover_url"`
	ExternalID  string  `json:"external_id"`
}

type isfdbAuthorSearchResult struct {
	ExternalID string `json:"external_id"`
	Name       string `json:"name"`
	Bio        string `json:"bio"`
	PhotoURL   string `json:"photo_url"`
}

type isfdbAuthorWork struct {
	Title       string `json:"title"`
	ISBN13      string `json:"isbn_13"`
	ISBN10      string `json:"isbn_10"`
	PublishYear *int   `json:"publish_year"`
	CoverURL    string `json:"cover_url"`
}

type isfdbAuthor struct {
	ExternalID string            `json:"external_id"`
	Name       string            `json:"name"`
	Bio        string            `json:"bio"`
	BornDate   string            `json:"born_date"`
	DiedDate   string            `json:"died_date"`
	PhotoURL   string            `json:"photo_url"`
	Works      []isfdbAuthorWork `json:"works"`
}

// ─── HTTP helper ────────────────────────────────────────────────────────────────

// errNotFound is returned by fetchJSON on a 404; callers translate it to a
// nil result (not found is not an error, per the other providers' convention).
type errNotFound struct{}

func (errNotFound) Error() string { return "not found" }

func (p *ISFDBProvider) fetchJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return errNotFound{}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("isfdb mirror: status %d for %s", resp.StatusCode, path)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func parseDate(s string) *time.Time {
	if s == "" {
		return nil
	}
	for _, layout := range []string{"2006-01-02", "2006-01", "2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	return nil
}

// normalizeDateString reduces s to VolumeResult.ReleaseDate's documented
// "YYYY-MM-DD" or "" contract. The adapter deliberately returns
// month/year-only precision when that's all ISFDB has (e.g. "1989-02"),
// which parseDate already accepts (Go's "2006-01"/"2006" layouts imply
// day 1) — pass it back through so a bare year or year-month, valid input
// by the adapter's own contract, doesn't reach a downstream `::date` SQL
// cast as something that isn't actually a date literal.
func normalizeDateString(s string) string {
	t := parseDate(s)
	if t == nil {
		return ""
	}
	return t.Format("2006-01-02")
}

// ─── BookISBNProvider ───────────────────────────────────────────────────────────

func (p *ISFDBProvider) LookupByISBN(ctx context.Context, isbn string) (*providers.BookResult, error) {
	var book isfdbBook
	if err := p.fetchJSON(ctx, "/isbn/"+url.PathEscape(isbn), &book); err != nil {
		if _, ok := err.(errNotFound); ok {
			return nil, nil
		}
		return nil, err
	}
	return book.toBookResult(), nil
}

func (b *isfdbBook) toBookResult() *providers.BookResult {
	return &providers.BookResult{
		Provider:        "isfdb",
		ProviderDisplay: "ISFDB",
		Title:           b.Title,
		Subtitle:        b.Subtitle,
		Authors:         b.Authors,
		Publisher:       b.Publisher,
		PublishDate:     b.PublishDate,
		ISBN10:          b.ISBN10,
		ISBN13:          b.ISBN13,
		Description:     b.Description,
		CoverURL:        b.CoverURL,
		Language:        b.Language,
		PageCount:       b.PageCount,
		Categories:      b.Categories,
		Format:          mapISFDBBinding(b.Binding),
	}
}

// ─── BookSearchProvider ─────────────────────────────────────────────────────────

// searchEditionsPerTitle/searchLimit override the adapter's own defaults
// (10 editions per matched title, 20 results total). The adapter orders
// editions oldest-first before Go-side ranking ever sees them — any fixed
// cap smaller than a title's real edition count silently excludes whatever
// fell past it, regardless of how good the ranking algorithm is. This was
// tuned to 25 once (matching Neuromancer's 66 editions, target ~17th
// oldest) but that was curve-fitting to one example: ISFDB's own data has
// far more prolific titles — Dracula alone has 348 editions, several
// classics exceed 150 — so a small fixed cap just relocates the same bug
// to a longer tail. The correctness-safe design is to filter *after*
// ranking, not before: fetch generously here (verified live 2026-08-03,
// Dracula's full ~400-candidate response took 5.73s single-provider,
// comfortably inside BestMatches' deadline — see providers.go), let
// ScoreCandidate rank the complete set, and truncate for display only
// after that ranking has happened (bestMatchesResultLimit in providers.go).
const (
	searchEditionsPerTitle = 400
	searchLimit            = 500
)

func (p *ISFDBProvider) SearchBooks(ctx context.Context, query string) ([]*providers.BookResult, error) {
	var books []isfdbBook
	path := fmt.Sprintf("/search?q=%s&editions_per_title=%d&limit=%d",
		url.QueryEscape(query), searchEditionsPerTitle, searchLimit)
	if err := p.fetchJSON(ctx, path, &books); err != nil {
		if _, ok := err.(errNotFound); ok {
			return nil, nil
		}
		return nil, err
	}
	out := make([]*providers.BookResult, 0, len(books))
	for i := range books {
		out = append(out, books[i].toBookResult())
	}
	return out, nil
}

// ─── SeriesSearchProvider ───────────────────────────────────────────────────────

func (p *ISFDBProvider) SearchSeries(ctx context.Context, query string) ([]providers.SeriesResult, error) {
	var series []isfdbSeries
	if err := p.fetchJSON(ctx, "/series/search?q="+url.QueryEscape(query), &series); err != nil {
		if _, ok := err.(errNotFound); ok {
			return nil, nil
		}
		return nil, err
	}
	out := make([]providers.SeriesResult, 0, len(series))
	for _, s := range series {
		out = append(out, providers.SeriesResult{
			Provider:         "isfdb",
			ProviderDisplay:  "ISFDB",
			Name:             s.Name,
			Description:      s.Description,
			TotalCount:       s.TotalCount,
			IsComplete:       s.IsComplete,
			CoverURL:         s.CoverURL,
			ExternalID:       s.ExternalID,
			ExternalSource:   "isfdb",
			Status:           s.Status,
			OriginalLanguage: s.OriginalLanguage,
			PublicationYear:  s.PublicationYear,
			Demographic:      s.Demographic,
			Genres:           s.Genres,
			URL:              s.URL,
		})
	}
	return out, nil
}

// ─── SeriesVolumesProvider ──────────────────────────────────────────────────────

func (p *ISFDBProvider) FetchSeriesVolumes(ctx context.Context, externalID string) ([]providers.VolumeResult, error) {
	var volumes []isfdbVolume
	if err := p.fetchJSON(ctx, "/series/"+url.PathEscape(externalID)+"/volumes", &volumes); err != nil {
		if _, ok := err.(errNotFound); ok {
			return nil, nil
		}
		return nil, err
	}
	out := make([]providers.VolumeResult, 0, len(volumes))
	for _, v := range volumes {
		out = append(out, providers.VolumeResult{
			Position:    v.Position,
			Title:       v.Title,
			ReleaseDate: normalizeDateString(v.ReleaseDate),
			CoverURL:    v.CoverURL,
			ExternalID:  v.ExternalID,
		})
	}
	return out, nil
}

// ─── ContributorProvider ────────────────────────────────────────────────────────

func (p *ISFDBProvider) SearchContributors(ctx context.Context, name string) ([]*providers.ContributorSearchResult, error) {
	var authors []isfdbAuthorSearchResult
	if err := p.fetchJSON(ctx, "/authors/search?q="+url.QueryEscape(name), &authors); err != nil {
		if _, ok := err.(errNotFound); ok {
			return nil, nil
		}
		return nil, err
	}
	out := make([]*providers.ContributorSearchResult, 0, len(authors))
	for _, a := range authors {
		out = append(out, &providers.ContributorSearchResult{
			ExternalID: a.ExternalID,
			Name:       a.Name,
			Bio:        a.Bio,
			PhotoURL:   a.PhotoURL,
		})
	}
	return out, nil
}

func (p *ISFDBProvider) FetchContributor(ctx context.Context, externalID string) (*providers.ContributorData, error) {
	var author isfdbAuthor
	if err := p.fetchJSON(ctx, "/authors/"+url.PathEscape(externalID), &author); err != nil {
		if _, ok := err.(errNotFound); ok {
			return nil, nil
		}
		return nil, err
	}

	works := make([]providers.ContributorWorkResult, 0, len(author.Works))
	for _, w := range author.Works {
		works = append(works, providers.ContributorWorkResult{
			Title:       w.Title,
			ISBN13:      w.ISBN13,
			ISBN10:      w.ISBN10,
			PublishYear: w.PublishYear,
			CoverURL:    w.CoverURL,
		})
	}

	return &providers.ContributorData{
		Provider:    "isfdb",
		ExternalID:  author.ExternalID,
		Name:        author.Name,
		Bio:         author.Bio,
		BornDate:    parseDate(author.BornDate),
		DiedDate:    parseDate(author.DiedDate),
		Nationality: "", // ISFDB doesn't track this directly
		PhotoURL:    author.PhotoURL,
		Works:       works,
	}, nil
}
