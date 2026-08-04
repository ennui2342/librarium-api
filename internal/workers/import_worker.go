// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

// Package workers contains River job workers.
package workers

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/fireball1725/librarium-api/internal/imports"
	"github.com/fireball1725/librarium-api/internal/models"
	"github.com/fireball1725/librarium-api/internal/repository"
	"github.com/fireball1725/librarium-api/internal/service"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// importedBook holds the ID and title of a successfully created book for batch enrichment.
type importedBook struct {
	id    uuid.UUID
	title string
}

// ImportWorker processes one CSV import job from start to finish.
type ImportWorker struct {
	river.WorkerDefaults[models.ImportJobArgs]

	pool         *pgxpool.Pool
	importJobs   *repository.ImportJobRepo
	books        *repository.BookRepo
	libraryBooks *repository.LibraryBookRepo
	contributors *repository.ContributorRepo
	editions     *repository.EditionRepo
	tags         *repository.TagRepo
	genres       *repository.GenreRepo
	batches      *repository.EnrichmentBatchRepo
	riverClient  *river.Client[pgx.Tx]
}

func NewImportWorker(
	pool *pgxpool.Pool,
	importJobs *repository.ImportJobRepo,
	books *repository.BookRepo,
	libraryBooks *repository.LibraryBookRepo,
	contributors *repository.ContributorRepo,
	editions *repository.EditionRepo,
	tags *repository.TagRepo,
	genres *repository.GenreRepo,
	batches *repository.EnrichmentBatchRepo,
	riverClient *river.Client[pgx.Tx],
) *ImportWorker {
	return &ImportWorker{
		pool:         pool,
		importJobs:   importJobs,
		books:        books,
		libraryBooks: libraryBooks,
		contributors: contributors,
		editions:     editions,
		tags:         tags,
		genres:       genres,
		batches:      batches,
		riverClient:  riverClient,
	}
}

// SetRiverClient wires in the River client after it has been constructed.
// Called from main after river.NewClient to break the initialization cycle.
func (w *ImportWorker) SetRiverClient(c *river.Client[pgx.Tx]) {
	w.riverClient = c
}

func (w *ImportWorker) Work(ctx context.Context, job *river.Job[models.ImportJobArgs]) error {
	jobID := job.Args.ImportJobID
	slog.Info("import job started", "import_job_id", jobID)

	// Load job first — bail early if it was cancelled before River picked it up.
	importJob, err := w.importJobs.GetJob(ctx, jobID)
	if err != nil {
		return fmt.Errorf("loading import job: %w", err)
	}
	if importJob.Status == models.ImportJobCancelled {
		slog.Info("import job was cancelled before processing", "import_job_id", jobID)
		return nil
	}

	if err := w.importJobs.UpdateJobStatus(ctx, jobID, models.ImportJobProcessing, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("marking job as processing: %w", err)
	}

	items, err := w.importJobs.ListPendingItems(ctx, jobID)
	if err != nil {
		return fmt.Errorf("loading pending items: %w", err)
	}

	tagCache := make(map[string]uuid.UUID) // lowercase name → id

	allGenres, err := w.genres.List(ctx)
	if err != nil {
		return fmt.Errorf("loading genres: %w", err)
	}

	var processed, failed, skipped, needsReview int
	var newBooks []importedBook // tracks newly created books for post-import enrichment batches
	for _, item := range items {
		// Check for cancellation before each item so the worker stops promptly.
		if current, cerr := w.importJobs.GetJob(ctx, jobID); cerr == nil && current.Status == models.ImportJobCancelled {
			slog.Info("import job cancelled mid-processing", "import_job_id", jobID, "processed", processed)
			return nil
		}

		status, msg, bookID, addedToLibrary, candidates := w.processItem(ctx, importJob, &item, tagCache, allGenres)
		if status == models.ImportItemNeedsReview {
			// Needs its own persistence call — UpdateItemStatus has no
			// slot for the candidate list a human will resolve against.
			_ = w.importJobs.SetItemNeedsReview(ctx, item.ID, msg, candidates)
		} else {
			_ = w.importJobs.UpdateItemStatus(ctx, item.ID, status, msg, bookID)
		}

		switch status {
		case models.ImportItemDone:
			processed++
			// Books newly added to *this* library (fresh creates and
			// links of an edition that lived in another library) are
			// queued for post-import enrichment. Pure in-library
			// duplicates that took an action (count bump or
			// interaction refresh) are excluded — those have already
			// been enriched on a prior run. The cover/metadata workers
			// short-circuit when the data is already present, so a
			// library-link with a cover already on disk is a cheap no-op.
			if addedToLibrary && bookID != nil && item.Title != "" {
				newBooks = append(newBooks, importedBook{id: *bookID, title: item.Title})
			}
		case models.ImportItemFailed:
			failed++
			slog.Warn("import row failed",
				"import_job_id", jobID,
				"row", item.RowNumber,
				"title", item.Title,
				"isbn", item.ISBN,
				"error", msg,
			)
		case models.ImportItemSkipped:
			skipped++
		case models.ImportItemNeedsReview:
			needsReview++
			slog.Info("import row needs review",
				"import_job_id", jobID,
				"row", item.RowNumber,
				"title", item.Title,
				"candidates", len(candidates),
			)
		}
		_ = w.importJobs.UpdateJobStatus(ctx, jobID, models.ImportJobProcessing, processed, failed, skipped, needsReview)
	}

	// A needs_review row does not block the job from completing — it's a
	// per-row parked state, not a job-level failure. needsReview is
	// reflected in the job's own counter for visibility (see the Jobs
	// history UI) and each row is followed up individually via ResolveItem.
	finalStatus := models.ImportJobDone
	if err := w.importJobs.UpdateJobStatus(ctx, jobID, finalStatus, processed, failed, skipped, needsReview); err != nil {
		return fmt.Errorf("finalizing import job: %w", err)
	}

	// After the import completes, spawn tracked enrichment batches so progress
	// appears in the Jobs page and River TUI as a single cohesive job.
	opts := importJob.Options
	if len(newBooks) > 0 && w.riverClient != nil && w.batches != nil {
		if opts.EnrichMetadata {
			w.spawnEnrichmentBatch(ctx, importJob, newBooks, models.EnrichmentBatchTypeMetadata)
		}
		if opts.EnrichCovers {
			w.spawnEnrichmentBatch(ctx, importJob, newBooks, models.EnrichmentBatchTypeCover)
		}
	}

	slog.Info("import job done",
		"import_job_id", jobID,
		"processed", processed,
		"failed", failed,
		"skipped", skipped,
		"needs_review", needsReview,
	)
	return nil
}

