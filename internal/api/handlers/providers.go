// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/fireball1725/librarium-api/internal/api/respond"
	"github.com/fireball1725/librarium-api/internal/providers"
	"github.com/fireball1725/librarium-api/internal/repository"
	"github.com/fireball1725/librarium-api/internal/service"
	"github.com/google/uuid"
)

type ProviderHandler struct {
	svc *service.ProviderService
}

func NewProviderHandler(svc *service.ProviderService) *ProviderHandler {
	return &ProviderHandler{svc: svc}
}

// ListProviders godoc
//
// @Summary     List metadata providers (admin)
// @Description Returns the status and configuration of all registered metadata providers.
// @Tags        admin,providers
// @Produce     json
// @Security    BearerAuth
// @Success     200  {array}   service.ProviderStatus
// @Failure     401  {object}  object{error=string}
// @Failure     403  {object}  object{error=string}
// @Router      /admin/providers [get]
func (h *ProviderHandler) ListProviders(w http.ResponseWriter, r *http.Request) {
	statuses, err := h.svc.GetAllProviderStatus(r.Context())
	if err != nil {
		respond.ServerError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, statuses)
}

// ConfigureProvider godoc
//
// @Summary     Configure a metadata provider (admin)
// @Description Updates the API key or settings for the named provider.
// @Tags        admin,providers
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Param       name  path      string            true  "Provider name"
// @Param       body  body      object{}          true  "Provider configuration key-value pairs"
// @Success     200   {array}   service.ProviderStatus
// @Failure     400   {object}  object{error=string}
// @Failure     401   {object}  object{error=string}
// @Failure     403   {object}  object{error=string}
// @Router      /admin/providers/{name} [put]
func (h *ProviderHandler) ConfigureProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		respond.Error(w, http.StatusBadRequest, "provider name is required")
		return
	}

	var cfg map[string]string
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := h.svc.ConfigureProvider(r.Context(), name, cfg); err != nil {
		respond.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	statuses, err := h.svc.GetAllProviderStatus(r.Context())
	if err != nil {
		respond.ServerError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, statuses)
}

// TestProvider godoc
//
// @Summary     Test a metadata provider (admin)
// @Description Performs a live test lookup to verify provider credentials are working.
// @Tags        admin,providers
// @Produce     json
// @Security    BearerAuth
// @Param       name  path      string  true  "Provider name"
// @Success     200   {object}  object{ok=boolean,title=string}
// @Failure     400   {object}  object{error=string}
// @Failure     401   {object}  object{error=string}
// @Failure     403   {object}  object{error=string}
// @Router      /admin/providers/{name}/test [post]
func (h *ProviderHandler) TestProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		respond.Error(w, http.StatusBadRequest, "provider name is required")
		return
	}
	title, err := h.svc.TestProvider(r.Context(), name)
	if err != nil {
		respond.JSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	respond.JSON(w, http.StatusOK, map[string]any{"ok": true, "title": title})
}

// LookupISBN godoc
//
// @Summary     Lookup book by ISBN
// @Description Queries all enabled providers and returns per-provider results.
// @Tags        lookup
// @Produce     json
// @Security    BearerAuth
// @Param       isbn  path      string  true  "ISBN-10 or ISBN-13"
// @Success     200   {array}   providers.BookResult
// @Failure     400   {object}  object{error=string}
// @Failure     401   {object}  object{error=string}
// @Router      /lookup/isbn/{isbn} [get]
func (h *ProviderHandler) LookupISBN(w http.ResponseWriter, r *http.Request) {
	isbn := r.PathValue("isbn")
	if isbn == "" {
		respond.Error(w, http.StatusBadRequest, "isbn is required")
		return
	}

	results := h.svc.LookupISBN(r.Context(), isbn)
	if results == nil {
		results = []*providers.BookResult{}
	}
	respond.JSON(w, http.StatusOK, results)
}

// LookupISBNMerged godoc
//
// @Summary     Lookup ISBN merged
// @Description Returns a single merged result across all providers with per-field source attribution.
// @Tags        lookup
// @Produce     json
// @Security    BearerAuth
// @Param       isbn  path      string  true  "ISBN-10 or ISBN-13"
// @Success     200   {object}  providers.MergedBookResult
// @Failure     400   {object}  object{error=string}
// @Failure     401   {object}  object{error=string}
// @Router      /lookup/isbn/{isbn}/merged [get]
func (h *ProviderHandler) LookupISBNMerged(w http.ResponseWriter, r *http.Request) {
	isbn := r.PathValue("isbn")
	if isbn == "" {
		respond.Error(w, http.StatusBadRequest, "isbn is required")
		return
	}
	merged, err := h.svc.LookupISBNMerged(r.Context(), isbn)
	if err != nil {
		respond.ServerError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, merged)
}

