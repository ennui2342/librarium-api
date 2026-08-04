// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/fireball1725/librarium-api/internal/api/middleware"
	"github.com/fireball1725/librarium-api/internal/api/respond"
	"github.com/fireball1725/librarium-api/internal/models"
	"github.com/fireball1725/librarium-api/internal/repository"
	"github.com/fireball1725/librarium-api/internal/service"
	"github.com/fireball1725/librarium-api/internal/workers"
	"github.com/google/uuid"
)

type ImportHandler struct {
	svc         *service.ImportService
	memberships *repository.MembershipRepo
	// worker is used directly (bypassing ImportService) only for
	// ResolveImportItem: the create/attach logic it needs to reuse lives on
	// ImportWorker, which already imports internal/service (for
	// contributor sort-name derivation) — routing this through
	// ImportService instead would need the reverse dependency and create
	// an import cycle.
	worker *workers.ImportWorker
}

func NewImportHandler(svc *service.ImportService, memberships *repository.MembershipRepo, worker *workers.ImportWorker) *ImportHandler {
	return &ImportHandler{svc: svc, memberships: memberships, worker: worker}
}

// CreateImport godoc
//
// @Summary     Create a CSV import job
// @Description Accepts a multipart form upload with a CSV file and column mapping, and starts an import job.
// @Tags        imports
// @Accept      multipart/form-data
// @Produce     json
// @Security    BearerAuth
// @Param       library_id       path      string  true   "Library UUID"
// @Param       file             formData  file    true   "CSV file to import"
// @Param       mapping          formData  string  false  "JSON column mapping {field_name: column_index}"
// @Param       duplicate_increment_count  formData  string  false  "On duplicate ISBN: bump copy count (default false)"
// @Param       duplicate_update_from_csv  formData  string  false  "On duplicate ISBN: refresh user-interaction fields from the CSV row (default false)"
// @Param       default_format             formData  string  false  "Default edition format (default paperback)"
// @Param       enrich_metadata            formData  string  false  "Enrich metadata after import"
// @Param       enrich_covers              formData  string  false  "Fetch covers after import"
// @Param       attribute_to_user_id       formData  string  false  "User UUID to attribute reading data to (admin-only when not the caller)"
// @Success     201  {object}  models.ImportJob
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Router      /libraries/{library_id}/imports [post]
func (h *ImportHandler) CreateImport(w http.ResponseWriter, r *http.Request) {
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

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		respond.Error(w, http.StatusBadRequest, "failed to parse multipart form")
		return
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "missing file field")
		return
	}
	defer file.Close()

	csvBytes, err := io.ReadAll(file)
	if err != nil {
		respond.ServerError(w, r, err)
		return
	}

	// Parse field mapping: JSON {"title": 0, "author": 1, ...}
	var mapping map[string]int
	if mappingStr := r.FormValue("mapping"); mappingStr != "" {
		if err := json.Unmarshal([]byte(mappingStr), &mapping); err != nil {
			respond.Error(w, http.StatusBadRequest, "invalid mapping JSON")
			return
		}
	}

	dupIncrement := false
	if v := r.FormValue("duplicate_increment_count"); v != "" {
		dupIncrement, _ = strconv.ParseBool(v)
	}

	dupUpdate := false
	if v := r.FormValue("duplicate_update_from_csv"); v != "" {
		dupUpdate, _ = strconv.ParseBool(v)
	}

	defaultFormat := r.FormValue("default_format")
	if defaultFormat == "" {
		defaultFormat = "paperback"
	}

	enrichMetadata := false
	if em := r.FormValue("enrich_metadata"); em != "" {
		enrichMetadata, _ = strconv.ParseBool(em)
	}

	enrichCovers := false
	if ec := r.FormValue("enrich_covers"); ec != "" {
		enrichCovers, _ = strconv.ParseBool(ec)
	}

	useAICleanup := false
	if v := r.FormValue("use_ai_cleanup"); v != "" {
		useAICleanup, _ = strconv.ParseBool(v)
	}

	// Optional attribution override — when set, the user-interaction
	// fields land on this user instead of the caller. Only instance
	// admins may attribute to someone else; everyone else either omits
	// the field or sends their own user id (which is a no-op).
	var attributeTo *uuid.UUID
	if v := strings.TrimSpace(r.FormValue("attribute_to_user_id")); v != "" {
		uid, err := uuid.Parse(v)
		if err != nil {
			respond.Error(w, http.StatusBadRequest, "invalid attribute_to_user_id")
			return
		}
		if uid != caller.UserID {
			if !caller.IsInstanceAdmin {
				respond.Error(w, http.StatusForbidden, "only instance admins can attribute imports to other users")
				return
			}
			isMember, err := h.memberships.IsMember(r.Context(), libraryID, uid)
			if err != nil {
				respond.ServerError(w, r, err)
				return
			}
			if !isMember {
				respond.Error(w, http.StatusBadRequest, "attribute_to_user_id is not a member of this library")
				return
			}
			attributeTo = &uid
		}
	}

	req := service.ImportRequest{
		LibraryID:                   libraryID,
		CallerID:                    caller.UserID,
		CSVText:                     string(csvBytes),
		FieldMapping:                mapping,
		DuplicateIncrementCopyCount: dupIncrement,
		DuplicateUpdateFromCSV:      dupUpdate,
		DefaultFormat:               defaultFormat,
		EnrichMetadata:              enrichMetadata,
		EnrichCovers:                enrichCovers,
		UseAICleanup:                useAICleanup,
		AttributeToUserID:           attributeTo,
	}

	job, err := h.svc.StartImport(r.Context(), req)
	if err != nil {
		if strings.Contains(err.Error(), "no rows in CSV") || strings.Contains(err.Error(), "parsing CSV") {
			respond.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		respond.ServerError(w, r, err)
		return
	}

	respond.JSON(w, http.StatusCreated, job)
}