// spawnEnrichmentBatch creates an EnrichmentBatch record + items in the database and
// enqueues a single EnrichmentBatchJobArgs River job.  This makes the post-import
// enrichment visible in the Jobs page and the River TUI.
//
// For cover batches, the candidate set is filtered to books that don't
// already have a primary cover on disk — without this, importing into
// a library where most edition-links already had covers from another
// library produced a "0/1410" batch that read as broken even though
// every per-book worker call was a correct no-op.
func (w *ImportWorker) spawnEnrichmentBatch(
	ctx context.Context,
	importJob *models.ImportJob,
	books []importedBook,
	batchType models.EnrichmentBatchType,
) {
	bookIDs := make([]uuid.UUID, len(books))
	for i, b := range books {
		bookIDs[i] = b.id
	}
	if batchType == models.EnrichmentBatchTypeCover {
		needCover, err := w.books.BooksWithoutCover(ctx, bookIDs)
		if err != nil {
			slog.Warn("filtering cover candidates failed; queueing all", "error", err)
		} else {
			needSet := make(map[uuid.UUID]struct{}, len(needCover))
			for _, id := range needCover {
				needSet[id] = struct{}{}
			}
			filtered := make([]importedBook, 0, len(needCover))
			for _, b := range books {
				if _, ok := needSet[b.id]; ok {
					filtered = append(filtered, b)
				}
			}
			books = filtered
			bookIDs = needCover
		}
		if len(books) == 0 {
			slog.Info("cover batch skipped — every imported book already has a cover",
				"import_job_id", importJob.ID)
			return
		}
	}
	batchID := uuid.New()

	libraryID := importJob.LibraryID
	batch := &models.EnrichmentBatch{
		ID:           batchID,
		LibraryID:    &libraryID,
		CreatedBy:    importJob.CreatedBy,
		Type:         batchType,
		Force:        false,
		// AI cleanup applies to metadata batches only; cover-only batches don't
		// touch description text.
		UseAICleanup: batchType == models.EnrichmentBatchTypeMetadata && importJob.Options.UseAICleanup,
		Status:       models.EnrichmentBatchPending,
		BookIDs:      bookIDs,
		TotalBooks:   len(books),
	}
	if err := w.batches.Create(ctx, batch); err != nil {
		slog.Warn("creating enrichment batch after import", "type", batchType, "error", err)
		return
	}

	items := make([]models.EnrichmentBatchItem, len(books))
	for i, b := range books {
		bookIDCopy := b.id
		items[i] = models.EnrichmentBatchItem{
			ID:        uuid.New(),
			BatchID:   batchID,
			BookID:    &bookIDCopy,
			BookTitle: b.title,
			Status:    models.EnrichmentItemPending,
		}
	}
	if err := w.batches.CreateItems(ctx, items); err != nil {
		slog.Warn("creating enrichment batch items after import", "type", batchType, "error", err)
		return
	}

	if _, err := w.riverClient.Insert(ctx, models.EnrichmentBatchJobArgs{BatchID: batchID}, nil); err != nil {
		slog.Warn("enqueuing enrichment batch job after import", "type", batchType, "error", err)
	}
}

