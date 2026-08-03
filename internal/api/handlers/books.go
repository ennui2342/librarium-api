// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fireball1725/librarium-api/internal/api/middleware"
	"github.com/fireball1725/librarium-api/internal/api/respond"
	"github.com/fireball1725/librarium-api/internal/models"
	"github.com/fireball1725/librarium-api/internal/repository"
	"github.com/fireball1725/librarium-api/internal/search"
	"github.com/fireball1725/librarium-api/internal/service"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

type BookHandler struct {
	svc               *service.BookService
	books             *repository.BookRepo             // used for TitlesByIDs at batch creation
	loans             *repository.LoanRepo             // active loans on GetBook responses
	riverClient       *river.Client[pgx.Tx]            // may be nil; used for enrichment batch jobs
	enrichmentBatches *repository.EnrichmentBatchRepo  // may be nil; required for bulk enrich/cover
	editionFiles      *service.EditionFileService
}

func NewBookHandler(svc *service.BookService, books *repository.BookRepo, loans *repository.LoanRepo, riverClient *river.Client[pgx.Tx], enrichmentBatches *repository.EnrichmentBatchRepo, editionFiles *service.EditionFileService) *BookHandler {
	return &BookHandler{svc: svc, books: books, loans: loans, riverClient: riverClient, enrichmentBatches: enrichmentBatches, editionFiles: editionFiles}
}

// ─── Media types ──────────────────────────────────────────────────────────────

func (h *BookHandler) ListMediaTypes(w http.ResponseWriter, r *http.Request) {
	mts, err := h.svc.ListMediaTypes(r.Context())
	if err != nil {
		respond.ServerError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(mts))
	for _, mt := range mts {
		out = append(out, map[string]any{
			"id":           mt.ID,
			"name":         mt.Name,
			"display_name": mt.DisplayName,
		})
	}
	respond.JSON(w, http.StatusOK, out)
}

// ─── Contributors ─────────────────────────────────────────────────────────────

// SearchContributors godoc
//
// @Summary     Search contributors
// @Description Full-text search for contributors (authors, illustrators, etc.). Query must be at least 2 characters.
// @Tags        books
// @Produce     json
// @Security    BearerAuth
// @Param       q    query     string  true  "Search query (min 2 chars)"
// @Success     200  {array}   responses.ContributorItem
// @Failure     401  {object}  object{error=string}
// @Router      /contributors [get]
func (h *BookHandler) SearchContributors(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if len(q) < 2 {
		respond.JSON(w, http.StatusOK, []any{})
		return
	}
	contributors, err := h.svc.SearchContributors(r.Context(), q)
	if err != nil {
		respond.ServerError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(contributors))
	for _, c := range contributors {
		out = append(out, map[string]any{"id": c.ID, "name": c.Name})
	}
	respond.JSON(w, http.StatusOK, out)
}

// CreateContributor godoc
//
// @Summary     Create a contributor
// @Description Creates a new contributor record (author, illustrator, etc.).
// @Tags        books
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Param       body  body      object{name=string}  true  "Contributor name"
// @Success     201   {object}  responses.ContributorItem
// @Failure     400   {object}  object{error=string}
// @Failure     401   {object}  object{error=string}
// @Router      /contributors [post]
func (h *BookHandler) CreateContributor(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Name == "" {
		respond.Error(w, http.StatusBadRequest, "name is required")
		return
	}
	c, err := h.svc.CreateContributor(r.Context(), body.Name)
	if err != nil {
		respond.ServerError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusCreated, map[string]any{"id": c.ID, "name": c.Name})
}

// ─── Books ────────────────────────────────────────────────────────────────────

// ListBookLetters godoc
//
// @Summary     List available first letters
// @Description Returns the sorted list of first letters (by sort title) that have books in this library.
// @Tags        books
// @Produce     json
// @Security    BearerAuth
// @Param       library_id  path      string  true  "Library UUID"
// @Success     200  {array}   string
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Router      /libraries/{library_id}/books/letters [get]
func (h *BookHandler) ListBookLetters(w http.ResponseWriter, r *http.Request) {
	libraryID, err := uuid.Parse(r.PathValue("library_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid library id")
		return
	}
	letters, err := h.svc.ListBookLetters(r.Context(), libraryID)
	if err != nil {
		respond.ServerError(w, r, err)
		return
	}
	if letters == nil {
		letters = []string{}
	}
	respond.JSON(w, http.StatusOK, letters)
}