// CancelImport godoc
//
// @Summary     Cancel an import job
// @Description Requests cancellation of a running import job.
// @Tags        imports
// @Security    BearerAuth
// @Param       import_id  path  string  true  "Import job UUID"
// @Success     204
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Failure     404  {object}  object{error=string}
// @Router      /imports/{import_id}/cancel [post]
func (h *ImportHandler) CancelImport(w http.ResponseWriter, r *http.Request) {
	caller := middleware.ClaimsFromContext(r.Context())
	if caller == nil {
		respond.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	importID, err := uuid.Parse(r.PathValue("import_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid import id")
		return
	}

	if err := h.svc.CancelImport(r.Context(), importID, caller.UserID); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			respond.Error(w, http.StatusNotFound, "job not found or not cancellable")
			return
		}
		respond.ServerError(w, r, err)
		return
	}

	respond.JSON(w, http.StatusNoContent, nil)
}

// DeleteImport godoc
//
// @Summary     Delete an import job
// @Description Deletes a finished (done/failed/cancelled) import job record.
// @Tags        imports
// @Security    BearerAuth
// @Param       import_id  path  string  true  "Import job UUID"
// @Success     204
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Failure     404  {object}  object{error=string}
// @Router      /imports/{import_id} [delete]
func (h *ImportHandler) DeleteImport(w http.ResponseWriter, r *http.Request) {
	caller := middleware.ClaimsFromContext(r.Context())
	if caller == nil {
		respond.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	importID, err := uuid.Parse(r.PathValue("import_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid import id")
		return
	}

	if err := h.svc.DeleteImport(r.Context(), importID, caller.UserID); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			respond.Error(w, http.StatusNotFound, "job not found or not deletable")
			return
		}
		respond.ServerError(w, r, err)
		return
	}

	respond.JSON(w, http.StatusNoContent, nil)
}