// processItem returns the per-row outcome plus an addedToLibrary flag the
// caller uses to gate post-import enrichment fan-out, and — only when the
// status is ImportItemNeedsReview — the candidate books a human can later
// resolve the row against via ResolveItem. addedToLibrary is true for any
// row that newly placed a book into the target library (true creates AND
// links of editions from other libraries); false for in-library
// duplicates, skipped rows, and needs_review rows (nothing was added yet).
func (w *ImportWorker) processItem(
	ctx context.Context,
	job *models.ImportJob,
	item *models.ImportJobItem,
	tagCache map[string]uuid.UUID,
	allGenres []*models.Genre,
) (models.ImportItemStatus, string, *uuid.UUID, bool, []models.TitleMatchCandidate) {
	opts := job.Options
	row := item.RawData

	// Reading data is normally attributed to the importer; admins can
	// retarget the whole job to another library member via the
	// attribute_to_user_id option.
	interactionUserID := job.CreatedBy
	if opts.AttributeToUserID != nil {
		interactionUserID = *opts.AttributeToUserID
	}

	title := strings.TrimSpace(row["title"])
	if title == "" {
		return models.ImportItemSkipped, "no title", nil, false, nil
	}

	isbn := strings.TrimSpace(row["isbn_13"])
	if isbn == "" {
		isbn = strings.TrimSpace(row["isbn_10"])
	}

	// ── Duplicate check (ISBN deduplication at edition level) ─────────────────
	// Editions are globally unique by ISBN under M2M. If one already
	// exists, the duplicate-handling options decide whether to bump the
	// copy count and/or refresh user-interaction fields. A book that
	// exists globally but isn't yet in this library is not a duplicate
	// from the user's perspective — we always link it and let the
	// update-from-CSV option carry the row's user-interaction data.
	if isbn != "" {
		existing, err := w.editions.FindByISBN(ctx, isbn)
		if err == nil && existing != nil {
			inLibrary, ierr := w.libraryBooks.IsBookInLibrary(ctx, job.LibraryID, existing.BookID)
			if ierr != nil {
				return models.ImportItemFailed, fmt.Sprintf("checking library membership: %v", ierr), nil, false, nil
			}
			bookID := existing.BookID

			if !inLibrary {
				// First time this library is seeing the edition — add it
				// and seed the user-interaction fields from the CSV row.
				if addErr := w.libraryBooks.AddBookToLibrary(ctx, nil, job.LibraryID, bookID, &job.CreatedBy); addErr != nil {
					return models.ImportItemFailed, fmt.Sprintf("adding book to library: %v", addErr), nil, false, nil
				}
				w.applyInteraction(ctx, existing.ID, interactionUserID, row)
				// addedToLibrary=true: the book is new to *this* library
				// even though the edition row pre-existed globally. Queue
				// it for enrichment so missing covers/metadata get filled
				// in if the original-library import skipped that step.
				// The metadata and cover workers no-op when the data is
				// already present.
				return models.ImportItemDone, fmt.Sprintf("linked existing edition (ISBN %s) into this library", isbn), &bookID, true, nil
			}

			// True duplicate — book is already in this library. Apply the
			// user's duplicate-handling preferences. Default (both off) is
			// a no-op skip so re-running an import is idempotent.
			actions := make([]string, 0, 2)
			if opts.DuplicateIncrementCopyCount {
				if incrErr := w.editions.IncrementCopyCount(ctx, job.LibraryID, existing.ID); incrErr != nil {
					return models.ImportItemFailed, fmt.Sprintf("increment copy count: %v", incrErr), nil, false, nil
				}
				actions = append(actions, "copy count incremented")
			}
			if opts.DuplicateUpdateFromCSV {
				w.applyInteraction(ctx, existing.ID, interactionUserID, row)
				actions = append(actions, "user fields updated")
			}
			if len(actions) == 0 {
				return models.ImportItemSkipped, fmt.Sprintf("duplicate ISBN %s — skipped", isbn), &bookID, false, nil
			}
			return models.ImportItemDone, fmt.Sprintf("duplicate ISBN %s — %s", isbn, strings.Join(actions, ", ")), &bookID, false, nil
		}
	}

	// ── Title fallback duplicate check ─────────────────────────────────────────
	// The ISBN check above never engages for a row with no ISBN at all
	// (common — Goodreads and other sources frequently lack ISBN data for
	// older or small-press titles), or one whose ISBN belongs to a different
	// edition/printing than what's already catalogued (e.g. a paperback vs.
	// hardcover, or a reissue with a new ISBN). Left unchecked, either case
	// falls straight through to "create a new book" below and silently
	// duplicates a work already in the library. Before doing that, look for
	// existing books in this library with an exact normalized-title match
	// (see imports.NormalizeTitle).
	//
	// A title match alone is not proof of a duplicate — two distinct books
	// can share a title. So the match is only auto-resolved (attach this
	// row as a new edition of the matched book) when there is exactly one
	// candidate AND the row's author overlaps that book's contributors.
	// Anything less certain — no author data to check, no overlap, or more
	// than one candidate — is deliberately NOT guessed: a wrong guess here
	// would silently attach an edition to the wrong book (or skip creating
	// a book that should exist), which is worse than the duplicate-book
	// status quo this is meant to fix. Those rows come back as
	// ImportItemNeedsReview with the candidates a human can resolve
	// against via ResolveItem, instead of falling through to either
	// outcome automatically.
	candidates, candErr := w.books.FindCandidatesByNormalizedTitleInLibrary(ctx, job.LibraryID, imports.NormalizeTitle(title))
	if candErr != nil {
		return models.ImportItemFailed, fmt.Sprintf("checking title match: %v", candErr), nil, false, nil
	}

	var attachBookID *uuid.UUID
	switch {
	case len(candidates) == 1 && authorsOverlap(rowAuthorNames(row), candidates[0].Authors):
		id := candidates[0].BookID
		attachBookID = &id
	case len(candidates) > 0:
		msg := fmt.Sprintf("title matches %d existing book(s) but the match couldn't be confirmed by author — needs review", len(candidates))
		return models.ImportItemNeedsReview, msg, nil, false, candidates
	}
	attachToExisting := attachBookID != nil

	bookID, editionID, err := w.createOrAttachEdition(ctx, job, row, tagCache, allGenres, attachBookID)
	if err != nil {
		return models.ImportItemFailed, err.Error(), nil, false, nil
	}

	// User-interaction fields are applied after the book/edition is
	// committed so that a per-user `user_book_interactions` row points
	// at a real `book_edition_id`. Failures here are non-fatal — the
	// book is already imported, so we log and move on rather than
	// rolling back the whole row.
	w.applyInteraction(ctx, editionID, interactionUserID, row)

	// addedToLibrary=true either way: either a fresh book + edition was
	// created, or a new edition was attached to an existing book — both
	// cases add a new edition row that's missing metadata/cover, so both
	// get queued for post-import enrichment.
	if attachToExisting {
		return models.ImportItemDone, fmt.Sprintf("added new edition of existing book %q", title), &bookID, true, nil
	}
	return models.ImportItemDone, fmt.Sprintf("imported %q", title), &bookID, true, nil
}