// GetBookFingerprint godoc
//
// @Summary     Get books collection fingerprint
// @Description Returns a cheap summary (count + max updated_at) that clients
// @Description can compare against a stored value to decide whether to resync.
// @Tags        books
// @Produce     json
// @Security    BearerAuth
// @Param       library_id  path      string  true  "Library UUID"
// @Success     200  {object}  repository.BookFingerprint
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Router      /libraries/{library_id}/books/fingerprint [get]
func (h *BookHandler) GetBookFingerprint(w http.ResponseWriter, r *http.Request) {
	libraryID, err := uuid.Parse(r.PathValue("library_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid library id")
		return
	}
	fp, err := h.svc.BookFingerprint(r.Context(), libraryID)
	if err != nil {
		respond.ServerError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, fp)
}

// ListBooks godoc
//
// @Summary     List books in a library
// @Description Returns a paginated, filtered, sorted list of books in the library.
// @Tags        books
// @Produce     json
// @Security    BearerAuth
// @Param       library_id   path      string   true   "Library UUID"
// @Param       q            query     string   false  "Search query or query-language expression"
// @Param       page         query     integer  false  "Page number"
// @Param       per_page     query     integer  false  "Items per page"
// @Param       sort         query     string   false  "Sort field"
// @Param       sort_dir     query     string   false  "Sort direction (asc/desc)"
// @Param       letter       query     string   false  "Filter by first letter"
// @Param       tag          query     string   false  "Filter by tag name"
// @Param       type_filter  query     string   false  "Filter by media type"
// @Param       regex        query     boolean  false  "Treat q as regex"
// @Success     200  {object}  responses.PagedBooksResponse
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Router      /libraries/{library_id}/books [get]
func (h *BookHandler) ListBooks(w http.ResponseWriter, r *http.Request) {
	libraryID, err := uuid.Parse(r.PathValue("library_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid library id")
		return
	}

	q := r.URL.Query().Get("q")
	if len(q) > 500 {
		respond.Error(w, http.StatusBadRequest, "query too long")
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	sort := r.URL.Query().Get("sort")
	sortDir := r.URL.Query().Get("sort_dir")
	letter := r.URL.Query().Get("letter")
	if len(letter) > 5 {
		letter = ""
	}
	tagFilter := r.URL.Query().Get("tag")
	typeFilter := r.URL.Query().Get("type_filter")
	isRegex, _ := strconv.ParseBool(r.URL.Query().Get("regex"))
	isExact, _ := strconv.ParseBool(r.URL.Query().Get("exact"))

	var filterGroups []repository.ConditionGroup
	if filterJSON := r.URL.Query().Get("filter"); filterJSON != "" {
		// Structured filter JSON (legacy frontend format):
		// {"groups": [{mode, conditions}]} or flat {"mode", "conditions": [...]}
		var newFmt struct {
			Groups []repository.ConditionGroup `json:"groups"`
		}
		if err := json.Unmarshal([]byte(filterJSON), &newFmt); err == nil && len(newFmt.Groups) > 0 {
			filterGroups = newFmt.Groups
		} else {
			var oldFmt struct {
				Mode       string                       `json:"mode"`
				Conditions []repository.FilterCondition `json:"conditions"`
			}
			if err := json.Unmarshal([]byte(filterJSON), &oldFmt); err == nil && len(oldFmt.Conditions) > 0 {
				filterGroups = []repository.ConditionGroup{{Mode: oldFmt.Mode, Conditions: oldFmt.Conditions}}
			}
		}
	} else if q != "" && !isExact {
		// Parse the raw query string using the backend query language parser.
		// Clients can send q=bleach+not+type:Manga; the server handles the full parse.
		// Skipped in exact mode — exact=true means "this is a literal title
		// to match, not a query-language expression," so q must survive
		// into ListBooksOpts.Query for BookRepo.List's Exact branch.
		parsed := search.Parse(q)
		q = "" // groups take over; clear the legacy simple-text-search fallback

		// Extract letter:X conditions into the dedicated Letter opt (uses sort_title ordering).
		for _, group := range parsed {
			var remaining []repository.FilterCondition
			for _, cond := range group.Conditions {
				if cond.Field == "letter" && cond.Op == "equals" && letter == "" {
					letter = cond.Value
				} else {
					remaining = append(remaining, cond)
				}
			}
			if len(remaining) > 0 {
				filterGroups = append(filterGroups, repository.ConditionGroup{
					Mode:       group.Mode,
					Conditions: remaining,
				})
			}
		}
	}

	claims := middleware.ClaimsFromContext(r.Context())

	var callerID uuid.UUID
	if claims != nil {
		callerID = claims.UserID
	}

	books, total, err := h.svc.ListBooks(r.Context(), libraryID, repository.ListBooksOpts{
		Query:      q,
		Page:       page,
		PerPage:    perPage,
		Sort:       sort,
		SortDir:    sortDir,
		Letter:     letter,
		TagFilter:  tagFilter,
		TypeFilter: typeFilter,
		IsRegex:    isRegex,
		Exact:      isExact,
		Groups:     filterGroups,
		CallerID:   callerID,
	})
	if err != nil {
		respond.ServerError(w, r, err)
		return
	}

	items := make([]map[string]any, 0, len(books))
	for _, b := range books {
		items = append(items, bookBody(b))
	}
	resp := map[string]any{
		"items":    items,
		"total":    total,
		"page":     max(page, 1),
		"per_page": perPage,
	}
	// "Did you mean…" — when the literal query found nothing, run a
	// pg_trgm similarity match and include up to 5 suggestions. Skipped
	// when q is empty (pure browse) or when results came back.
	if total == 0 && q != "" {
		if suggestions, err := h.books.SearchSuggestions(r.Context(), libraryID, q); err == nil && len(suggestions) > 0 {
			resp["suggestions"] = suggestions
		}
	}
	respond.JSON(w, http.StatusOK, resp)
}

// CreateBook godoc
//
// @Summary     Create a book
// @Description Adds a new book to the library.
// @Tags        books
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Param       library_id  path      string  true  "Library UUID"
// @Param       body        body      object{title=string,subtitle=string,media_type_id=string,description=string,contributors=[]object,tag_ids=[]string,genre_ids=[]string,edition=object}  true  "Book details"
// @Success     201  {object}  responses.BookResponse
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Router      /libraries/{library_id}/books [post]
func (h *BookHandler) CreateBook(w http.ResponseWriter, r *http.Request) {
	libraryID, err := uuid.Parse(r.PathValue("library_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid library id")
		return
	}
	claims := middleware.ClaimsFromContext(r.Context())

	req, err := decodeBookRequest(r)
	if err != nil {
		respond.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	book, err := h.svc.CreateBook(r.Context(), libraryID, claims.UserID, *req)
	if err != nil {
		respond.ServerError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusCreated, bookBody(book))
}

// GetBook godoc
//
// @Summary     Get a book
// @Description Returns full details for a specific book including contributors, tags, genres, and series.
// @Tags        books
// @Produce     json
// @Security    BearerAuth
// @Param       library_id  path      string  true  "Library UUID"
// @Param       book_id     path      string  true  "Book UUID"
// @Success     200  {object}  responses.BookResponse
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Failure     404  {object}  object{error=string}
// @Router      /libraries/{library_id}/books/{book_id} [get]
func (h *BookHandler) GetBook(w http.ResponseWriter, r *http.Request) {
	bookID, err := uuid.Parse(r.PathValue("book_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid book id")
		return
	}
	libraryID, err := uuid.Parse(r.PathValue("library_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid library id")
		return
	}
	// Caller-scope the user_read_status / user_rating / user_progress_pct
	// + per-library active_loan_count columns so the per-book GET emits
	// the same shape as the list endpoint. Series detail (and any other
	// per-book lookup that needs reading state) depends on this.
	claims := middleware.ClaimsFromContext(r.Context())
	var callerID uuid.UUID
	if claims != nil {
		callerID = claims.UserID
	}
	book, err := h.svc.GetBook(r.Context(), bookID, callerID, libraryID)
	if errors.Is(err, repository.ErrNotFound) {
		respond.Error(w, http.StatusNotFound, "book not found")
		return
	}
	if err != nil {
		respond.ServerError(w, r, err)
		return
	}
	// Active loans surface only on the single-book read so the badge on
	// list views stays cheap (just the count). Errors here aren't fatal —
	// the book row is still useful without the loan list.
	loans, _ := h.loans.ListActiveByBook(r.Context(), bookID)
	body := bookBody(book)
	body["active_loans"] = loanBodies(loans)
	respond.JSON(w, http.StatusOK, body)
}

// UpdateBook godoc
//
// @Summary     Update a book
// @Description Replaces the book's metadata.
// @Tags        books
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Param       library_id  path      string  true  "Library UUID"
// @Param       book_id     path      string  true  "Book UUID"
// @Param       body        body      object{title=string,subtitle=string,media_type_id=string,description=string,contributors=[]object,tag_ids=[]string,genre_ids=[]string}  true  "Updated book"
// @Success     200  {object}  responses.BookResponse
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Failure     404  {object}  object{error=string}
// @Router      /libraries/{library_id}/books/{book_id} [put]
func (h *BookHandler) UpdateBook(w http.ResponseWriter, r *http.Request) {
	bookID, err := uuid.Parse(r.PathValue("book_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid book id")
		return
	}

	req, err := decodeBookRequest(r)
	if err != nil {
		respond.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	book, err := h.svc.UpdateBook(r.Context(), bookID, *req)
	if errors.Is(err, repository.ErrNotFound) {
		respond.Error(w, http.StatusNotFound, "book not found")
		return
	}
	if err != nil {
		respond.ServerError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, bookBody(book))
}

// FindByISBN godoc
//
// @Summary     Find book by ISBN
// @Description Searches the library for a book with the given ISBN.
// @Tags        books
// @Produce     json
// @Security    BearerAuth
// @Param       library_id  path      string  true  "Library UUID"
// @Param       isbn        path      string  true  "ISBN-10 or ISBN-13"
// @Success     200  {object}  responses.BookResponse
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Failure     404  {object}  object{error=string}
// @Router      /libraries/{library_id}/book-by-isbn/{isbn} [get]
func (h *BookHandler) FindByISBN(w http.ResponseWriter, r *http.Request) {
	libraryID, err := uuid.Parse(r.PathValue("library_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid library id")
		return
	}
	isbn := r.PathValue("isbn")
	book, err := h.svc.FindBookByISBN(r.Context(), libraryID, isbn)
	if errors.Is(err, repository.ErrNotFound) {
		respond.Error(w, http.StatusNotFound, "book not found")
		return
	}
	if err != nil {
		respond.ServerError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, bookBody(book))
}

// DeleteBook godoc
//
// @Summary     Remove a book from a library
// @Description Drops the library_books junction row for this library/book.
// @Description The book row itself stays (may be held by other libraries or
// @Description referenced by AI suggestions). Use the admin endpoint to
// @Description delete a book entirely.
// @Tags        books
// @Security    BearerAuth
// @Param       library_id  path  string  true  "Library UUID"
// @Param       book_id     path  string  true  "Book UUID"
// @Success     204
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Failure     404  {object}  object{error=string}
// @Router      /libraries/{library_id}/books/{book_id} [delete]
func (h *BookHandler) DeleteBook(w http.ResponseWriter, r *http.Request) {
	libraryID, err := uuid.Parse(r.PathValue("library_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid library id")
		return
	}
	bookID, err := uuid.Parse(r.PathValue("book_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid book id")
		return
	}
	if err := h.svc.RemoveBookFromLibrary(r.Context(), libraryID, bookID); errors.Is(err, repository.ErrNotFound) {
		respond.Error(w, http.StatusNotFound, "book not found in this library")
		return
	} else if err != nil {
		respond.ServerError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AdminDeleteBook godoc
//
// @Summary     Delete a book entirely (admin only)
// @Description Permanently deletes the books row and cascades through all
// @Description libraries, editions, user interactions, loans, and any other
// @Description references. Use with care.
// @Tags        admin
// @Security    BearerAuth
// @Param       book_id  path  string  true  "Book UUID"
// @Success     204
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Failure     403  {object}  object{error=string}
// @Failure     404  {object}  object{error=string}
// @Router      /admin/books/{book_id} [delete]
func (h *BookHandler) AdminDeleteBook(w http.ResponseWriter, r *http.Request) {
	bookID, err := uuid.Parse(r.PathValue("book_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid book id")
		return
	}
	if err := h.svc.DeleteBook(r.Context(), bookID); errors.Is(err, repository.ErrNotFound) {
		respond.Error(w, http.StatusNotFound, "book not found")
		return
	} else if err != nil {
		respond.ServerError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AddBookToLibrary godoc
//
// @Summary     Add an existing book to a library
// @Description Inserts the library_books junction row. Idempotent. Used by
// @Description the suggestions-as-books "Add to library" CTA and any other
// @Description flow that wants to attach a floating book to a real library.
// @Tags        books
// @Security    BearerAuth
// @Param       library_id  path  string  true  "Library UUID"
// @Param       book_id     path  string  true  "Book UUID"
// @Success     204
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Failure     404  {object}  object{error=string}
// @Router      /libraries/{library_id}/books/{book_id} [post]
func (h *BookHandler) AddBookToLibrary(w http.ResponseWriter, r *http.Request) {
	libraryID, err := uuid.Parse(r.PathValue("library_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid library id")
		return
	}
	bookID, err := uuid.Parse(r.PathValue("book_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid book id")
		return
	}
	var callerID *uuid.UUID
	if claims := middleware.ClaimsFromContext(r.Context()); claims != nil {
		id := claims.UserID
		callerID = &id
	}
	if err := h.svc.AddBookToLibrary(r.Context(), libraryID, bookID, callerID); err != nil {
		respond.ServerError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

type bookRequestBody struct {
	Title       string `json:"title"`
	Subtitle    string `json:"subtitle"`
	MediaTypeID string `json:"media_type_id"`
	Description string `json:"description"`
	Contributors []struct {
		ContributorID string `json:"contributor_id"`
		Role          string `json:"role"`
		DisplayOrder  int    `json:"display_order"`
	} `json:"contributors"`
	TagIDs   []string            `json:"tag_ids"`
	GenreIDs []string            `json:"genre_ids"`
	Edition  *editionRequestBody `json:"edition"`
}

func decodeBookRequest(r *http.Request) (*service.BookRequest, error) {
	var body bookRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, errors.New("invalid request body")
	}
	if body.Title == "" {
		return nil, errors.New("title is required")
	}
	mediaTypeID, err := uuid.Parse(body.MediaTypeID)
	if err != nil {
		return nil, errors.New("invalid media_type_id")
	}

	contributors := make([]repository.ContributorInput, 0, len(body.Contributors))
	for _, c := range body.Contributors {
		cid, err := uuid.Parse(c.ContributorID)
		if err != nil {
			return nil, errors.New("invalid contributor_id")
		}
		contributors = append(contributors, repository.ContributorInput{
			ContributorID: cid,
			Role:          c.Role,
			DisplayOrder:  c.DisplayOrder,
		})
	}

	tagIDs := make([]uuid.UUID, 0, len(body.TagIDs))
	for _, s := range body.TagIDs {
		tid, err := uuid.Parse(s)
		if err != nil {
			return nil, errors.New("invalid tag_id: " + s)
		}
		tagIDs = append(tagIDs, tid)
	}

	genreIDs := make([]uuid.UUID, 0, len(body.GenreIDs))
	for _, s := range body.GenreIDs {
		gid, err := uuid.Parse(s)
		if err != nil {
			return nil, errors.New("invalid genre_id: " + s)
		}
		genreIDs = append(genreIDs, gid)
	}

	var edReq *service.EditionRequest
	if body.Edition != nil {
		edReq, err = parseEditionRequestBody(body.Edition)
		if err != nil {
			return nil, err
		}
	}

	return &service.BookRequest{
		Title:        body.Title,
		Subtitle:     body.Subtitle,
		MediaTypeID:  mediaTypeID,
		Description:  body.Description,
		Contributors: contributors,
		TagIDs:       tagIDs,
		GenreIDs:     genreIDs,
		Edition:      edReq,
	}, nil
}

func bookBody(b *models.Book) map[string]any {
	var coverURL any
	if b.HasCover {
		// Library-agnostic cover URL; a book can now live in multiple libraries
		// so the cover path is keyed by book id, not library+book. Includes
		// updated_at as a cache-buster.
		coverURL = fmt.Sprintf("/api/v1/books/%s/cover?v=%d",
			b.ID, b.UpdatedAt.Unix())
	}
	// Preserve the legacy `library_id` field for clients that expect it by
	// picking the first library this book belongs to. Empty if floating.
	var primaryLibraryID any
	if len(b.Libraries) > 0 {
		primaryLibraryID = b.Libraries[0].ID
	}
	return map[string]any{
		"id":               b.ID,
		"library_id":       primaryLibraryID,
		"libraries":        b.Libraries,
		"title":            b.Title,
		"subtitle":         b.Subtitle,
		"media_type_id":    b.MediaTypeID,
		"media_type":       b.MediaType,
		"description":      b.Description,
		"contributors":     b.Contributors,
		"tags":             b.Tags,
		"genres":           b.Genres,
		"cover_url":        coverURL,
		"created_at":       b.CreatedAt,
		"updated_at":       b.UpdatedAt,
		"series":           b.Series,
		"shelves":          b.Shelves,
		"publisher":        b.Publisher,
		"publish_year":     b.PublishYear,
		"language":         b.Language,
		"user_read_status":   b.UserReadStatus,
		"user_rating":        b.UserRating,
		"user_progress_pct":  b.UserProgressPct,
		"active_loan_count":  b.ActiveLoanCount,
	}
}

// parseFlexDate tries several date formats and returns the parsed time.
// Accepts: YYYY-MM-DD, YYYY-MM, YYYY, "January 2006", "January 2, 2006",
// "Jan 2, 2006", "Jan 2006", "2 January 2006", "2 Jan 2006".
func parseFlexDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}

	// Exact formats tried in order
	for _, layout := range []string{
		"2006-01-02",
		"2006-01",
		"2006",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}

	// Year-only bare number
	if reYear.MatchString(s) {
		if t, err := time.Parse("2006", s); err == nil {
			return t, true
		}
	}

	// "Month DD, YYYY" or "Month D YYYY" (full or abbreviated)
	if m := reFullDate.FindStringSubmatch(s); m != nil {
		day := m[2]
		if len(day) == 1 {
			day = "0" + day
		}
		for _, mfmt := range []string{"January", "Jan"} {
			if t, err := time.Parse(mfmt+" 02 2006", m[1]+" "+day+" "+m[3]); err == nil {
				return t, true
			}
		}
	}

	// "Month YYYY" (full or abbreviated)
	if m := reMonYear.FindStringSubmatch(s); m != nil {
		for _, mfmt := range []string{"January 2006", "Jan 2006"} {
			if t, err := time.Parse(mfmt, m[1]+" "+m[2]); err == nil {
				return t, true
			}
		}
	}

	// "D Month YYYY" or "D Mon YYYY" (day-first European)
	if m := reDayFirst.FindStringSubmatch(s); m != nil {
		day := m[1]
		if len(day) == 1 {
			day = "0" + day
		}
		for _, mfmt := range []string{"January", "Jan"} {
			if t, err := time.Parse("02 "+mfmt+" 2006", day+" "+m[2]+" "+m[3]); err == nil {
				return t, true
			}
		}
	}

	return time.Time{}, false
}

var (
	reYear     = regexp.MustCompile(`^\d{4}$`)
	reFullDate = regexp.MustCompile(`^([A-Za-z]+)\s+(\d{1,2}),?\s+(\d{4})$`)
	reMonYear  = regexp.MustCompile(`^([A-Za-z]+)\s+(\d{4})$`)
	reDayFirst = regexp.MustCompile(`^(\d{1,2})\s+([A-Za-z]+)\s+(\d{4})$`)
)

// ─── Bulk operations ──────────────────────────────────────────────────────────

type bulkEnrichRequest struct {
	BookIDs      []uuid.UUID `json:"book_ids"`
	Force        bool        `json:"force"`
	UseAICleanup bool        `json:"use_ai_cleanup"`
}

// BulkEnrich godoc
//
// @Summary     Bulk enrich books
// @Description Creates an enrichment batch to refresh metadata for a list of books.
// @Tags        books,enrichment
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Param       library_id  path      string  true  "Library UUID"
// @Param       body        body      object{book_ids=[]string,force=boolean}  true  "Books to enrich"
// @Success     202  {object}  models.EnrichmentBatch
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Failure     503  {object}  object{error=string}
// @Router      /libraries/{library_id}/books/bulk/enrich [post]
func (h *BookHandler) BulkEnrich(w http.ResponseWriter, r *http.Request) {
	if h.riverClient == nil || h.enrichmentBatches == nil {
		respond.Error(w, http.StatusServiceUnavailable, "job queue not available")
		return
	}

	libraryID, err := uuid.Parse(r.PathValue("library_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid library id")
		return
	}

	caller := middleware.ClaimsFromContext(r.Context())
	if caller == nil {
		respond.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req bulkEnrichRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.BookIDs) == 0 {
		respond.Error(w, http.StatusBadRequest, "book_ids must not be empty")
		return
	}

	batch := &models.EnrichmentBatch{
		ID:           uuid.New(),
		LibraryID:    &libraryID,
		CreatedBy:    caller.UserID,
		Type:         models.EnrichmentBatchTypeMetadata,
		Force:        req.Force,
		UseAICleanup: req.UseAICleanup,
		Status:       models.EnrichmentBatchPending,
		BookIDs:      req.BookIDs,
		TotalBooks:   len(req.BookIDs),
	}
	if err := h.enrichmentBatches.Create(r.Context(), batch); err != nil {
		respond.ServerError(w, r, err)
		return
	}
	if err := h.createBatchItems(r.Context(), batch.ID, req.BookIDs); err != nil {
		respond.ServerError(w, r, err)
		return
	}

	if _, err := h.riverClient.Insert(r.Context(), models.EnrichmentBatchJobArgs{BatchID: batch.ID}, nil); err != nil {
		respond.ServerError(w, r, err)
		return
	}

	respond.JSON(w, http.StatusAccepted, batch)
}

// EnrichBook godoc
//
// @Summary     Re-enrich metadata for a single book
// @Description Queues a metadata enrichment job for one book, regardless of
// @Description library ownership. Used by the BookDetailPage "Refresh
// @Description metadata" button on floating (suggestion-backed) books and
// @Description anywhere we want to touch up a single title.
// @Tags        books
// @Security    BearerAuth
// @Param       book_id  path      string  true  "Book UUID"
// @Success     202      {object}  models.EnrichmentBatch
// @Failure     400      {object}  object{error=string}
// @Failure     401      {object}  object{error=string}
// @Failure     503      {object}  object{error=string}
// @Router      /books/{book_id}/enrich [post]
func (h *BookHandler) EnrichBook(w http.ResponseWriter, r *http.Request) {
	if h.riverClient == nil || h.enrichmentBatches == nil {
		respond.Error(w, http.StatusServiceUnavailable, "job queue not available")
		return
	}

	bookID, err := uuid.Parse(r.PathValue("book_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid book id")
		return
	}

	caller := middleware.ClaimsFromContext(r.Context())
	if caller == nil {
		respond.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// Body is optional; absent body means "no AI cleanup" — preserves the
	// existing zero-body POST contract.
	var body struct {
		UseAICleanup bool `json:"use_ai_cleanup"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	batch := &models.EnrichmentBatch{
		ID:           uuid.New(),
		LibraryID:    nil, // library-agnostic — works for floating books
		CreatedBy:    caller.UserID,
		Type:         models.EnrichmentBatchTypeMetadata,
		Force:        true, // one-off re-enrich always overwrites
		UseAICleanup: body.UseAICleanup,
		Status:       models.EnrichmentBatchPending,
		BookIDs:      []uuid.UUID{bookID},
		TotalBooks:   1,
	}
	if err := h.enrichmentBatches.Create(r.Context(), batch); err != nil {
		respond.ServerError(w, r, err)
		return
	}
	if err := h.createBatchItems(r.Context(), batch.ID, batch.BookIDs); err != nil {
		respond.ServerError(w, r, err)
		return
	}
	if _, err := h.riverClient.Insert(r.Context(), models.EnrichmentBatchJobArgs{BatchID: batch.ID}, nil); err != nil {
		respond.ServerError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusAccepted, batch)
}

// BulkRefreshCovers godoc
//
// @Summary     Bulk refresh covers
// @Description Creates an enrichment batch to refresh cover images for a list of books.
// @Tags        books,covers
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Param       library_id  path      string  true  "Library UUID"
// @Param       body        body      object{book_ids=[]string}  true  "Books to refresh covers for"
// @Success     202  {object}  models.EnrichmentBatch
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Failure     503  {object}  object{error=string}
// @Router      /libraries/{library_id}/books/bulk/cover [post]
func (h *BookHandler) BulkRefreshCovers(w http.ResponseWriter, r *http.Request) {
	if h.riverClient == nil || h.enrichmentBatches == nil {
		respond.Error(w, http.StatusServiceUnavailable, "job queue not available")
		return
	}

	libraryID, err := uuid.Parse(r.PathValue("library_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid library id")
		return
	}

	caller := middleware.ClaimsFromContext(r.Context())
	if caller == nil {
		respond.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req struct {
		BookIDs []uuid.UUID `json:"book_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.BookIDs) == 0 {
		respond.Error(w, http.StatusBadRequest, "book_ids must not be empty")
		return
	}

	batch := &models.EnrichmentBatch{
		ID:         uuid.New(),
		LibraryID:  &libraryID,
		CreatedBy:  caller.UserID,
		Type:       models.EnrichmentBatchTypeCover,
		Force:      false,
		Status:     models.EnrichmentBatchPending,
		BookIDs:    req.BookIDs,
		TotalBooks: len(req.BookIDs),
	}
	if err := h.enrichmentBatches.Create(r.Context(), batch); err != nil {
		respond.ServerError(w, r, err)
		return
	}
	if err := h.createBatchItems(r.Context(), batch.ID, req.BookIDs); err != nil {
		respond.ServerError(w, r, err)
		return
	}

	if _, err := h.riverClient.Insert(r.Context(), models.EnrichmentBatchJobArgs{BatchID: batch.ID}, nil); err != nil {
		respond.ServerError(w, r, err)
		return
	}

	respond.JSON(w, http.StatusAccepted, batch)
}

// createBatchItems inserts per-book item records for a new enrichment batch,
// pre-populating book titles with a single bulk query.
func (h *BookHandler) createBatchItems(ctx context.Context, batchID uuid.UUID, bookIDs []uuid.UUID) error {
	titles, _ := h.books.TitlesByIDs(ctx, bookIDs) // best-effort; empty title is fine

	items := make([]models.EnrichmentBatchItem, len(bookIDs))
	for i, bookID := range bookIDs {
		id := bookID // copy for pointer
		items[i] = models.EnrichmentBatchItem{
			ID:        uuid.New(),
			BatchID:   batchID,
			BookID:    &id,
			BookTitle: titles[bookID],
			Status:    models.EnrichmentItemPending,
		}
	}
	return h.enrichmentBatches.CreateItems(ctx, items)
}
