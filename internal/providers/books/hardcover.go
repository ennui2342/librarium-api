// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package books

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/fireball1725/librarium-api/internal/providers"
)

// HardcoverProvider looks up books via the Hardcover GraphQL API.
// Requires a personal API key from hardcover.app/account/api (free account).
// Rate limit: 60 requests/minute.
type HardcoverProvider struct {
	base
	apiKey string
	client *http.Client
}

func NewHardcoverProvider() *HardcoverProvider {
	return &HardcoverProvider{
		base:   base{enabled: false},
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

func (p *HardcoverProvider) Info() providers.ProviderInfo {
	return providers.ProviderInfo{
		Name:         "hardcover",
		DisplayName:  "Hardcover",
		Description:  "Book metadata from Hardcover.app. Free account required. 60 requests/minute.",
		RequiresKey:  true,
		Capabilities: []string{providers.CapBookISBN, providers.CapBookSearch, providers.CapSeriesName, providers.CapSeriesVolumes, providers.CapContributor},
		HelpText:     "Create a free account at hardcover.app, then visit hardcover.app/account/api to generate your API key.",
		HelpURL:      "https://hardcover.app/account/api",
	}
}

func (p *HardcoverProvider) Configure(cfg map[string]string) {
	p.apiKey = cfg["api_key"]
	if v, ok := cfg["enabled"]; ok {
		p.enabled = v == "true" && p.apiKey != ""
	} else {
		p.enabled = p.apiKey != ""
	}
}

const hardcoverEndpoint = "https://api.hardcover.app/v1/graphql"

// hardcoverISBNQuery queries editions by ISBN-10 or ISBN-13.
// Joins to book for title, description, authors, genres (cached_tags), and language.
const hardcoverISBNQuery = `
query BookByISBN($isbn: String!) {
  editions(
    where: { _or: [{ isbn_13: { _eq: $isbn } }, { isbn_10: { _eq: $isbn } }] }
    limit: 1
  ) {
    isbn_13
    isbn_10
    pages
    release_date
    image {
      url
    }
    publisher {
      name
    }
    language {
      language
    }
    book {
      title
      description
      cached_tags
      contributions {
        author {
          name
        }
      }
    }
  }
}`

func (p *HardcoverProvider) LookupByISBN(ctx context.Context, isbn string) (*providers.BookResult, error) {
	payload, err := json.Marshal(hcGQLRequest{
		Query:     hardcoverISBNQuery,
		Variables: map[string]any{"isbn": isbn},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hardcoverEndpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("hardcover: invalid API key")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hardcover returned status %d", resp.StatusCode)
	}

	var body hcGQLResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if len(body.Errors) > 0 {
		return nil, fmt.Errorf("hardcover: %s", body.Errors[0].Message)
	}
	if len(body.Data.Editions) == 0 {
		return nil, nil // not found
	}

	ed := body.Data.Editions[0]
	result := &providers.BookResult{
		Provider:        "hardcover",
		ProviderDisplay: "Hardcover",
		ISBN13:          ed.ISBN13,
		ISBN10:          ed.ISBN10,
		PublishDate:     ed.ReleaseDate, // already "YYYY-MM-DD"
		PageCount:       ed.PageCount,
	}

	if ed.Publisher != nil {
		result.Publisher = ed.Publisher.Name
	}
	if ed.Image != nil {
		result.CoverURL = ed.Image.URL
	}
	if ed.Language != nil {
		result.Language = normalizeHardcoverLanguage(ed.Language.Language)
	}

	if ed.Book != nil {
		result.Title = ed.Book.Title
		result.Description = ed.Book.Description
		for _, c := range ed.Book.Contributions {
			if c.Author != nil && c.Author.Name != "" {
				result.Authors = append(result.Authors, c.Author.Name)
			}
		}
		result.Categories = extractHardcoverGenres(ed.Book.CachedTags)
	}

	return result, nil
}

// hardcoverSearchQuery searches books by freetext using the Hardcover search API.
// The results field is returned as opaque jsonb.
const hardcoverSearchQuery = `
query SearchBooks($query: String!) {
  search(query: $query, query_type: "Book", per_page: 15) {
    results
  }
}`

func (p *HardcoverProvider) SearchBooks(ctx context.Context, query string) ([]*providers.BookResult, error) {
	payload, err := json.Marshal(hcGQLRequest{
		Query:     hardcoverSearchQuery,
		Variables: map[string]any{"query": query},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hardcoverEndpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("hardcover: invalid API key")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hardcover search: status %d", resp.StatusCode)
	}

	var body hcSearchGQLResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if len(body.Errors) > 0 {
		return nil, fmt.Errorf("hardcover search: %s", body.Errors[0].Message)
	}

	// results is opaque jsonb from Typesense — shape is {"found":N,"hits":[{"document":{...}}]}
	docs, err := parseHardcoverSearchDocs(ctx, body.Data.Search.Results)
	if err != nil {
		return nil, nil
	}

	loggedMissingImage := false
	var out []*providers.BookResult
	for _, doc := range docs {
		if doc.Title == "" {
			continue
		}
		coverURL := doc.coverURL()
		if coverURL == "" && !loggedMissingImage {
			raw, _ := json.Marshal(doc)
			slog.DebugContext(ctx, "hardcover search: no image found in document", "doc", string(raw))
			loggedMissingImage = true
		}
		result := &providers.BookResult{
			Provider:        "hardcover",
			ProviderDisplay: "Hardcover",
			Title:           doc.Title,
			Authors:         doc.AuthorNames,
			CoverURL:        coverURL,
		}
		for _, isbn := range doc.ISBNs {
			switch len(isbn) {
			case 13:
				if result.ISBN13 == "" {
					result.ISBN13 = isbn
				}
			case 10:
				if result.ISBN10 == "" {
					result.ISBN10 = isbn
				}
			}
		}
		out = append(out, result)
	}
	return out, nil
}

// parseHardcoverSeriesDocs extracts series documents from Hardcover's opaque search results jsonb.
func parseHardcoverSeriesDocs(ctx context.Context, raw json.RawMessage) ([]hcSeriesSearchDoc, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	// Primary: Typesense wrapper
	var wrapper struct {
		Hits []struct {
			Document hcSeriesSearchDoc `json:"document"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(raw, &wrapper); err == nil && len(wrapper.Hits) > 0 {
		docs := make([]hcSeriesSearchDoc, 0, len(wrapper.Hits))
		for _, h := range wrapper.Hits {
			docs = append(docs, h.Document)
		}
		return docs, nil
	}
	// Fallback: bare array
	var docs []hcSeriesSearchDoc
	if err := json.Unmarshal(raw, &docs); err == nil {
		return docs, nil
	}
	slog.DebugContext(ctx, "hardcover series search: unrecognised results shape", "raw", string(raw))
	return nil, fmt.Errorf("unrecognised results shape")
}

// parseHardcoverSearchDocs extracts book documents from Hardcover's opaque search results jsonb.
// Hardcover uses Typesense internally; the blob is {"found":N,"hits":[{"document":{...}}]}.
// Falls back to treating the blob as a bare []hcSearchDoc in case the shape ever changes.
func parseHardcoverSearchDocs(ctx context.Context, raw json.RawMessage) ([]hcSearchDoc, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	// Primary: Typesense wrapper
	var wrapper struct {
		Hits []struct {
			Document hcSearchDoc `json:"document"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(raw, &wrapper); err == nil && len(wrapper.Hits) > 0 {
		docs := make([]hcSearchDoc, 0, len(wrapper.Hits))
		for _, h := range wrapper.Hits {
			docs = append(docs, h.Document)
		}
		return docs, nil
	}
	// Fallback: bare array
	var docs []hcSearchDoc
	if err := json.Unmarshal(raw, &docs); err == nil {
		return docs, nil
	}
	slog.DebugContext(ctx, "hardcover book search: unrecognised results shape", "raw", string(raw))
	return nil, fmt.Errorf("unrecognised results shape")
}

// hardcoverSeriesSearchQuery searches for series by name using the Hardcover search API.
const hardcoverSeriesSearchQuery = `
query SearchSeries($query: String!) {
  search(query: $query, query_type: "Series", per_page: 15) {
    results
  }
}`

func (p *HardcoverProvider) SearchSeries(ctx context.Context, query string) ([]providers.SeriesResult, error) {
	payload, err := json.Marshal(hcGQLRequest{
		Query:     hardcoverSeriesSearchQuery,
		Variables: map[string]any{"query": query},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hardcoverEndpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("hardcover: invalid API key")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hardcover series search: status %d", resp.StatusCode)
	}

	var body hcSearchGQLResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if len(body.Errors) > 0 {
		return nil, fmt.Errorf("hardcover series search: %s", body.Errors[0].Message)
	}

	docs, err := parseHardcoverSeriesDocs(ctx, body.Data.Search.Results)
	if err != nil {
		return nil, nil
	}

	var out []providers.SeriesResult
	slugs := make([]string, 0, len(docs))
	for _, doc := range docs {
		if doc.Name == "" {
			continue
		}
		result := providers.SeriesResult{
			Provider:        "hardcover",
			ProviderDisplay: "Hardcover",
			Name:            doc.Name,
			CoverURL:        doc.ImageURL,
			ExternalID:      doc.Slug,
			ExternalSource:  "hardcover",
			URL:             fmt.Sprintf("https://hardcover.app/series/%s", doc.Slug),
		}
		if doc.BooksCount != nil {
			result.TotalCount = doc.BooksCount
		}
		out = append(out, result)
		if doc.Slug != "" {
			slugs = append(slugs, doc.Slug)
		}
	}

	// Second pass: pull description, genres, publication year, and a cover
	// from the first book — the search index only carries the bare-minimum
	// fields, so without this step rows render as just "N vols via Hardcover".
	// Enrichment is best-effort; on failure we return the basic list.
	if len(slugs) > 0 {
		if enriched, err := p.enrichSeries(ctx, slugs); err == nil {
			for i := range out {
				e, ok := enriched[out[i].ExternalID]
				if !ok {
					continue
				}
				if e.description != "" {
					out[i].Description = e.description
				}
				if out[i].CoverURL == "" && e.coverURL != "" {
					out[i].CoverURL = e.coverURL
				}
				if e.publicationYear != nil && out[i].PublicationYear == nil {
					out[i].PublicationYear = e.publicationYear
				}
				if len(e.genres) > 0 && len(out[i].Genres) == 0 {
					out[i].Genres = e.genres
				}
			}
		} else {
			slog.DebugContext(ctx, "hardcover series enrichment failed", "error", err)
		}
	}
	return out, nil
}

// hcSeriesEnrichQuery batches slug → rich-fields lookups. It fans out to a
// single GraphQL call regardless of result count; the search response only
// carries name/slug/books_count so this is what populates the UI cards.
const hcSeriesEnrichQuery = `
query EnrichSeries($slugs: [String!]!) {
  series(where: {slug: {_in: $slugs}}) {
    slug
    description
    book_series(order_by: {position: asc}, limit: 1) {
      book {
        release_date
        image { url }
        cached_tags
      }
    }
  }
}`

type hcSeriesEnrichData struct {
	description     string
	coverURL        string
	publicationYear *int
	genres          []string
}

func (p *HardcoverProvider) enrichSeries(ctx context.Context, slugs []string) (map[string]hcSeriesEnrichData, error) {
	payload, err := json.Marshal(hcGQLRequest{
		Query:     hcSeriesEnrichQuery,
		Variables: map[string]any{"slugs": slugs},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hardcoverEndpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hardcover enrich: status %d", resp.StatusCode)
	}

	var body struct {
		Data struct {
			Series []struct {
				Slug        string `json:"slug"`
				Description string `json:"description"`
				BookSeries  []struct {
					Book *struct {
						ReleaseDate string   `json:"release_date"`
						Image       *hcImage `json:"image"`
						// cached_tags is a jsonb map of category → [{tag,...}]; we
						// only care about the Genre bucket.
						CachedTags struct {
							Genre []struct {
								Tag string `json:"tag"`
							} `json:"Genre"`
						} `json:"cached_tags"`
					} `json:"book"`
				} `json:"book_series"`
			} `json:"series"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if len(body.Errors) > 0 {
		return nil, fmt.Errorf("hardcover enrich: %s", body.Errors[0].Message)
	}

	out := make(map[string]hcSeriesEnrichData, len(body.Data.Series))
	for _, s := range body.Data.Series {
		data := hcSeriesEnrichData{description: s.Description}
		if len(s.BookSeries) > 0 && s.BookSeries[0].Book != nil {
			b := s.BookSeries[0].Book
			if b.Image != nil && b.Image.URL != "" {
				data.coverURL = b.Image.URL
			}
			if len(b.ReleaseDate) >= 4 {
				if y, err := strconv.Atoi(b.ReleaseDate[:4]); err == nil {
					data.publicationYear = &y
				}
			}
			for _, g := range b.CachedTags.Genre {
				if g.Tag != "" {
					data.genres = append(data.genres, g.Tag)
				}
				if len(data.genres) >= 5 {
					break
				}
			}
		}
		out[s.Slug] = data
	}
	return out, nil
}

// hardcoverSeriesVolumesQuery fetches per-book metadata for a series by its slug.
const hardcoverSeriesVolumesQuery = `
query SeriesVolumes($slug: String!) {
  series(where: {slug: {_eq: $slug}}, limit: 1) {
    id
    name
    books_count
    book_series(order_by: {position: asc}) {
      position
      book {
        title
        release_date
        image { url }
      }
    }
  }
}`

func (p *HardcoverProvider) FetchSeriesVolumes(ctx context.Context, externalID string) ([]providers.VolumeResult, error) {
	payload, err := json.Marshal(hcGQLRequest{
		Query:     hardcoverSeriesVolumesQuery,
		Variables: map[string]any{"slug": externalID},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hardcoverEndpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("hardcover: invalid API key")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hardcover volumes: status %d", resp.StatusCode)
	}

	var body hcSeriesVolumesGQLResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if len(body.Errors) > 0 {
		return nil, fmt.Errorf("hardcover volumes: %s", body.Errors[0].Message)
	}
	if len(body.Data.Series) == 0 {
		return nil, nil
	}

	series := body.Data.Series[0]
	var out []providers.VolumeResult
	for _, sb := range series.SeriesBooks {
		vr := providers.VolumeResult{
			Position: sb.Position,
		}
		if sb.Book != nil {
			vr.Title = sb.Book.Title
			vr.ReleaseDate = normalizeHardcoverVolumeDate(sb.Book.ReleaseDate)
			if sb.Book.Image != nil {
				vr.CoverURL = sb.Book.Image.URL
			}
		}
		out = append(out, vr)
	}
	return out, nil
}

// normalizeHardcoverVolumeDate reduces s to VolumeResult.ReleaseDate's
// documented "YYYY-MM-DD" or "" contract. Hardcover's release dates are
// typically already full dates (see the "already YYYY-MM-DD" assumption
// elsewhere in this file for the book-level PublishDate field), but that's
// an assumption about a crowdsourced, community-editable database, not a
// guarantee — and unlike PublishDate, this field's value flows into a
// series_volumes.release_date column via a raw `::date` SQL cast, where an
// imprecise or malformed date isn't just cosmetically wrong, it fails the
// whole insert. Validate rather than trust.
func normalizeHardcoverVolumeDate(s string) string {
	if s == "" {
		return ""
	}
	for _, layout := range []string{"2006-01-02", "2006-01", "2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return ""
}

// normalizeHardcoverLanguage converts Hardcover's full language names to ISO 639-1 codes.
// Hardcover returns e.g. "English" where every other provider returns "en".
func normalizeHardcoverLanguage(name string) string {
	codes := map[string]string{
		"Afrikaans":           "af",
		"Albanian":            "sq",
		"Arabic":              "ar",
		"Basque":              "eu",
		"Belarusian":          "be",
		"Bengali":             "bn",
		"Bulgarian":           "bg",
		"Catalan":             "ca",
		"Chinese":             "zh",
		"Croatian":            "hr",
		"Czech":               "cs",
		"Danish":              "da",
		"Dutch":               "nl",
		"English":             "en",
		"Estonian":            "et",
		"Finnish":             "fi",
		"French":              "fr",
		"Galician":            "gl",
		"German":              "de",
		"Greek":               "el",
		"Hebrew":              "he",
		"Hindi":               "hi",
		"Hungarian":           "hu",
		"Icelandic":           "is",
		"Indonesian":          "id",
		"Irish":               "ga",
		"Italian":             "it",
		"Japanese":            "ja",
		"Korean":              "ko",
		"Latvian":             "lv",
		"Lithuanian":          "lt",
		"Macedonian":          "mk",
		"Malay":               "ms",
		"Maltese":             "mt",
		"Norwegian":           "no",
		"Persian":             "fa",
		"Polish":              "pl",
		"Portuguese":          "pt",
		"Romanian":            "ro",
		"Russian":             "ru",
		"Serbian":             "sr",
		"Slovak":              "sk",
		"Slovenian":           "sl",
		"Spanish":             "es",
		"Swahili":             "sw",
		"Swedish":             "sv",
		"Thai":                "th",
		"Turkish":             "tr",
		"Ukrainian":           "uk",
		"Urdu":                "ur",
		"Vietnamese":          "vi",
		"Welsh":               "cy",
	}
	if code, ok := codes[name]; ok {
		return code
	}
	return name // pass through if unrecognised (may already be a code)
}

// extractHardcoverGenres pulls genre names from the cached_tags JSONB field.
// Hardcover stores tags as {"genres": [{"tag": {"tag": "Fantasy"}}, ...], ...}.
// Falls back to treating array elements as plain strings if the nested shape
// is absent (schema may change over time).
func extractHardcoverGenres(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(raw, &outer); err != nil {
		return nil
	}
	genresRaw, ok := outer["genres"]
	if !ok {
		return nil
	}

	// Try [{tag: {tag: "Name"}}, ...]
	var nested []struct {
		Tag struct {
			Tag string `json:"tag"`
		} `json:"tag"`
	}
	if err := json.Unmarshal(genresRaw, &nested); err == nil && len(nested) > 0 {
		var out []string
		for _, item := range nested {
			if item.Tag.Tag != "" {
				out = append(out, item.Tag.Tag)
			}
		}
		if len(out) > 0 {
			return out
		}
	}

	// Fallback: plain string array ["Fantasy", "Fiction"]
	var plain []string
	if err := json.Unmarshal(genresRaw, &plain); err == nil {
		return plain
	}

	return nil
}

// ─── Hardcover GraphQL types ──────────────────────────────────────────────────

type hcGQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

type hcGQLResponse struct {
	Data struct {
		Editions []hcEdition `json:"editions"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type hcEdition struct {
	ISBN13      string          `json:"isbn_13"`
	ISBN10      string          `json:"isbn_10"`
	PageCount   *int            `json:"pages"`
	ReleaseDate string          `json:"release_date"`
	Image       *hcImage        `json:"image"`
	Publisher   *hcNameField    `json:"publisher"`
	Language    *hcLanguage     `json:"language"`
	Book        *hcBook         `json:"book"`
}

type hcImage struct {
	URL string `json:"url"`
}

type hcNameField struct {
	Name string `json:"name"`
}

type hcLanguage struct {
	Language string `json:"language"`
}

type hcBook struct {
	Title         string           `json:"title"`
	Description   string           `json:"description"`
	CachedTags    json.RawMessage  `json:"cached_tags"`
	Contributions []hcContribution `json:"contributions"`
}

type hcContribution struct {
	Author *hcNameField `json:"author"`
}

// ─── Hardcover search types ───────────────────────────────────────────────────

type hcSearchGQLResponse struct {
	Data struct {
		Search struct {
			Results json.RawMessage `json:"results"`
		} `json:"search"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// hcSearchDoc is one book document from the Typesense search results.
// Hardcover may return the image as a flat "image_url" string or as a nested {"url":"..."} object.
type hcSearchDoc struct {
	Title       string   `json:"title"`
	AuthorNames []string `json:"author_names"`
	ISBNs       []string `json:"isbns"`
	ImageURL    string   `json:"image_url"` // flat form
	Image       *hcImage `json:"image"`     // nested form
	Slug        string   `json:"slug"`
}

func (d *hcSearchDoc) coverURL() string {
	if d.ImageURL != "" {
		return d.ImageURL
	}
	if d.Image != nil && d.Image.URL != "" {
		return d.Image.URL
	}
	return ""
}

// ─── Hardcover series types ───────────────────────────────────────────────────

// hcSeriesSearchDoc is one series document from the Typesense search results.
// The series index uses "name" (not "title") as the display field; older
// assumptions about "title" returned empty-name rows for every hit.
type hcSeriesSearchDoc struct {
	Name       string `json:"name"`
	Slug       string `json:"slug"`
	ImageURL   string `json:"image_url"`
	BooksCount *int   `json:"books_count"`
}

type hcSeriesVolumesGQLResponse struct {
	Data struct {
		Series []hcSeriesData `json:"series"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type hcSeriesData struct {
	ID         int            `json:"id"`
	Name       string         `json:"name"`
	BooksCount int            `json:"books_count"`
	// Hardcover's schema field is "book_series" (verified live against
	// api.hardcover.app — "series_books" doesn't exist and fails GraphQL
	// validation outright, so this was broken for every Hardcover-linked
	// series regardless of data quality, not an edge case).
	SeriesBooks []hcSeriesBook `json:"book_series"`
}

type hcSeriesBook struct {
	Position float64     `json:"position"`
	Book     *hcBookData `json:"book"`
}

type hcBookData struct {
	Title       string   `json:"title"`
	ReleaseDate string   `json:"release_date"`
	Image       *hcImage `json:"image"`
}