// createOrAttachEdition creates a new book+edition from row, or — when
// attachBookID is non-nil — attaches row as a new (non-primary) edition of
// that existing book, leaving the book's own title/subtitle/description/
// contributors/tags/genres untouched (already established by whatever
// created the book originally). Shared by processItem's two non-ambiguous
// outcomes and ResolveItem's human-driven resolution of a needs_review
// item, so the two code paths can't drift apart on how a book/edition
// actually gets created.
func (w *ImportWorker) createOrAttachEdition(
	ctx context.Context,
	job *models.ImportJob,
	row map[string]string,
	tagCache map[string]uuid.UUID,
	allGenres []*models.Genre,
	attachBookID *uuid.UUID,
) (bookID, editionID uuid.UUID, err error) {
	opts := job.Options
	attachToExisting := attachBookID != nil

	// CSV values are used directly; provider enrichment happens asynchronously
	// via MetadataEnrichmentJob when opts.EnrichMetadata is true.
	title := strings.TrimSpace(row["title"])
	finalSubtitle := row["subtitle"]
	finalDescription := row["description"]
	finalPublisher := row["publisher"]
	finalLanguage := row["language"]
	finalISBN10 := strings.TrimSpace(row["isbn_10"])
	finalISBN13 := strings.TrimSpace(row["isbn_13"])

	var publishDate *time.Time
	if ds := strings.TrimSpace(row["publish_date"]); ds != "" {
		for _, layout := range []string{"2006-01-02", "2006-01", "2006", "January 2, 2006", "Jan 2, 2006"} {
			if t, perr := time.Parse(layout, ds); perr == nil {
				publishDate = &t
				break
			}
		}
	}

	// ── Media type ────────────────────────────────────────────────────────────
	mediaTypes, mtErr := w.books.ListMediaTypes(ctx)
	if mtErr != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("loading media types: %w", mtErr)
	}
	mediaTypeID := findMediaTypeID(mediaTypes, row["media_type"])
	if mediaTypeID == uuid.Nil {
		mediaTypeID = inferMediaType(mediaTypes, row["tags"])
	}
	if mediaTypeID == uuid.Nil {
		for _, mt := range mediaTypes {
			if mt.Name == "novel" {
				mediaTypeID = mt.ID
				break
			}
		}
	}

	// ── Contributors ──────────────────────────────────────────────────────────
	var contribs []repository.ContributorInput
	if authorStr := strings.TrimSpace(row["author"]); authorStr != "" {
		for i, rawName := range splitAuthors(authorStr) {
			name, role := parseContributorNameRole(rawName)
			c, cErr := w.findOrCreateContributor(ctx, name)
			if cErr != nil {
				slog.Warn("contributor find/create failed", "name", name, "error", cErr)
				continue
			}
			contribs = append(contribs, repository.ContributorInput{
				ContributorID: c.ID,
				Role:          role,
				DisplayOrder:  i,
			})
		}
	}

	// ── Tags ──────────────────────────────────────────────────────────────────
	var tagIDs []uuid.UUID
	if tagStr := row["tags"]; tagStr != "" {
		for _, rawName := range strings.Split(tagStr, ",") {
			name := strings.TrimSpace(rawName)
			if name == "" {
				continue
			}
			id, tErr := w.resolveTag(ctx, job.LibraryID, job.CreatedBy, name, tagCache)
			if tErr != nil {
				slog.Warn("resolving tag", "name", name, "error", tErr)
				continue
			}
			tagIDs = append(tagIDs, id)
		}
	}

	// ── Genres (from CSV tags only; provider enrichment adds more if enabled) ─
	var genreIDs []uuid.UUID
	if tagStr := row["tags"]; tagStr != "" {
		var csvTagParts []string
		for _, t := range strings.Split(tagStr, ",") {
			if p := strings.TrimSpace(t); p != "" {
				csvTagParts = append(csvTagParts, p)
			}
		}
		if len(csvTagParts) > 0 {
			genreIDs = normalizeCategories(csvTagParts, allGenres)
		}
	}

	// ── Page count ────────────────────────────────────────────────────────────
	var pageCount *int
	if pc := strings.TrimSpace(row["page_count"]); pc != "" {
		var n int
		if _, scanErr := fmt.Sscanf(pc, "%d", &n); scanErr == nil && n > 0 {
			pageCount = &n
		}
	}

	// ── Create book in transaction (or attach a new edition to an existing
	//    book) ──────────────────────────────────────────────────────────────
	tx, txErr := w.pool.Begin(ctx)
	if txErr != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("begin tx: %w", txErr)
	}
	defer tx.Rollback(ctx)

	if attachToExisting {
		bookID = *attachBookID
		// title, subtitle, description, contributors, tags, and genres are
		// already established on the matched book and are deliberately
		// left untouched — this row only contributes a new edition below
		// (plus this user's copy/reading data for it).
	} else {
		bookID = uuid.New()
		if cErr := w.books.Create(ctx, tx, bookID,
			title, finalSubtitle, mediaTypeID,
			finalDescription,
		); cErr != nil {
			return uuid.Nil, uuid.Nil, fmt.Errorf("creating book: %w", cErr)
		}

		if aErr := w.libraryBooks.AddBookToLibrary(ctx, tx, job.LibraryID, bookID, &job.CreatedBy); aErr != nil {
			return uuid.Nil, uuid.Nil, fmt.Errorf("adding book to library: %w", aErr)
		}

		if len(contribs) > 0 {
			if sErr := w.books.SetContributors(ctx, tx, bookID, contribs); sErr != nil {
				return uuid.Nil, uuid.Nil, fmt.Errorf("setting contributors: %w", sErr)
			}
		}

		if len(tagIDs) > 0 {
			if sErr := w.tags.SetBookTags(ctx, tx, bookID, tagIDs); sErr != nil {
				return uuid.Nil, uuid.Nil, fmt.Errorf("setting tags: %w", sErr)
			}
		}

		if len(genreIDs) > 0 {
			if sErr := w.genres.SetBookGenres(ctx, tx, bookID, genreIDs); sErr != nil {
				return uuid.Nil, uuid.Nil, fmt.Errorf("setting genres: %w", sErr)
			}
		}
	}

	format := models.NormalizeEditionFormat(opts.DefaultFormat)
	editionLang := finalLanguage
	if editionLang == "" {
		editionLang = "en"
	}
	// ── Acquired date ─────────────────────────────────────────────────────────
	var acquiredAt *time.Time
	if ds := strings.TrimSpace(row["acquired_date"]); ds != "" {
		for _, layout := range []string{"2006-01-02", "2006-01", "2006", "January 2, 2006", "Jan 2, 2006"} {
			if t, perr := time.Parse(layout, ds); perr == nil {
				acquiredAt = &t
				break
			}
		}
	}

	// isPrimary is false when attaching to an existing book — the book's
	// established edition stays primary; this new one is additional
	// (a different printing, format, or the ISBN-less/mismatched row that
	// triggered the title-fallback match above), not a replacement.
	editionID = uuid.New()
	if eErr := w.editions.Create(ctx, tx, editionID, bookID,
		format, editionLang, "", "", finalPublisher,
		publishDate, finalISBN10, finalISBN13, finalDescription,
		nil, pageCount, !attachToExisting, nil,
	); eErr != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("creating edition: %w", eErr)
	}
	// Record this library's copy of the new edition.
	var acq *any
	if acquiredAt != nil {
		v := any(*acquiredAt)
		acq = &v
	}
	if sErr := w.libraryBooks.SetEditionCopyCount(ctx, tx, job.LibraryID, editionID, 1, acq); sErr != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("setting library copy count: %w", sErr)
	}

	if cErr := tx.Commit(ctx); cErr != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("commit: %w", cErr)
	}

	return bookID, editionID, nil
}