// DeleteFinishedImports godoc
//
// @Summary     Delete all finished import jobs
// @Description Bulk-deletes all done/failed/cancelled import jobs for the calling user.
// @Tags        imports
// @Security    BearerAuth
// @Success     204
// @Failure     401  {object}  object{error=string}
// @Router      /imports [delete]
func (h *ImportHandler) DeleteFinishedImports(w http.ResponseWriter, r *http.Request) {
	caller := middleware.ClaimsFromContext(r.Context())
	if caller == nil {
		respond.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	if err := h.svc.DeleteFinishedImports(r.Context(), caller.UserID); err != nil {
		respond.ServerError(w, r, err)
		return
	}

	respond.JSON(w, http.StatusNoContent, nil)
}

// ListAllImports godoc
//
// @Summary     List all import jobs (global)
// @Description Returns all import jobs across all libraries for the calling user.
// @Tags        imports
// @Produce     json
// @Security    BearerAuth
// @Success     200  {array}   models.ImportJob
// @Failure     401  {object}  object{error=string}
// @Router      /imports [get]
func (h *ImportHandler) ListAllImports(w http.ResponseWriter, r *http.Request) {
	caller := middleware.ClaimsFromContext(r.Context())
	if caller == nil {
		respond.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	jobs, err := h.svc.ListAllImports(r.Context(), caller.UserID)
	if err != nil {
		respond.ServerError(w, r, err)
		return
	}

	if jobs == nil {
		jobs = []models.ImportJob{}
	}
	respond.JSON(w, http.StatusOK, jobs)
}

// ListImports godoc
//
// @Summary     List import jobs for a library
// @Description Returns all import jobs for a specific library.
// @Tags        imports
// @Produce     json
// @Security    BearerAuth
// @Param       library_id  path      string  true  "Library UUID"
// @Success     200  {array}   models.ImportJob
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Router      /libraries/{library_id}/imports [get]
func (h *ImportHandler) ListImports(w http.ResponseWriter, r *http.Request) {
	libraryID, err := uuid.Parse(r.PathValue("library_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid library id")
		return
	}

	jobs, err := h.svc.ListImports(r.Context(), libraryID)
	if err != nil {
		respond.ServerError(w, r, err)
		return
	}

	if jobs == nil {
		jobs = []models.ImportJob{}
	}
	respond.JSON(w, http.StatusOK, jobs)
}

// GetImport godoc
//
// @Summary     Get an import job
// @Description Returns the status and progress of a specific import job.
// @Tags        imports
// @Produce     json
// @Security    BearerAuth
// @Param       library_id  path      string  true  "Library UUID"
// @Param       import_id   path      string  true  "Import job UUID"
// @Success     200  {object}  models.ImportJob
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Failure     404  {object}  object{error=string}
// @Router      /libraries/{library_id}/imports/{import_id} [get]
func (h *ImportHandler) GetImport(w http.ResponseWriter, r *http.Request) {
	libraryID, err := uuid.Parse(r.PathValue("library_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid library id")
		return
	}
	importID, err := uuid.Parse(r.PathValue("import_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid import id")
		return
	}

	job, err := h.svc.GetImportStatus(r.Context(), libraryID, importID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			respond.Error(w, http.StatusNotFound, "import not found")
			return
		}
		respond.ServerError(w, r, err)
		return
	}

	respond.JSON(w, http.StatusOK, job)
}

// resolveItemRequestBody is the JSON body for ResolveImportItem.
type resolveItemRequestBody struct {
	Action string     `json:"action"`
	BookID *uuid.UUID `json:"book_id,omitempty"`
}

// ResolveImportItem godoc
//
// @Summary     Resolve an ambiguous import item
// @Description Applies a human decision to an import row the worker parked as needs_review — either attach it as a new edition of one of the candidate books it found, or import it as a brand-new book.
// @Tags        imports
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Param       library_id  path  string  true  "Library UUID"
// @Param       import_id   path  string  true  "Import job UUID"
// @Param       item_id     path  string  true  "Import job item UUID"
// @Param       body        body  resolveItemRequestBody  true  "action: attach|create, book_id required for attach"
// @Success     200  {object}  models.ImportJobItem
// @Failure     400  {object}  object{error=string}
// @Failure     401  {object}  object{error=string}
// @Failure     404  {object}  object{error=string}
// @Router      /libraries/{library_id}/imports/{import_id}/items/{item_id}/resolve [post]
func (h *ImportHandler) ResolveImportItem(w http.ResponseWriter, r *http.Request) {
	caller := middleware.ClaimsFromContext(r.Context())
	if caller == nil {
		respond.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	libraryID, err := uuid.Parse(r.PathValue("library_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid library id")
		return
	}
	importID, err := uuid.Parse(r.PathValue("import_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid import id")
		return
	}
	itemID, err := uuid.Parse(r.PathValue("item_id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid item id")
		return
	}

	// Confirms the job belongs to this library before touching the item —
	// requireLibraryPerm already checked the caller's permission on
	// libraryID, this just guards against an item_id/import_id pair that
	// doesn't actually belong to it.
	if _, err := h.svc.GetImportStatus(r.Context(), libraryID, importID); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			respond.Error(w, http.StatusNotFound, "import not found")
			return
		}
		respond.ServerError(w, r, err)
		return
	}

	var body resolveItemRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respond.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Action != "attach" && body.Action != "create" {
		respond.Error(w, http.StatusBadRequest, `action must be "attach" or "create"`)
		return
	}
	if body.Action == "attach" && body.BookID == nil {
		respond.Error(w, http.StatusBadRequest, "book_id is required for the attach action")
		return
	}

	item, err := h.worker.ResolveItem(r.Context(), importID, itemID, body.Action, body.BookID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			respond.Error(w, http.StatusNotFound, "import item not found")
			return
		}
		// Anything else here is a caller error (wrong status, book_id not
		// among the candidates, unknown action) rather than a server
		// fault — ResolveItem never wraps a genuine infra failure inside
		// one of these paths without the DB/network error text showing
		// through, so 400 is the right default rather than 500.
		respond.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	respond.JSON(w, http.StatusOK, item)
}