// GetProviderOrder godoc
//
// @Summary     Get provider priority order (admin)
// @Description Returns the ordered list of provider names used when merging results.
// @Tags        admin,providers
// @Produce     json
// @Security    BearerAuth
// @Success     200  {object}  object{order=[]string}
// @Failure     401  {object}  object{error=string}
// @Failure     403  {object}  object{error=string}
// @Router      /admin/providers/order [get]
func (h *ProviderHandler) GetProviderOrder(w http.ResponseWriter, r *http.Request) {
	order, err := h.svc.GetProviderOrder(r.Context())
	if err != nil {
		respond.ServerError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, map[string]any{"order": order})
}

// SetProviderOrder godoc
//
// @Summary     Set provider priority order (admin)
// @Description Sets the ordered list of provider names used when merging results.
// @Tags        admin,providers
// @Accept      json
// @Security    BearerAuth
// @Param       body  body      object{order=[]string}  true  "Ordered provider names"
// @Success     204
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Failure     403  {object}  object{error=string}
// @Router      /admin/providers/order [put]
func (h *ProviderHandler) SetProviderOrder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Order []string `json:"order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Order) == 0 {
		respond.Error(w, http.StatusBadRequest, "order array is required")
		return
	}
	if err := h.svc.SetProviderOrder(r.Context(), body.Order); err != nil {
		respond.ServerError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SearchBooks godoc
//
// @Summary     Search books across providers
// @Description Queries all enabled providers for books matching the freetext query.
// @Tags        lookup
// @Produce     json
// @Security    BearerAuth
// @Param       q    query     string  true  "Search query"
// @Success     200  {array}   providers.BookResult
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Router      /lookup/books [get]
func (h *ProviderHandler) SearchBooks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		respond.Error(w, http.StatusBadRequest, "q (query) parameter is required")
		return
	}

	slog.InfoContext(r.Context(), "SearchBooks handler entered", "query", q)
	start := time.Now()

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	results := h.svc.SearchBooks(ctx, q)
	if results == nil {
		results = []*providers.BookResult{}
	}

	slog.InfoContext(r.Context(), "SearchBooks handler done",
		"query", q,
		"results", len(results),
		"elapsed", time.Since(start).String(),
		"ctx_err", ctx.Err(),
	)
	respond.JSON(w, http.StatusOK, results)
}

// BestMatches godoc
//
// @Summary     Find and rank provider candidates against a book's own metadata
// @Description Loads the book's already-known title/authors/publisher/year/language/format/ISBN, searches all enabled providers, and returns candidates scored and bucketed by similarity to that known data — ranked highest first. Distinct from SearchBooks (plain, unranked freetext) — this is the "which edition is this" flow, seeded entirely from data the book record already has.
// @Tags        lookup
// @Produce     json
// @Security    BearerAuth
// @Param       book_id  path      string  true  "Book UUID"
// @Success     200      {array}   service.ScoredResult
// @Failure     400      {object}  object{error=string}
// @Failure     401      {object}  object{error=string}
// @Failure     404      {object}  object{error=string}
// @Router      /books/{book_id}/best-matches [get]
func (h *ProviderHandler) BestMatches(w http.ResponseWriter, r *http.Request) {
	bookID, err := uuid.Parse(r.PathValue("book_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid book id")
		return
	}

	// Longer than SearchBooks' 20s: BestMatches' inner search deadline
	// (service.bestMatchesSearchDeadline, 20s) needs headroom above it, or
	// this outer timeout would cancel the request — and the still-running
	// straggler goroutines it's waiting on — before that inner deadline
	// ever gets a chance to fire and return partial results itself.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	known, err := h.svc.LoadBookFields(ctx, bookID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			respond.Error(w, http.StatusNotFound, "book not found")
			return
		}
		respond.ServerError(w, r, err)
		return
	}

	slog.InfoContext(r.Context(), "BestMatches handler entered", "book_id", bookID, "title", known.Title)
	start := time.Now()

	results := h.svc.BestMatches(ctx, known)
	if results == nil {
		results = []service.ScoredResult{}
	}

	slog.InfoContext(r.Context(), "BestMatches handler done",
		"book_id", bookID,
		"results", len(results),
		"elapsed", time.Since(start).String(),
	)
	respond.JSON(w, http.StatusOK, results)
}

// SearchSeries godoc
//
// @Summary     Search series across providers
// @Description Queries enabled providers for series matching the search term.
// @Tags        lookup
// @Produce     json
// @Security    BearerAuth
// @Param       q    query     string  true  "Search query"
// @Success     200  {array}   providers.SeriesResult
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Router      /lookup/series [get]
func (h *ProviderHandler) SearchSeries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		respond.Error(w, http.StatusBadRequest, "q (query) parameter is required")
		return
	}

	results := h.svc.SearchSeries(r.Context(), q)
	if results == nil {
		results = []providers.SeriesResult{}
	}
	respond.JSON(w, http.StatusOK, results)
}