// ResolveItem applies a human decision to a needs_review item — one whose
// title-fallback match couldn't be auto-resolved by processItem. action is
// "attach" (bookID, required, must be one of the item's stored candidates —
// add this row as a new edition of that existing book) or "create" (import
// the row as a brand-new book, exactly as processItem does when no
// candidate exists at all). Runs synchronously — a single row's writes are
// small enough that routing through River for one item would only add
// latency, not buy anything.
func (w *ImportWorker) ResolveItem(ctx context.Context, jobID, itemID uuid.UUID, action string, bookID *uuid.UUID) (*models.ImportJobItem, error) {
	item, err := w.importJobs.GetItem(ctx, itemID)
	if err != nil {
		return nil, err
	}
	if item.ImportJobID != jobID {
		return nil, repository.ErrNotFound
	}
	if item.Status != models.ImportItemNeedsReview {
		return nil, fmt.Errorf("item is not awaiting review (status %q)", item.Status)
	}

	job, err := w.importJobs.GetJob(ctx, jobID)
	if err != nil {
		return nil, fmt.Errorf("loading import job: %w", err)
	}

	var attachBookID *uuid.UUID
	switch action {
	case "attach":
		if bookID == nil {
			return nil, fmt.Errorf("book_id is required for the attach action")
		}
		valid := false
		for _, c := range item.Candidates {
			if c.BookID == *bookID {
				valid = true
				break
			}
		}
		if !valid {
			return nil, fmt.Errorf("book_id is not one of this item's candidates")
		}
		attachBookID = bookID
	case "create":
		// attachBookID stays nil — createOrAttachEdition creates a new book.
	default:
		return nil, fmt.Errorf("unknown action %q (must be \"attach\" or \"create\")", action)
	}

	allGenres, err := w.genres.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading genres: %w", err)
	}
	tagCache := make(map[string]uuid.UUID)

	resolvedBookID, editionID, err := w.createOrAttachEdition(ctx, job, item.RawData, tagCache, allGenres, attachBookID)
	if err != nil {
		return nil, err
	}

	interactionUserID := job.CreatedBy
	if job.Options.AttributeToUserID != nil {
		interactionUserID = *job.Options.AttributeToUserID
	}
	w.applyInteraction(ctx, editionID, interactionUserID, item.RawData)

	var resultMsg string
	if attachBookID != nil {
		resultMsg = fmt.Sprintf("resolved: added new edition of existing book %q", item.Title)
	} else {
		resultMsg = fmt.Sprintf("resolved: imported %q as a new book", item.Title)
	}
	if err := w.importJobs.UpdateItemStatus(ctx, itemID, models.ImportItemDone, resultMsg, &resolvedBookID); err != nil {
		return nil, fmt.Errorf("updating item status: %w", err)
	}

	// The job already finished — needs_review rows don't block completion
	// (see Work) — so this just reflects the row's resolution in the job's
	// counters without touching its status.
	newProcessed := job.ProcessedRows + 1
	newNeedsReview := job.NeedsReviewRows - 1
	if newNeedsReview < 0 {
		newNeedsReview = 0
	}
	if err := w.importJobs.UpdateJobStatus(ctx, jobID, job.Status, newProcessed, job.FailedRows, job.SkippedRows, newNeedsReview); err != nil {
		return nil, fmt.Errorf("updating job counters: %w", err)
	}

	if w.riverClient != nil && w.batches != nil && item.Title != "" {
		booksToEnrich := []importedBook{{id: resolvedBookID, title: item.Title}}
		if job.Options.EnrichMetadata {
			w.spawnEnrichmentBatch(ctx, job, booksToEnrich, models.EnrichmentBatchTypeMetadata)
		}
		if job.Options.EnrichCovers {
			w.spawnEnrichmentBatch(ctx, job, booksToEnrich, models.EnrichmentBatchTypeCover)
		}
	}

	return w.importJobs.GetItem(ctx, itemID)
}

