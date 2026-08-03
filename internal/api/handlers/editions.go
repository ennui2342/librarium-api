// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/fireball1725/librarium-api/internal/api/middleware"
	"github.com/fireball1725/librarium-api/internal/api/respond"
	"github.com/fireball1725/librarium-api/internal/models"
	"github.com/fireball1725/librarium-api/internal/repository"
	"github.com/fireball1725/librarium-api/internal/service"
	"github.com/google/uuid"
)

// ─── Editions ─────────────────────────────────────────────────────────────────

// ListEditions godoc
//
// @Summary     List editions of a book
// @Description Returns all editions (physical/digital/audio) for a book.
// @Tags        editions
// @Produce     json
// @Security    BearerAuth
// @Param       library_id  path      string  true  "Library UUID"
// @Param       book_id     path      string  true  "Book UUID"
// @Success     200  {array}   responses.EditionResponse
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Router      /libraries/{library_id}/books/{book_id}/editions [get]
func (h *BookHandler) ListEditions(w http.ResponseWriter, r *http.Request) {
	bookID, err := uuid.Parse(r.PathValue("book_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid book id")
		return
	}
	editions, err := h.svc.ListEditions(r.Context(), bookID)
	if err != nil {
		respond.ServerError(w, r, err)
		return
	}

	// Batch-load files for all editions in one query.
	if len(editions) > 0 && h.editionFiles != nil {
		ids := make([]uuid.UUID, len(editions))
		for i, e := range editions {
			ids[i] = e.ID
		}
		filesMap, err := h.editionFiles.ListEditionFilesByEditions(r.Context(), ids)
		if err != nil {
			respond.ServerError(w, r, err)
			return
		}
		for _, e := range editions {
			e.Files = filesMap[e.ID]
			if len(e.Files) > 0 {
				h.editionFiles.PopulateRootPaths(r.Context(), e.Files, e.Format)
			}
		}
	}

	out := make([]map[string]any, 0, len(editions))
	for _, e := range editions {
		out = append(out, editionBody(e))
	}
	respond.JSON(w, http.StatusOK, out)
}

