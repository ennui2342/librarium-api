// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fireball1725/librarium-api/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ImportJobRepo struct {
	db *pgxpool.Pool
}

func NewImportJobRepo(db *pgxpool.Pool) *ImportJobRepo {
	return &ImportJobRepo{db: db}
}

// CreateJob inserts a new import job and its items in a single transaction.
func (r *ImportJobRepo) CreateJob(ctx context.Context, job *models.ImportJob, items []models.ImportJobItem) error {
	optionsJSON, err := json.Marshal(job.Options)
	if err != nil {
		return fmt.Errorf("marshaling options: %w", err)
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Create the umbrella jobs row first so the import row can reference it
	// via job_id. Kind is "import"; status mirrors the import row.
	var umbrellaID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO jobs (kind, status, triggered_by, created_by)
		VALUES ('import', $1, 'user', $2)
		RETURNING id`,
		normalizeStatusForJobs(string(job.Status)), job.CreatedBy,
	).Scan(&umbrellaID); err != nil {
		return fmt.Errorf("creating umbrella job: %w", err)
	}

	const qJob = `
		INSERT INTO import_jobs (id, job_id, library_id, created_by, status, total_rows, options)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`
	if _, err := tx.Exec(ctx, qJob,
		job.ID, umbrellaID, job.LibraryID, job.CreatedBy,
		string(job.Status), job.TotalRows, optionsJSON,
	); err != nil {
		return fmt.Errorf("inserting import job: %w", err)
	}

	for _, item := range items {
		rawJSON, err := json.Marshal(item.RawData)
		if err != nil {
			return fmt.Errorf("marshaling raw data: %w", err)
		}
		const qItem = `
			INSERT INTO import_job_items (id, import_job_id, row_number, raw_data, title, isbn)
			VALUES ($1, $2, $3, $4, $5, $6)`
		if _, err := tx.Exec(ctx, qItem,
			item.ID, item.ImportJobID, item.RowNumber, rawJSON, item.Title, item.ISBN,
		); err != nil {
			return fmt.Errorf("inserting import job item: %w", err)
		}
	}

	return tx.Commit(ctx)
}

// GetJob returns an import job with all its items.
func (r *ImportJobRepo) GetJob(ctx context.Context, id uuid.UUID) (*models.ImportJob, error) {
	const qJob = `
		SELECT id, library_id, created_by, status, total_rows, processed_rows, failed_rows, skipped_rows, needs_review_rows, options, created_at, updated_at
		FROM import_jobs WHERE id = $1`

	var (
		pgID        pgtype.UUID
		pgLibraryID pgtype.UUID
		pgCreatedBy pgtype.UUID
		optJSON     []byte
		job         models.ImportJob
	)
	row := r.db.QueryRow(ctx, qJob, id)
	if err := row.Scan(
		&pgID, &pgLibraryID, &pgCreatedBy,
		&job.Status, &job.TotalRows, &job.ProcessedRows, &job.FailedRows, &job.SkippedRows, &job.NeedsReviewRows,
		&optJSON, &job.CreatedAt, &job.UpdatedAt,
	); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("scanning import job: %w", err)
	}
	job.ID = uuid.UUID(pgID.Bytes)
	job.LibraryID = uuid.UUID(pgLibraryID.Bytes)
	job.CreatedBy = uuid.UUID(pgCreatedBy.Bytes)
	if err := json.Unmarshal(optJSON, &job.Options); err != nil {
		return nil, fmt.Errorf("unmarshaling options: %w", err)
	}

	items, err := r.listItems(ctx, id)
	if err != nil {
		return nil, err
	}
	job.Items = items
	return &job, nil
}

// GetJobByLibrary returns an import job only if it belongs to the given library.
func (r *ImportJobRepo) GetJobByLibrary(ctx context.Context, libraryID, jobID uuid.UUID) (*models.ImportJob, error) {
	job, err := r.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if job.LibraryID != libraryID {
		return nil, ErrNotFound
	}
	return job, nil
}

func (r *ImportJobRepo) listItems(ctx context.Context, jobID uuid.UUID) ([]models.ImportJobItem, error) {
	const q = `
		SELECT id, import_job_id, row_number, raw_data, status, title, isbn, message, book_id, candidates, created_at, updated_at
		FROM import_job_items
		WHERE import_job_id = $1
		ORDER BY row_number`
	rows, err := r.db.Query(ctx, q, jobID)
	if err != nil {
		return nil, fmt.Errorf("listing import job items: %w", err)
	}
	defer rows.Close()

	var out []models.ImportJobItem
	for rows.Next() {
		item, err := scanImportItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *item)
	}
	return out, rows.Err()
}

// UpdateJobStatus updates the status and counters of a job.
// It never overwrites a 'cancelled' status so a user cancel cannot be
// undone by the worker. Mirrors the status and progress counters to the
// umbrella jobs row so unified history stays in sync.
func (r *ImportJobRepo) UpdateJobStatus(ctx context.Context, id uuid.UUID, status models.ImportJobStatus, processed, failed, skipped, needsReview int) error {
	const q = `
		UPDATE import_jobs
		SET status = $2, processed_rows = $3, failed_rows = $4, skipped_rows = $5, needs_review_rows = $6, updated_at = now()
		WHERE id = $1 AND status != 'cancelled'
		RETURNING job_id, total_rows`
	var (
		pgJobID pgtype.UUID
		total   int
	)
	if err := r.db.QueryRow(ctx, q, id, string(status), processed, failed, skipped, needsReview).Scan(&pgJobID, &total); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // already cancelled / missing
		}
		return fmt.Errorf("updating import job status: %w", err)
	}
	if pgJobID.Valid {
		const updJob = `
			UPDATE jobs
			   SET status      = $2,
			       progress    = jsonb_build_object('processed', $3::int, 'failed', $4::int, 'skipped', $5::int, 'needs_review', $6::int, 'total', $7::int),
			       started_at  = CASE WHEN $2 = 'running' AND started_at IS NULL THEN NOW() ELSE started_at END,
			       finished_at = CASE WHEN $2 IN ('completed','failed','cancelled') THEN COALESCE(finished_at, NOW()) ELSE finished_at END
			 WHERE id = $1`
		if _, err := r.db.Exec(ctx, updJob, uuid.UUID(pgJobID.Bytes), normalizeStatusForJobs(string(status)), processed, failed, skipped, needsReview, total); err != nil {
			return fmt.Errorf("mirroring status to umbrella job: %w", err)
		}
	}
	return nil
}

// UpdateItemStatus updates a single item's status and message. Not used for
// transitions into ImportItemNeedsReview — that also needs to persist the
// candidate list, so it goes through SetItemNeedsReview instead.
func (r *ImportJobRepo) UpdateItemStatus(ctx context.Context, id uuid.UUID, status models.ImportItemStatus, message string, bookID *uuid.UUID) error {
	const q = `
		UPDATE import_job_items
		SET status = $2, message = $3, book_id = $4, updated_at = now()
		WHERE id = $1`
	if _, err := r.db.Exec(ctx, q, id, string(status), message, bookID); err != nil {
		return fmt.Errorf("updating import job item: %w", err)
	}
	return nil
}

// SetItemNeedsReview marks an item as needing human review and records the
// candidate books the title-fallback match found but couldn't auto-resolve.
func (r *ImportJobRepo) SetItemNeedsReview(ctx context.Context, id uuid.UUID, message string, candidates []models.TitleMatchCandidate) error {
	candJSON, err := json.Marshal(candidates)
	if err != nil {
		return fmt.Errorf("marshaling candidates: %w", err)
	}
	const q = `
		UPDATE import_job_items
		SET status = 'needs_review', message = $2, candidates = $3, updated_at = now()
		WHERE id = $1`
	if _, err := r.db.Exec(ctx, q, id, message, candJSON); err != nil {
		return fmt.Errorf("setting import job item needs_review: %w", err)
	}
	return nil
}

// GetItem returns a single import job item by id.
func (r *ImportJobRepo) GetItem(ctx context.Context, id uuid.UUID) (*models.ImportJobItem, error) {
	const q = `
		SELECT id, import_job_id, row_number, raw_data, status, title, isbn, message, book_id, candidates, created_at, updated_at
		FROM import_job_items
		WHERE id = $1`
	item, err := scanImportItem(r.db.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	return item, nil
}

// ListByUser returns all import jobs created by a user across all libraries, newest first.
// The library name is populated via a JOIN.
func (r *ImportJobRepo) ListByUser(ctx context.Context, userID uuid.UUID) ([]models.ImportJob, error) {
	const q = `
		SELECT ij.id, ij.library_id, ij.created_by, ij.status,
		       ij.total_rows, ij.processed_rows, ij.failed_rows, ij.skipped_rows, ij.needs_review_rows,
		       ij.options, ij.created_at, ij.updated_at, l.name
		FROM import_jobs ij
		JOIN libraries l ON l.id = ij.library_id
		WHERE ij.created_by = $1
		ORDER BY ij.created_at DESC`
	rows, err := r.db.Query(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("listing import jobs by user: %w", err)
	}
	defer rows.Close()

	var out []models.ImportJob
	for rows.Next() {
		var (
			pgID        pgtype.UUID
			pgLibraryID pgtype.UUID
			pgCreatedBy pgtype.UUID
			optJSON     []byte
			job         models.ImportJob
		)
		if err := rows.Scan(
			&pgID, &pgLibraryID, &pgCreatedBy,
			&job.Status, &job.TotalRows, &job.ProcessedRows, &job.FailedRows, &job.SkippedRows, &job.NeedsReviewRows,
			&optJSON, &job.CreatedAt, &job.UpdatedAt, &job.LibraryName,
		); err != nil {
			return nil, fmt.Errorf("scanning import job: %w", err)
		}
		job.ID = uuid.UUID(pgID.Bytes)
		job.LibraryID = uuid.UUID(pgLibraryID.Bytes)
		job.CreatedBy = uuid.UUID(pgCreatedBy.Bytes)
		if err := json.Unmarshal(optJSON, &job.Options); err != nil {
			return nil, fmt.Errorf("unmarshaling options: %w", err)
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// CancelJob marks a pending or processing import job as cancelled.
// Only the job's creator can cancel it.
func (r *ImportJobRepo) CancelJob(ctx context.Context, jobID, userID uuid.UUID) error {
	const q = `
		UPDATE import_jobs
		SET status = 'cancelled', updated_at = now()
		WHERE id = $1 AND created_by = $2 AND status IN ('pending', 'processing')`
	tag, err := r.db.Exec(ctx, q, jobID, userID)
	if err != nil {
		return fmt.Errorf("cancelling import job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListByLibrary returns all import jobs for a library, newest first, without items.
func (r *ImportJobRepo) ListByLibrary(ctx context.Context, libraryID uuid.UUID) ([]models.ImportJob, error) {
	const q = `
		SELECT id, library_id, created_by, status, total_rows, processed_rows, failed_rows, skipped_rows, needs_review_rows, options, created_at, updated_at
		FROM import_jobs
		WHERE library_id = $1
		ORDER BY created_at DESC`
	rows, err := r.db.Query(ctx, q, libraryID)
	if err != nil {
		return nil, fmt.Errorf("listing import jobs: %w", err)
	}
	defer rows.Close()

	var out []models.ImportJob
	for rows.Next() {
		var (
			pgID        pgtype.UUID
			pgLibraryID pgtype.UUID
			pgCreatedBy pgtype.UUID
			optJSON     []byte
			job         models.ImportJob
		)
		if err := rows.Scan(
			&pgID, &pgLibraryID, &pgCreatedBy,
			&job.Status, &job.TotalRows, &job.ProcessedRows, &job.FailedRows, &job.SkippedRows, &job.NeedsReviewRows,
			&optJSON, &job.CreatedAt, &job.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning import job: %w", err)
		}
		job.ID = uuid.UUID(pgID.Bytes)
		job.LibraryID = uuid.UUID(pgLibraryID.Bytes)
		job.CreatedBy = uuid.UUID(pgCreatedBy.Bytes)
		if err := json.Unmarshal(optJSON, &job.Options); err != nil {
			return nil, fmt.Errorf("unmarshaling options: %w", err)
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// DeleteJob removes a finished (done/failed/cancelled) import job owned by the user.
// Returns ErrNotFound if no matching row is affected.
func (r *ImportJobRepo) DeleteJob(ctx context.Context, jobID, userID uuid.UUID) error {
	const q = `
		DELETE FROM import_jobs
		WHERE id = $1 AND created_by = $2 AND status IN ('done', 'failed', 'cancelled')`
	tag, err := r.db.Exec(ctx, q, jobID, userID)
	if err != nil {
		return fmt.Errorf("deleting import job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteFinishedJobs removes all done/failed/cancelled jobs created by the user.
func (r *ImportJobRepo) DeleteFinishedJobs(ctx context.Context, userID uuid.UUID) error {
	const q = `
		DELETE FROM import_jobs
		WHERE created_by = $1 AND status IN ('done', 'failed', 'cancelled')`
	if _, err := r.db.Exec(ctx, q, userID); err != nil {
		return fmt.Errorf("deleting finished import jobs: %w", err)
	}
	return nil
}

// ListPendingItems returns all pending items for a job, ordered by row_number.
func (r *ImportJobRepo) ListPendingItems(ctx context.Context, jobID uuid.UUID) ([]models.ImportJobItem, error) {
	const q = `
		SELECT id, import_job_id, row_number, raw_data, status, title, isbn, message, book_id, candidates, created_at, updated_at
		FROM import_job_items
		WHERE import_job_id = $1 AND status = 'pending'
		ORDER BY row_number`
	rows, err := r.db.Query(ctx, q, jobID)
	if err != nil {
		return nil, fmt.Errorf("listing pending items: %w", err)
	}
	defer rows.Close()

	var out []models.ImportJobItem
	for rows.Next() {
		item, err := scanImportItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *item)
	}
	return out, rows.Err()
}

func scanImportItem(s scanner) (*models.ImportJobItem, error) {
	var (
		pgID     pgtype.UUID
		pgJobID  pgtype.UUID
		pgBookID pgtype.UUID
		rawJSON  []byte
		candJSON []byte
		item     models.ImportJobItem
	)

	if err := s.Scan(
		&pgID, &pgJobID, &item.RowNumber, &rawJSON,
		&item.Status, &item.Title, &item.ISBN, &item.Message,
		&pgBookID, &candJSON, &item.CreatedAt, &item.UpdatedAt,
	); err != nil {
		return nil, fmt.Errorf("scanning import item: %w", err)
	}
	item.ID = uuid.UUID(pgID.Bytes)
	item.ImportJobID = uuid.UUID(pgJobID.Bytes)
	if pgBookID.Valid {
		id := uuid.UUID(pgBookID.Bytes)
		item.BookID = &id
	}
	if len(rawJSON) > 0 {
		if err := json.Unmarshal(rawJSON, &item.RawData); err != nil {
			return nil, fmt.Errorf("unmarshaling raw data: %w", err)
		}
	}
	if len(candJSON) > 0 {
		if err := json.Unmarshal(candJSON, &item.Candidates); err != nil {
			return nil, fmt.Errorf("unmarshaling candidates: %w", err)
		}
	}
	return &item, nil
}

// JobRef is the lightweight (id, library_id, library_name, subtype)
// tuple the unified jobs view needs to deep-link an umbrella job_id
// back to its per-kind detail row. LibraryName is the JOIN-loaded
// display label — without it the unified row falls back to the raw
// library UUID. Subtype is per-kind specific: enrichment batches use
// "metadata" / "cover" so the unified row can render the right badge.
type JobRef struct {
	ID          uuid.UUID
	LibraryID   uuid.UUID
	LibraryName string
	Subtype     string
}

// LookupByJobIDs maps umbrella job_id → JobRef for every input id with a
// matching import_jobs row. Missing rows are silently absent.
func (r *ImportJobRepo) LookupByJobIDs(ctx context.Context, jobIDs []uuid.UUID) (map[uuid.UUID]JobRef, error) {
	out := make(map[uuid.UUID]JobRef, len(jobIDs))
	if len(jobIDs) == 0 {
		return out, nil
	}
	const q = `
		SELECT ij.id, ij.library_id, ij.job_id, l.name
		FROM   import_jobs ij
		JOIN   libraries  l ON l.id = ij.library_id
		WHERE  ij.job_id = ANY($1)`
	rows, err := r.db.Query(ctx, q, jobIDs)
	if err != nil {
		return nil, fmt.Errorf("looking up import jobs by job_id: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var pgID, pgLibraryID, pgJobID pgtype.UUID
		var libName string
		if err := rows.Scan(&pgID, &pgLibraryID, &pgJobID, &libName); err != nil {
			return nil, fmt.Errorf("scanning job ref: %w", err)
		}
		out[uuid.UUID(pgJobID.Bytes)] = JobRef{
			ID:          uuid.UUID(pgID.Bytes),
			LibraryID:   uuid.UUID(pgLibraryID.Bytes),
			LibraryName: libName,
		}
	}
	return out, rows.Err()
}