// rowAuthorNames extracts plain contributor names (role annotations like
// "(Illustrator)" stripped) from an import row's author field, for
// comparing against an existing book's contributors during title-fallback
// matching.
func rowAuthorNames(row map[string]string) []string {
	authorStr := strings.TrimSpace(row["author"])
	if authorStr == "" {
		return nil
	}
	names := make([]string, 0, 4)
	for _, raw := range splitAuthors(authorStr) {
		name, _ := parseContributorNameRole(raw)
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

// authorsOverlap reports whether any name in a appears (case-insensitively)
// in b — the tiebreak used to decide whether a title-fallback match is
// safe to auto-attach. Same heuristic already validated live in the
// Goodreads-sync pipelines' own author-based disambiguation.
func authorsOverlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if strings.EqualFold(strings.TrimSpace(x), strings.TrimSpace(y)) {
				return true
			}
		}
	}
	return false
}

// applyInteraction reads the user-interaction columns out of an import
// row and upserts a `user_book_interactions` record for the importing
// user against the given edition. Idempotent — re-running the same
// import on a row whose values haven't changed produces no-op writes.
//
// Skips the upsert entirely when none of the interaction fields are
// present. We don't want to clobber an existing rating/review just
// because the user re-ran an import that didn't carry user-data.
func (w *ImportWorker) applyInteraction(ctx context.Context, editionID, userID uuid.UUID, row map[string]string) {
	readStatus := imports.ReadStatus(row["read_status"])
	rating, hasRating := imports.Rating(row["rating"])
	review := strings.TrimSpace(row["review"])
	notes := strings.TrimSpace(row["notes"])
	startedAt, hasStarted := imports.Date(row["date_started"])
	finishedAt, hasFinished := imports.Date(row["date_finished"])
	isFavorite, hasFavorite := imports.Bool(row["is_favorite"])

	// Bail when nothing interaction-shaped is present — most generic
	// CSVs won't carry any of these and we don't want to touch the row.
	if readStatus == "" && !hasRating && review == "" && notes == "" &&
		!hasStarted && !hasFinished && !hasFavorite {
		return
	}

	// If we have a finish date but no explicit status, infer "read".
	// Mirrors the behaviour every external tracker assumes — if you
	// finished a book on a date, you read it.
	if readStatus == "" && hasFinished {
		readStatus = "read"
	}

	// MergeInteraction preserves whatever the existing row holds for
	// any field the CSV didn't populate. The previous Upsert variant
	// did an unconditional overwrite, which silently wiped manually
	// entered ratings/reviews on every re-import.
	var readStatusArg *string
	if readStatus != "" {
		readStatusArg = &readStatus
	}
	var ratingArg any
	if hasRating {
		ratingArg = rating
	}
	var notesArg *string
	if notes != "" {
		notesArg = &notes
	}
	var reviewArg *string
	if review != "" {
		reviewArg = &review
	}
	var startedArg any
	if hasStarted {
		startedArg = startedAt
	}
	var finishedArg any
	if hasFinished {
		finishedArg = finishedAt
	}
	var favoriteArg *bool
	if hasFavorite {
		favoriteArg = &isFavorite
	}

	if _, err := w.editions.MergeInteraction(
		ctx, userID, editionID,
		readStatusArg, ratingArg,
		notesArg, reviewArg,
		startedArg, finishedArg,
		favoriteArg, nil, // progress not imported from CSV
	); err != nil {
		slog.Warn("import: merging user interaction failed",
			"user_id", userID, "edition_id", editionID, "error", err)
	}
}