// CreateEdition godoc
//
// @Summary     Create an edition
// @Description Adds a new edition to an existing book.
// @Tags        editions
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Param       library_id  path      string  true  "Library UUID"
// @Param       book_id     path      string  true  "Book UUID"
// @Param       body        body      object{format=string,language=string,edition_name=string,narrator=string,publisher=string,publish_date=string,isbn_10=string,isbn_13=string,description=string,duration_seconds=integer,page_count=integer,copy_count=integer,is_primary=boolean,acquired_at=string}  true  "Edition details"
// @Success     201  {object}  responses.EditionResponse
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Router      /libraries/{library_id}/books/{book_id}/editions [post]
func (h *BookHandler) CreateEdition(w http.ResponseWriter, r *http.Request) {
	bookID, err := uuid.Parse(r.PathValue("book_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid book id")
		return
	}
	req, err := decodeEditionRequest(r)
	if err != nil {
		respond.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	edition, err := h.svc.CreateEdition(r.Context(), bookID, *req)
	if err != nil {
		respond.ServerError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusCreated, editionBody(edition))
}

// UpdateEdition godoc
//
// @Summary     Update an edition
// @Description Replaces an edition's metadata.
// @Tags        editions
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Param       library_id   path      string  true  "Library UUID"
// @Param       book_id      path      string  true  "Book UUID"
// @Param       edition_id   path      string  true  "Edition UUID"
// @Param       body         body      object{format=string,language=string,edition_name=string,narrator=string,publisher=string,publish_date=string,isbn_10=string,isbn_13=string,description=string,duration_seconds=integer,page_count=integer,copy_count=integer,is_primary=boolean,acquired_at=string}  true  "Edition details"
// @Success     200  {object}  responses.EditionResponse
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Failure     404  {object}  object{error=string}
// @Router      /libraries/{library_id}/books/{book_id}/editions/{edition_id} [put]
func (h *BookHandler) UpdateEdition(w http.ResponseWriter, r *http.Request) {
	editionID, err := uuid.Parse(r.PathValue("edition_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid edition id")
		return
	}
	req, err := decodeEditionRequest(r)
	if err != nil {
		respond.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	edition, err := h.svc.UpdateEdition(r.Context(), editionID, *req)
	if errors.Is(err, repository.ErrNotFound) {
		respond.Error(w, http.StatusNotFound, "edition not found")
		return
	}
	if err != nil {
		respond.ServerError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, editionBody(edition))
}

// DeleteEdition godoc
//
// @Summary     Delete an edition
// @Description Permanently deletes an edition from a book.
// @Tags        editions
// @Security    BearerAuth
// @Param       library_id   path  string  true  "Library UUID"
// @Param       book_id      path  string  true  "Book UUID"
// @Param       edition_id   path  string  true  "Edition UUID"
// @Success     204
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Failure     404  {object}  object{error=string}
// @Router      /libraries/{library_id}/books/{book_id}/editions/{edition_id} [delete]
func (h *BookHandler) DeleteEdition(w http.ResponseWriter, r *http.Request) {
	editionID, err := uuid.Parse(r.PathValue("edition_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid edition id")
		return
	}
	if err := h.svc.DeleteEdition(r.Context(), editionID); errors.Is(err, repository.ErrNotFound) {
		respond.Error(w, http.StatusNotFound, "edition not found")
		return
	} else if err != nil {
		respond.ServerError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── User interactions ────────────────────────────────────────────────────────

// GetMyInteraction godoc
//
// @Summary     Get my reading interaction
// @Description Returns the current user's reading status, rating, and notes for a specific edition.
// @Tags        editions
// @Produce     json
// @Security    BearerAuth
// @Param       library_id   path      string  true  "Library UUID"
// @Param       book_id      path      string  true  "Book UUID"
// @Param       edition_id   path      string  true  "Edition UUID"
// @Success     200  {object}  responses.InteractionResponse
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Router      /libraries/{library_id}/books/{book_id}/editions/{edition_id}/my-interaction [get]
func (h *BookHandler) GetMyInteraction(w http.ResponseWriter, r *http.Request) {
	editionID, err := uuid.Parse(r.PathValue("edition_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid edition id")
		return
	}
	claims := middleware.ClaimsFromContext(r.Context())
	i, err := h.svc.GetInteraction(r.Context(), claims.UserID, editionID)
	if errors.Is(err, repository.ErrNotFound) {
		respond.JSON(w, http.StatusOK, nil)
		return
	}
	if err != nil {
		respond.ServerError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, interactionBody(i))
}

// UpsertMyInteraction godoc
//
// @Summary     Set my reading interaction
// @Description Creates or updates the current user's reading status, rating, and notes for an edition.
// @Tags        editions
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Param       library_id   path      string  true  "Library UUID"
// @Param       book_id      path      string  true  "Book UUID"
// @Param       edition_id   path      string  true  "Edition UUID"
// @Param       body         body      object{read_status=string,rating=integer,notes=string,review=string,date_started=string,date_finished=string,is_favorite=boolean}  true  "Interaction data"
// @Success     200  {object}  responses.InteractionResponse
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Router      /libraries/{library_id}/books/{book_id}/editions/{edition_id}/my-interaction [put]
func (h *BookHandler) UpsertMyInteraction(w http.ResponseWriter, r *http.Request) {
	editionID, err := uuid.Parse(r.PathValue("edition_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid edition id")
		return
	}
	claims := middleware.ClaimsFromContext(r.Context())

	req, err := decodeInteractionRequest(r)
	if err != nil {
		respond.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	i, err := h.svc.UpsertInteraction(r.Context(), claims.UserID, editionID, *req)
	if err != nil {
		respond.ServerError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, interactionBody(i))
}

// MergeMyInteraction godoc
//
// @Summary     Partially update my reading interaction
// @Description Merges fields into the current user's reading interaction for an edition — any field omitted (or, for string fields, left empty) preserves the existing value instead of clearing it. Unlike PUT, which always replaces the full record, this is safe for automated callers (e.g. an external tracker sync) that only ever have partial data for a given update.
// @Tags        editions
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Param       library_id   path      string  true  "Library UUID"
// @Param       book_id      path      string  true  "Book UUID"
// @Param       edition_id   path      string  true  "Edition UUID"
// @Param       body         body      object{read_status=string,rating=integer,notes=string,review=string,date_started=string,date_finished=string,is_favorite=boolean}  true  "Fields to merge — omit any field to leave it unchanged"
// @Success     200  {object}  responses.InteractionResponse
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Router      /libraries/{library_id}/books/{book_id}/editions/{edition_id}/my-interaction [patch]
func (h *BookHandler) MergeMyInteraction(w http.ResponseWriter, r *http.Request) {
	editionID, err := uuid.Parse(r.PathValue("edition_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid edition id")
		return
	}
	claims := middleware.ClaimsFromContext(r.Context())

	req, err := decodeMergeInteractionRequest(r)
	if err != nil {
		respond.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	i, err := h.svc.MergeInteraction(r.Context(), claims.UserID, editionID, *req)
	if err != nil {
		respond.ServerError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, interactionBody(i))
}

// DeleteMyInteraction godoc
//
// @Summary     Delete my reading interaction
// @Description Removes the current user's reading interaction for an edition.
// @Tags        editions
// @Security    BearerAuth
// @Param       library_id   path  string  true  "Library UUID"
// @Param       book_id      path  string  true  "Book UUID"
// @Param       edition_id   path  string  true  "Edition UUID"
// @Success     204
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Failure     404  {object}  object{error=string}
// @Router      /libraries/{library_id}/books/{book_id}/editions/{edition_id}/my-interaction [delete]
func (h *BookHandler) DeleteMyInteraction(w http.ResponseWriter, r *http.Request) {
	editionID, err := uuid.Parse(r.PathValue("edition_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid edition id")
		return
	}
	claims := middleware.ClaimsFromContext(r.Context())
	if err := h.svc.DeleteInteraction(r.Context(), claims.UserID, editionID); errors.Is(err, repository.ErrNotFound) {
		respond.Error(w, http.StatusNotFound, "interaction not found")
		return
	} else if err != nil {
		respond.ServerError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── Request decoders ─────────────────────────────────────────────────────────

type editionRequestBody struct {
	Format                string  `json:"format"`
	Language              string  `json:"language"`
	EditionName           string  `json:"edition_name"`
	Narrator              string  `json:"narrator"`
	Publisher             string  `json:"publisher"`
	PublishDate           string  `json:"publish_date"` // flexible date string
	ISBN10                string  `json:"isbn_10"`
	ISBN13                string  `json:"isbn_13"`
	Description           string  `json:"description"`
	DurationSeconds       *int    `json:"duration_seconds"`
	PageCount             *int    `json:"page_count"`
	CopyCount             *int    `json:"copy_count"`
	IsPrimary             bool    `json:"is_primary"`
	AcquiredAt            string  `json:"acquired_at"` // YYYY-MM-DD or ""
	NarratorContributorID *string `json:"narrator_contributor_id"`
}

func decodeEditionRequest(r *http.Request) (*service.EditionRequest, error) {
	var body editionRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, errors.New("invalid request body")
	}
	return parseEditionRequestBody(&body)
}

func parseEditionRequestBody(body *editionRequestBody) (*service.EditionRequest, error) {
	if body.Format == "" {
		return nil, errors.New("format is required")
	}

	var publishDate *time.Time
	if body.PublishDate != "" {
		if t, ok := parseFlexDate(body.PublishDate); ok {
			publishDate = &t
		}
		// Silently treat unparseable dates as null — provider data varies.
	}

	copyCount := 1
	if body.CopyCount != nil && *body.CopyCount > 0 {
		copyCount = *body.CopyCount
	}

	var acquiredAt *time.Time
	if body.AcquiredAt != "" {
		if t, err := time.Parse("2006-01-02", body.AcquiredAt); err == nil {
			acquiredAt = &t
		}
	}

	var narratorContributorID *uuid.UUID
	if body.NarratorContributorID != nil && *body.NarratorContributorID != "" {
		parsed, err := uuid.Parse(*body.NarratorContributorID)
		if err != nil {
			return nil, errors.New("invalid narrator_contributor_id")
		}
		narratorContributorID = &parsed
	}

	return &service.EditionRequest{
		Format:                body.Format,
		Language:              body.Language,
		EditionName:           body.EditionName,
		Narrator:              body.Narrator,
		Publisher:             body.Publisher,
		PublishDate:           publishDate,
		ISBN10:                body.ISBN10,
		ISBN13:                body.ISBN13,
		Description:           body.Description,
		DurationSeconds:       body.DurationSeconds,
		PageCount:             body.PageCount,
		CopyCount:             copyCount,
		IsPrimary:             body.IsPrimary,
		AcquiredAt:            acquiredAt,
		NarratorContributorID: narratorContributorID,
	}, nil
}

type interactionRequestBody struct {
	ReadStatus   string          `json:"read_status"`
	Rating       *int            `json:"rating"`
	Notes        string          `json:"notes"`
	Review       string          `json:"review"`
	DateStarted  string          `json:"date_started"`  // YYYY-MM-DD or ""
	DateFinished string          `json:"date_finished"` // YYYY-MM-DD or ""
	IsFavorite   bool            `json:"is_favorite"`
	Progress     json.RawMessage `json:"progress"` // {pages_read?, percent?, position?}
}

func decodeInteractionRequest(r *http.Request) (*service.InteractionRequest, error) {
	var body interactionRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, errors.New("invalid request body")
	}
	if body.ReadStatus == "" {
		body.ReadStatus = "unread"
	}

	var dateStarted, dateFinished *time.Time
	if body.DateStarted != "" {
		t, err := time.Parse("2006-01-02", body.DateStarted)
		if err != nil {
			return nil, errors.New("date_started must be YYYY-MM-DD")
		}
		dateStarted = &t
	}
	if body.DateFinished != "" {
		t, err := time.Parse("2006-01-02", body.DateFinished)
		if err != nil {
			return nil, errors.New("date_finished must be YYYY-MM-DD")
		}
		dateFinished = &t
	}

	return &service.InteractionRequest{
		ReadStatus:   body.ReadStatus,
		Rating:       body.Rating,
		Notes:        body.Notes,
		Review:       body.Review,
		DateStarted:  dateStarted,
		DateFinished: dateFinished,
		IsFavorite:   body.IsFavorite,
		Progress:     body.Progress,
	}, nil
}

type mergeInteractionRequestBody struct {
	ReadStatus   string          `json:"read_status"`
	Rating       *int            `json:"rating"`
	Notes        string          `json:"notes"`
	Review       string          `json:"review"`
	DateStarted  string          `json:"date_started"`  // YYYY-MM-DD or "" (omit to leave unchanged)
	DateFinished string          `json:"date_finished"` // YYYY-MM-DD or "" (omit to leave unchanged)
	IsFavorite   *bool           `json:"is_favorite"`
	Progress     json.RawMessage `json:"progress"`
}

// decodeMergeInteractionRequest differs from decodeInteractionRequest in the
// one way that matters for merge semantics: it never defaults ReadStatus to
// "unread" when omitted (that default is correct for the PUT/replace flow,
// where the client always sends the full record, but would wrongly force
// every partial update back to "unread" here) and IsFavorite decodes as a
// *bool so "omitted" and "explicitly false" stay distinguishable.
func decodeMergeInteractionRequest(r *http.Request) (*service.MergeInteractionRequest, error) {
	var body mergeInteractionRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, errors.New("invalid request body")
	}

	var dateStarted, dateFinished *time.Time
	if body.DateStarted != "" {
		t, err := time.Parse("2006-01-02", body.DateStarted)
		if err != nil {
			return nil, errors.New("date_started must be YYYY-MM-DD")
		}
		dateStarted = &t
	}
	if body.DateFinished != "" {
		t, err := time.Parse("2006-01-02", body.DateFinished)
		if err != nil {
			return nil, errors.New("date_finished must be YYYY-MM-DD")
		}
		dateFinished = &t
	}

	return &service.MergeInteractionRequest{
		ReadStatus:   body.ReadStatus,
		Rating:       body.Rating,
		Notes:        body.Notes,
		Review:       body.Review,
		DateStarted:  dateStarted,
		DateFinished: dateFinished,
		IsFavorite:   body.IsFavorite,
		Progress:     body.Progress,
	}, nil
}

// ─── Response helpers ─────────────────────────────────────────────────────────

func editionBody(e *models.BookEdition) map[string]any {
	files := make([]map[string]any, 0, len(e.Files))
	for _, ef := range e.Files {
		files = append(files, editionFileBody(ef))
	}
	body := map[string]any{
		"id":                        e.ID,
		"book_id":                   e.BookID,
		"format":                    e.Format,
		"language":                  e.Language,
		"edition_name":              e.EditionName,
		"narrator":                  e.Narrator,
		"publisher":                 e.Publisher,
		"isbn_10":                   e.ISBN10,
		"isbn_13":                   e.ISBN13,
		"description":               e.Description,
		"is_primary":                e.IsPrimary,
		"created_at":                e.CreatedAt,
		"updated_at":                e.UpdatedAt,
		"narrator_contributor_name": e.NarratorContributorName,
		"files":                     files,
	}
	// copy_count and acquired_at are now per-library (library_book_editions).
	// Callers that need those for a specific library should query that
	// junction directly. Left out of the legacy per-edition body.
	if e.NarratorContributorID != nil {
		body["narrator_contributor_id"] = e.NarratorContributorID
	} else {
		body["narrator_contributor_id"] = nil
	}
	if e.PublishDate != nil {
		body["publish_date"] = e.PublishDate.Format("2006-01-02")
	} else {
		body["publish_date"] = nil
	}
	if e.DurationSeconds != nil {
		body["duration_seconds"] = *e.DurationSeconds
	} else {
		body["duration_seconds"] = nil
	}
	if e.PageCount != nil {
		body["page_count"] = *e.PageCount
	} else {
		body["page_count"] = nil
	}
	return body
}

func interactionBody(i *models.UserBookInteraction) map[string]any {
	body := map[string]any{
		"id":              i.ID,
		"user_id":         i.UserID,
		"book_edition_id": i.BookEditionID,
		"read_status":     i.ReadStatus,
		"notes":           i.Notes,
		"review":          i.Review,
		"is_favorite":     i.IsFavorite,
		"reread_count":    i.RereadCount,
		"created_at":      i.CreatedAt,
		"updated_at":      i.UpdatedAt,
	}
	if len(i.Progress) > 0 {
		body["progress"] = json.RawMessage(i.Progress)
	} else {
		body["progress"] = nil
	}
	if i.Rating != nil {
		body["rating"] = *i.Rating
	} else {
		body["rating"] = nil
	}
	if i.DateStarted != nil {
		body["date_started"] = i.DateStarted.Format("2006-01-02")
	} else {
		body["date_started"] = nil
	}
	if i.DateFinished != nil {
		body["date_finished"] = i.DateFinished.Format("2006-01-02")
	} else {
		body["date_finished"] = nil
	}
	return body
}
