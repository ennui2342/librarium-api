// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

// Package workers contains River job workers.
package workers

import (
	"context"
	"errors"
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

	if err := w.importJobs.UpdateJobStatus(ctx, jobID, models.ImportJobProcessing, 0, 0, 0); err != nil {
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

	var processed, failed, skipped int
	var newBooks []importedBook // tracks newly created books for post-import enrichment batches
	for _, item := range items {
		// Check for cancellation before each item so the worker stops promptly.
		if current, cerr := w.importJobs.GetJob(ctx, jobID); cerr == nil && current.Status == models.ImportJobCancelled {
			slog.Info("import job cancelled mid-processing", "import_job_id", jobID, "processed", processed)
			return nil
		}

		status, msg, bookID, addedToLibrary := w.processItem(ctx, importJob, &item, tagCache, allGenres)
		_ = w.importJobs.UpdateItemStatus(ctx, item.ID, status, msg, bookID)

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
		}
		_ = w.importJobs.UpdateJobStatus(ctx, jobID, models.ImportJobProcessing, processed, failed, skipped)
	}

	finalStatus := models.ImportJobDone
	if err := w.importJobs.UpdateJobStatus(ctx, jobID, finalStatus, processed, failed, skipped); err != nil {
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

// processItem returns the per-row outcome plus an addedToLibrary flag
// the caller uses to gate post-import enrichment fan-out. The flag is
// true for any row that newly placed a book into the target library
// (true creates AND links of editions from other libraries) — both
// are "added" from the user's perspective. It's false for pure
// in-library duplicates and skipped rows.
func (w *ImportWorker) processItem(
	ctx context.Context,
	job *models.ImportJob,
	item *models.ImportJobItem,
	tagCache map[string]uuid.UUID,
	allGenres []*models.Genre,
) (models.ImportItemStatus, string, *uuid.UUID, bool) {
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
		return models.ImportItemSkipped, "no title", nil, false
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
				return models.ImportItemFailed, fmt.Sprintf("checking library membership: %v", ierr), nil, false
			}
			bookID := existing.BookID

			if !inLibrary {
				// First time this library is seeing the edition — add it
				// and seed the user-interaction fields from the CSV row.
				if addErr := w.libraryBooks.AddBookToLibrary(ctx, nil, job.LibraryID, bookID, &job.CreatedBy); addErr != nil {
					return models.ImportItemFailed, fmt.Sprintf("adding book to library: %v", addErr), nil, false
				}
				w.applyInteraction(ctx, existing.ID, interactionUserID, row)
				// addedToLibrary=true: the book is new to *this* library
				// even though the edition row pre-existed globally. Queue
				// it for enrichment so missing covers/metadata get filled
				// in if the original-library import skipped that step.
				// The metadata and cover workers no-op when the data is
				// already present.
				return models.ImportItemDone, fmt.Sprintf("linked existing edition (ISBN %s) into this library", isbn), &bookID, true
			}

			// True duplicate — book is already in this library. Apply the
			// user's duplicate-handling preferences. Default (both off) is
			// a no-op skip so re-running an import is idempotent.
			actions := make([]string, 0, 2)
			if opts.DuplicateIncrementCopyCount {
				if incrErr := w.editions.IncrementCopyCount(ctx, job.LibraryID, existing.ID); incrErr != nil {
					return models.ImportItemFailed, fmt.Sprintf("increment copy count: %v", incrErr), nil, false
				}
				actions = append(actions, "copy count incremented")
			}
			if opts.DuplicateUpdateFromCSV {
				w.applyInteraction(ctx, existing.ID, interactionUserID, row)
				actions = append(actions, "user fields updated")
			}
			if len(actions) == 0 {
				return models.ImportItemSkipped, fmt.Sprintf("duplicate ISBN %s — skipped", isbn), &bookID, false
			}
			return models.ImportItemDone, fmt.Sprintf("duplicate ISBN %s — %s", isbn, strings.Join(actions, ", ")), &bookID, false
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
	// an existing book in this library with an exact normalized-title match
	// (see imports.NormalizeTitle) and, if found, attach this row as a new
	// edition of that book instead of creating a second Book record for the
	// same work.
	//
	// Deliberately exact-match only, not fuzzy — a wrong fuzzy match would
	// silently attach an edition to an unrelated book, which is worse than
	// the duplicate-book status quo this is meant to fix. Book-level metadata
	// (title, subtitle, description, contributors, tags, genres) is already
	// established for a matched book and is left untouched; only a new
	// edition (and this row's user-interaction data) gets added.
	attachBookID, attachErr := w.books.FindByNormalizedTitleInLibrary(ctx, job.LibraryID, imports.NormalizeTitle(title))
	attachToExisting := attachErr == nil
	if attachErr != nil && !errors.Is(attachErr, repository.ErrNotFound) {
		return models.ImportItemFailed, fmt.Sprintf("checking title match: %v", attachErr), nil, false
	}

	// CSV values are used directly; provider enrichment happens asynchronously
	// via MetadataEnrichmentJob when opts.EnrichMetadata is true.
	finalTitle := title
	finalSubtitle := row["subtitle"]
	finalDescription := row["description"]
	finalPublisher := row["publisher"]
	finalLanguage := row["language"]
	finalISBN10 := strings.TrimSpace(row["isbn_10"])
	finalISBN13 := strings.TrimSpace(row["isbn_13"])

	var publishDate *time.Time
	if ds := strings.TrimSpace(row["publish_date"]); ds != "" {
		for _, layout := range []string{"2006-01-02", "2006-01", "2006", "January 2, 2006", "Jan 2, 2006"} {
			if t, err := time.Parse(layout, ds); err == nil {
				publishDate = &t
				break
			}
		}
	}

	// ── Media type ────────────────────────────────────────────────────────────
	mediaTypes, err := w.books.ListMediaTypes(ctx)
	if err != nil {
		return models.ImportItemFailed, fmt.Sprintf("loading media types: %v", err), nil, false
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
			c, err := w.findOrCreateContributor(ctx, name)
			if err != nil {
				slog.Warn("contributor find/create failed", "name", name, "error", err)
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
			id, err := w.resolveTag(ctx, job.LibraryID, job.CreatedBy, name, tagCache)
			if err != nil {
				slog.Warn("resolving tag", "name", name, "error", err)
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
	//    book found by the title fallback above) ──────────────────────────────
	bookID := attachBookID
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return models.ImportItemFailed, fmt.Sprintf("begin tx: %v", err), nil, false
	}
	defer tx.Rollback(ctx)

	if !attachToExisting {
		bookID = uuid.New()
		if err := w.books.Create(ctx, tx, bookID,
			finalTitle, finalSubtitle, mediaTypeID,
			finalDescription,
		); err != nil {
			return models.ImportItemFailed, fmt.Sprintf("creating book: %v", err), nil, false
		}

		if err := w.libraryBooks.AddBookToLibrary(ctx, tx, job.LibraryID, bookID, &job.CreatedBy); err != nil {
			return models.ImportItemFailed, fmt.Sprintf("adding book to library: %v", err), nil, false
		}

		if len(contribs) > 0 {
			if err := w.books.SetContributors(ctx, tx, bookID, contribs); err != nil {
				return models.ImportItemFailed, fmt.Sprintf("setting contributors: %v", err), nil, false
			}
		}

		if len(tagIDs) > 0 {
			if err := w.tags.SetBookTags(ctx, tx, bookID, tagIDs); err != nil {
				return models.ImportItemFailed, fmt.Sprintf("setting tags: %v", err), nil, false
			}
		}

		if len(genreIDs) > 0 {
			if err := w.genres.SetBookGenres(ctx, tx, bookID, genreIDs); err != nil {
				return models.ImportItemFailed, fmt.Sprintf("setting genres: %v", err), nil, false
			}
		}
	}
	// else: attaching to an existing book — its title, subtitle,
	// description, contributors, tags, and genres are already established
	// and are deliberately left untouched. This row only contributes a new
	// edition below (plus this user's copy/reading data for it).

	format := models.NormalizeEditionFormat(opts.DefaultFormat)
	editionLang := finalLanguage
	if editionLang == "" {
		editionLang = "en"
	}
	// ── Acquired date ─────────────────────────────────────────────────────────
	var acquiredAt *time.Time
	if ds := strings.TrimSpace(row["acquired_date"]); ds != "" {
		for _, layout := range []string{"2006-01-02", "2006-01", "2006", "January 2, 2006", "Jan 2, 2006"} {
			if t, err := time.Parse(layout, ds); err == nil {
				acquiredAt = &t
				break
			}
		}
	}

	// isPrimary is false when attaching to an existing book — the book's
	// established edition stays primary; this new one is additional
	// (a different printing, format, or the ISBN-less/mismatched row that
	// triggered the title-fallback match above), not a replacement.
	editionID := uuid.New()
	if err := w.editions.Create(ctx, tx, editionID, bookID,
		format, editionLang, "", "", finalPublisher,
		publishDate, finalISBN10, finalISBN13, finalDescription,
		nil, pageCount, !attachToExisting, nil,
	); err != nil {
		return models.ImportItemFailed, fmt.Sprintf("creating edition: %v", err), nil, false
	}
	// Record this library's copy of the new edition.
	var acq *any
	if acquiredAt != nil {
		v := any(*acquiredAt)
		acq = &v
	}
	if err := w.libraryBooks.SetEditionCopyCount(ctx, tx, job.LibraryID, editionID, 1, acq); err != nil {
		return models.ImportItemFailed, fmt.Sprintf("setting library copy count: %v", err), nil, false
	}

	if err := tx.Commit(ctx); err != nil {
		return models.ImportItemFailed, fmt.Sprintf("commit: %v", err), nil, false
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
		return models.ImportItemDone, fmt.Sprintf("added new edition of existing book %q", finalTitle), &bookID, true
	}
	return models.ImportItemDone, fmt.Sprintf("imported %q", finalTitle), &bookID, true
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