func (w *ImportWorker) findOrCreateContributor(ctx context.Context, name string) (*models.Contributor, error) {
	results, err := w.contributors.Search(ctx, name, 5)
	if err != nil {
		return nil, err
	}
	for _, c := range results {
		if strings.EqualFold(c.Name, name) {
			return c, nil
		}
	}
	return w.contributors.Create(ctx, uuid.New(), name, service.DeriveSortName(name), false)
}

func (w *ImportWorker) resolveTag(ctx context.Context, libraryID, createdBy uuid.UUID, name string, cache map[string]uuid.UUID) (uuid.UUID, error) {
	key := strings.ToLower(name)
	if id, ok := cache[key]; ok {
		return id, nil
	}
	// Try to create; on conflict, list and find
	tag, err := w.tags.Create(ctx, uuid.New(), libraryID, name, "", createdBy)
	if err != nil {
		all, listErr := w.tags.List(ctx, libraryID)
		if listErr != nil {
			return uuid.Nil, fmt.Errorf("listing tags: %w", listErr)
		}
		for _, t := range all {
			if strings.EqualFold(t.Name, name) {
				cache[key] = t.ID
				return t.ID, nil
			}
		}
		return uuid.Nil, fmt.Errorf("creating tag %q: %w", name, err)
	}
	cache[key] = tag.ID
	return tag.ID, nil
}

