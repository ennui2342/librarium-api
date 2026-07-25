// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package books

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestISFDBProvider(t *testing.T, handler http.HandlerFunc) *ISFDBProvider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p := NewISFDBProvider()
	p.Configure(map[string]string{"base_url": srv.URL})
	return p
}

func TestISFDBProvider_Configure(t *testing.T) {
	p := NewISFDBProvider()
	if p.Enabled() {
		t.Fatal("provider should start disabled with no base_url configured")
	}

	p.Configure(map[string]string{"base_url": "http://example.invalid:8080/"})
	if !p.Enabled() {
		t.Fatal("provider should be enabled once base_url is set")
	}
	if p.baseURL != "http://example.invalid:8080" {
		t.Errorf("baseURL = %q, want trailing slash trimmed", p.baseURL)
	}

	p.Configure(map[string]string{"base_url": "http://example.invalid:8080", "enabled": "false"})
	if p.Enabled() {
		t.Fatal("explicit enabled=false should disable the provider even with a base_url set")
	}
}

func TestISFDBProvider_LookupByISBN(t *testing.T) {
	pages := 158
	p := newTestISFDBProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/isbn/0586028463" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(isfdbBook{
			Title:       "Camp Concentration",
			Authors:     []string{"Thomas M. Disch"},
			Publisher:   "Panther",
			PublishDate: "1973-00-00",
			ISBN10:      "0586028463",
			ISBN13:      "9780586028469",
			PageCount:   &pages,
		})
	})

	got, err := p.LookupByISBN(context.Background(), "0586028463")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.Title != "Camp Concentration" || got.Provider != "isfdb" {
		t.Errorf("got %+v, want Camp Concentration/isfdb", got)
	}
	if got.PageCount == nil || *got.PageCount != 158 {
		t.Errorf("PageCount = %v, want 158", got.PageCount)
	}
}

func TestISFDBProvider_LookupByISBN_NotFound(t *testing.T) {
	p := newTestISFDBProvider(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})

	got, err := p.LookupByISBN(context.Background(), "0000000000")
	if err != nil {
		t.Fatalf("not-found should not be an error, got: %v", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil result for a 404", got)
	}
}

func TestISFDBProvider_LookupByISBN_ServerError(t *testing.T) {
	p := newTestISFDBProvider(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	got, err := p.LookupByISBN(context.Background(), "0586028463")
	if err == nil {
		t.Fatal("expected an error for a 500 response, got nil")
	}
	if got != nil {
		t.Errorf("got %+v, want nil result alongside the error", got)
	}
}

func TestISFDBProvider_FetchSeriesVolumes_Ordering(t *testing.T) {
	p := newTestISFDBProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/series/869/volumes" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]isfdbVolume{
			{Position: 1, Title: "Dune", ExternalID: "2036"},
			{Position: 2, Title: "Dune Messiah", ExternalID: "2037"},
			{Position: 0, Title: "Dune Trilogy (omnibus)", ExternalID: "179019"},
		})
	})

	got, err := p.FetchSeriesVolumes(context.Background(), "869")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 || got[0].Title != "Dune" || got[1].Title != "Dune Messiah" {
		t.Errorf("got %+v, want Dune then Dune Messiah first (adapter is responsible for ordering)", got)
	}
}

func TestISFDBProvider_FetchContributor_DateParsing(t *testing.T) {
	p := newTestISFDBProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(isfdbAuthor{
			ExternalID: "821",
			Name:       "Thomas M. Disch",
			BornDate:   "1940-02-02",
			DiedDate:   "2008-07-04",
			Works: []isfdbAuthorWork{
				{Title: "Camp Concentration", ISBN13: "9780586028469"},
			},
		})
	})

	got, err := p.FetchContributor(context.Background(), "821")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("got nil, want a contributor")
	}
	wantBorn := time.Date(1940, 2, 2, 0, 0, 0, 0, time.UTC)
	if got.BornDate == nil || !got.BornDate.Equal(wantBorn) {
		t.Errorf("BornDate = %v, want %v", got.BornDate, wantBorn)
	}
	if len(got.Works) != 1 || got.Works[0].Title != "Camp Concentration" {
		t.Errorf("Works = %+v, want one entry for Camp Concentration", got.Works)
	}
}

func TestParseDate(t *testing.T) {
	cases := []struct {
		in      string
		wantNil bool
	}{
		{"", true},
		{"1940-02-02", false},
		{"1978-02", false},
		{"1978", false},
		{"1973-00-00", true}, // ISFDB's zero-day/month placeholder — not a valid date
		{"garbage", true},
	}
	for _, tc := range cases {
		got := parseDate(tc.in)
		if (got == nil) != tc.wantNil {
			t.Errorf("parseDate(%q) = %v, wantNil %v", tc.in, got, tc.wantNil)
		}
	}
}