// ─── Genre normalization ──────────────────────────────────────────────────────

// normalizeCategories maps provider category strings against the known genres.
// Splits on "/" and ",", skips strings with ">" or ":", caps at 4.
func normalizeCategories(cats []string, allGenres []*models.Genre) []uuid.UUID {
	byName := make(map[string]*models.Genre, len(allGenres))
	for _, g := range allGenres {
		byName[strings.ToLower(g.Name)] = g
	}

	seen := make(map[uuid.UUID]bool)
	var matched []*models.Genre

	for _, cat := range cats {
		for _, part := range strings.FieldsFunc(cat, func(r rune) bool { return r == '/' || r == ',' }) {
			part = strings.TrimSpace(part)
			if part == "" || strings.Contains(part, ">") || strings.Contains(part, ":") {
				continue
			}
			if g, ok := byName[strings.ToLower(part)]; ok && !seen[g.ID] {
				seen[g.ID] = true
				matched = append(matched, g)
			}
		}
	}

	// Sort by name length ascending (shorter = more general/cleaner)
	for i := 1; i < len(matched); i++ {
		for j := i; j > 0 && len(matched[j].Name) < len(matched[j-1].Name); j-- {
			matched[j], matched[j-1] = matched[j-1], matched[j]
		}
	}
	const maxGenres = 4
	if len(matched) > maxGenres {
		matched = matched[:maxGenres]
	}

	ids := make([]uuid.UUID, len(matched))
	for i, g := range matched {
		ids[i] = g.ID
	}
	return ids
}

// ─── Small helpers ────────────────────────────────────────────────────────────

// knownContributorRoles is the set of valid role strings that may appear in
// parentheses after a contributor name (e.g. "Jane Smith (Illustrator)").
// Must stay in sync with CONTRIBUTOR_ROLES in web/src/components/ContributorRow.tsx.
var knownContributorRoles = map[string]struct{}{
	"author": {}, "artist": {}, "illustrator": {}, "writer": {}, "penciller": {}, "inker": {},
	"colorist": {}, "letterer": {}, "translator": {}, "editor": {}, "narrator": {},
}

// parseContributorNameRole splits "Name (Role)" into ("Name", "role") when the
// parenthetical matches a known role. Otherwise it returns the full string and "author".
func parseContributorNameRole(raw string) (name, role string) {
	raw = strings.TrimSpace(raw)
	// Match trailing "(...)" — must be the last thing in the string.
	open := strings.LastIndex(raw, "(")
	if open > 0 && raw[len(raw)-1] == ')' {
		candidate := strings.ToLower(strings.TrimSpace(raw[open+1 : len(raw)-1]))
		if _, ok := knownContributorRoles[candidate]; ok {
			return strings.TrimSpace(raw[:open]), candidate
		}
	}
	return raw, "author"
}

func splitAuthors(s string) []string {
	var names []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			names = append(names, p)
		}
	}
	return names
}

func findMediaTypeID(types []*models.MediaType, name string) uuid.UUID {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return uuid.Nil // caller handles empty case
	}
	for _, mt := range types {
		if strings.ToLower(mt.DisplayName) == lower || strings.ToLower(mt.Name) == lower {
			return mt.ID
		}
	}
	for _, mt := range types {
		if strings.Contains(strings.ToLower(mt.DisplayName), lower) || strings.Contains(strings.ToLower(mt.Name), lower) {
			return mt.ID
		}
	}
	return uuid.Nil
}

// inferMediaType checks each comma-separated tag against media type names/display-names
// for an exact match — used when no explicit media_type is provided in the CSV.
func inferMediaType(types []*models.MediaType, tags string) uuid.UUID {
	if tags == "" {
		return uuid.Nil
	}
	for _, rawTag := range strings.Split(tags, ",") {
		tag := strings.TrimSpace(strings.ToLower(rawTag))
		if tag == "" {
			continue
		}
		for _, mt := range types {
			if tag == strings.ToLower(mt.Name) || tag == strings.ToLower(mt.DisplayName) {
				return mt.ID
			}
		}
	}
	return uuid.Nil
}
