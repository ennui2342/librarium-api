// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package service

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fireball1725/librarium-api/internal/models"
	"github.com/fireball1725/librarium-api/internal/repository"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrUpstreamHTTP is returned when a remote URL responds with a non-2xx status.
type ErrUpstreamHTTP struct {
	StatusCode int
}

func (e ErrUpstreamHTTP) Error() string {
	return fmt.Sprintf("cover URL returned %d", e.StatusCode)
}

type BookService struct {
	pool         *pgxpool.Pool
	books        *repository.BookRepo
	libraryBooks *repository.LibraryBookRepo
	contributors *repository.ContributorRepo
	editions     *repository.EditionRepo
	tags         *repository.TagRepo
	genres       *repository.GenreRepo
	covers       *repository.CoverRepo
	suggestions  *repository.AISuggestionsRepo
	coverPath    string
}

func NewBookService(pool *pgxpool.Pool, books *repository.BookRepo, libraryBooks *repository.LibraryBookRepo, contributors *repository.ContributorRepo, editions *repository.EditionRepo, tags *repository.TagRepo, genres *repository.GenreRepo, covers *repository.CoverRepo, suggestions *repository.AISuggestionsRepo, coverPath string) *BookService {
	return &BookService{pool: pool, books: books, libraryBooks: libraryBooks, contributors: contributors, editions: editions, tags: tags, genres: genres, covers: covers, suggestions: suggestions, coverPath: coverPath}
}

// ─── Media types ──────────────────────────────────────────────────────────────

func (s *BookService) ListMediaTypes(ctx context.Context) ([]*models.MediaType, error) {
	return s.books.ListMediaTypes(ctx)
}

// ─── Contributors ─────────────────────────────────────────────────────────────

func (s *BookService) SearchContributors(ctx context.Context, query string) ([]*models.Contributor, error) {
	return s.contributors.Search(ctx, query, 10)
}

func (s *BookService) CreateContributor(ctx context.Context, name string) (*models.Contributor, error) {
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	return s.contributors.Create(ctx, uuid.New(), name, DeriveSortName(name), false)
}

// ─── Books ────────────────────────────────────────────────────────────────────

type BookRequest struct {
	Title        string
	Subtitle     string
	MediaTypeID  uuid.UUID
	Description  string
	Contributors []repository.ContributorInput
	TagIDs       []uuid.UUID
	GenreIDs     []uuid.UUID
	// Edition, if set, is created atomically with the book.
	Edition *EditionRequest
}

func (s *BookService) CreateBook(ctx context.Context, libraryID, callerID uuid.UUID, req BookRequest) (*models.Book, error) {
	// If an ISBN is provided and already exists globally (in any library or
	// floating), we reuse the existing book + edition and bump the copy count
	// for *this* library via the junction. If this library didn't already
	// hold the book, the library_books junction row gets added too.
	if req.Edition != nil {
		isbn := req.Edition.ISBN13
		if isbn == "" {
			isbn = req.Edition.ISBN10
		}
		if isbn != "" {
			if existing, err := s.editions.FindByISBN(ctx, isbn); err == nil && existing != nil {
				tx, txErr := s.pool.Begin(ctx)
				if txErr != nil {
					return nil, fmt.Errorf("beginning transaction: %w", txErr)
				}
				defer tx.Rollback(ctx)
				if err := s.libraryBooks.AddBookToLibrary(ctx, tx, libraryID, existing.BookID, &callerID); err != nil {
					return nil, err
				}
				if err := tx.Commit(ctx); err != nil {
					return nil, fmt.Errorf("committing transaction: %w", err)
				}
				if incrErr := s.editions.IncrementCopyCount(ctx, libraryID, existing.ID); incrErr != nil {
					return nil, fmt.Errorf("incrementing copy count: %w", incrErr)
				}
				return s.books.FindByID(ctx, existing.BookID, callerID, libraryID)
			}
		}
	}

	bookID := uuid.New()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := s.books.Create(ctx, tx, bookID,
		req.Title, req.Subtitle, req.MediaTypeID,
		req.Description,
	); err != nil {
		return nil, err
	}

	if err := s.libraryBooks.AddBookToLibrary(ctx, tx, libraryID, bookID, &callerID); err != nil {
		return nil, err
	}

	if err := s.books.SetContributors(ctx, tx, bookID, req.Contributors); err != nil {
		return nil, err
	}

	if err := s.books.SetBookTags(ctx, tx, bookID, req.TagIDs); err != nil {
		return nil, err
	}

	if len(req.GenreIDs) > 0 {
		if err := s.genres.SetBookGenres(ctx, tx, bookID, req.GenreIDs); err != nil {
			return nil, err
		}
	}

	if req.Edition != nil {
		e := req.Edition
		editionID := uuid.New()
		if err := s.editions.Create(ctx, tx, editionID, bookID,
			e.Format, e.Language, e.EditionName, e.Narrator, e.Publisher,
			e.PublishDate, e.ISBN10, e.ISBN13, e.Description,
			e.DurationSeconds, e.PageCount, e.IsPrimary,
			e.NarratorContributorID,
		); err != nil {
			return nil, err
		}
		// Record that this library holds 1 copy of this new edition.
		var acq *any
		if e.AcquiredAt != nil {
			v := any(*e.AcquiredAt)
			acq = &v
		}
		if err := s.libraryBooks.SetEditionCopyCount(ctx, tx, libraryID, editionID, 1, acq); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing transaction: %w", err)
	}

	return s.books.FindByID(ctx, bookID, callerID, libraryID)
}

// GetBook returns one book with caller-scoped fields hydrated. callerID
// and libraryID are passed through to the repo so user_read_status /
// user_rating / user_progress_pct + per-library active_loan_count match
// what the list endpoint emits. Pass uuid.Nil for either param when the
// caller is purely internal.
func (s *BookService) GetBook(ctx context.Context, id uuid.UUID, callerID uuid.UUID, libraryID uuid.UUID) (*models.Book, error) {
	b, err := s.books.FindByID(ctx, id, callerID, libraryID)
	if err != nil {
		return nil, err
	}
	libs, err := s.libraryBooks.LibrariesForBook(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("loading libraries for book: %w", err)
	}
	b.Libraries = libs
	return b, nil
}

// hydrateLibraries loads the library membership list for each of the given
// books. Used by list paths so client UI can render "in libraries X, Y".
func (s *BookService) hydrateLibraries(ctx context.Context, books []*models.Book) error {
	for _, b := range books {
		libs, err := s.libraryBooks.LibrariesForBook(ctx, b.ID)
		if err != nil {
			return fmt.Errorf("loading libraries for book %s: %w", b.ID, err)
		}
		b.Libraries = libs
	}
	return nil
}

func (s *BookService) FindBookByISBN(ctx context.Context, libraryID uuid.UUID, isbn string) (*models.Book, error) {
	edition, err := s.editions.FindByISBNInLibrary(ctx, libraryID, isbn)
	if err != nil {
		return nil, err
	}
	return s.books.FindByID(ctx, edition.BookID, uuid.Nil, libraryID)
}

func (s *BookService) ListBooks(ctx context.Context, libraryID uuid.UUID, opts repository.ListBooksOpts) ([]*models.Book, int, error) {
	books, total, err := s.books.List(ctx, libraryID, opts)
	if err != nil {
		return nil, 0, err
	}
	if err := s.hydrateLibraries(ctx, books); err != nil {
		return nil, 0, err
	}
	return books, total, nil
}

func (s *BookService) ListBookLetters(ctx context.Context, libraryID uuid.UUID) ([]string, error) {
	return s.books.ListBookLetters(ctx, libraryID)
}

func (s *BookService) BookFingerprint(ctx context.Context, libraryID uuid.UUID) (repository.BookFingerprint, error) {
	return s.books.Fingerprint(ctx, libraryID)
}

func (s *BookService) UpdateBook(ctx context.Context, id uuid.UUID, req BookRequest) (*models.Book, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := s.books.Update(ctx, tx, id,
		req.Title, req.Subtitle, req.MediaTypeID,
		req.Description,
	); err != nil {
		return nil, err
	}

	if err := s.books.SetContributors(ctx, tx, id, req.Contributors); err != nil {
		return nil, err
	}

	if err := s.books.SetBookTags(ctx, tx, id, req.TagIDs); err != nil {
		return nil, err
	}

	if err := s.genres.SetBookGenres(ctx, tx, id, req.GenreIDs); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing transaction: %w", err)
	}

	return s.books.FindByID(ctx, id, uuid.Nil, uuid.Nil)
}

// DeleteBook permanently deletes a book. Cascades through library_books,
// book_editions, user_book_interactions, loans, ai_suggestions, and every
// other FK that references the book. Admin-only at the handler layer.
func (s *BookService) DeleteBook(ctx context.Context, id uuid.UUID) error {
	return s.books.Delete(ctx, id)
}

// AddBookToLibrary attaches an existing book to a library via the
// library_books junction. Idempotent — re-adding a book already in the
// library is a no-op.
//
// Remove-on-action: when the caller just added a book, any `buy`
// suggestion they had pointing at that book has served its purpose and
// gets deleted. Scoped to addedBy when present — otherwise we can't
// attribute the action to a user, and we leave suggestions alone.
func (s *BookService) AddBookToLibrary(ctx context.Context, libraryID, bookID uuid.UUID, addedBy *uuid.UUID) error {
	if err := s.libraryBooks.AddBookToLibrary(ctx, nil, libraryID, bookID, addedBy); err != nil {
		return err
	}
	if addedBy != nil && s.suggestions != nil {
		if _, derr := s.suggestions.DeleteForActionTaken(ctx, *addedBy, bookID, "buy"); derr != nil {
			slog.Warn("deleting buy suggestions after add-to-library",
				"user_id", *addedBy, "book_id", bookID, "error", derr)
		}
	}
	return nil
}

// RemoveBookFromLibrary drops the library_books junction row for this
// (library, book). The book row itself stays.
func (s *BookService) RemoveBookFromLibrary(ctx context.Context, libraryID, bookID uuid.UUID) error {
	return s.libraryBooks.RemoveBookFromLibrary(ctx, libraryID, bookID)
}

// ─── Editions ─────────────────────────────────────────────────────────────────

type EditionRequest struct {
	Format                string
	Language              string
	EditionName           string
	Narrator              string
	Publisher             string
	PublishDate           *time.Time
	ISBN10                string
	ISBN13                string
	Description           string
	DurationSeconds       *int
	PageCount             *int
	CopyCount             int
	IsPrimary             bool
	AcquiredAt            *time.Time
	NarratorContributorID *uuid.UUID
}

func (s *BookService) ListEditions(ctx context.Context, bookID uuid.UUID) ([]*models.BookEdition, error) {
	return s.editions.ListByBook(ctx, bookID)
}

func (s *BookService) GetEdition(ctx context.Context, id uuid.UUID) (*models.BookEdition, error) {
	return s.editions.FindByID(ctx, id)
}

func (s *BookService) CreateEdition(ctx context.Context, bookID uuid.UUID, req EditionRequest) (*models.BookEdition, error) {
	editionID := uuid.New()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := s.editions.Create(ctx, tx, editionID, bookID,
		req.Format, req.Language, req.EditionName, req.Narrator, req.Publisher,
		req.PublishDate, req.ISBN10, req.ISBN13, req.Description,
		req.DurationSeconds, req.PageCount, req.IsPrimary,
		req.NarratorContributorID,
	); err != nil {
		return nil, err
	}

	if req.NarratorContributorID != nil {
		if err := s.books.EnsureBookContributor(ctx, tx, bookID, *req.NarratorContributorID, "narrator"); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing transaction: %w", err)
	}

	return s.editions.FindByID(ctx, editionID)
}

func (s *BookService) UpdateEdition(ctx context.Context, id uuid.UUID, req EditionRequest) (*models.BookEdition, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := s.editions.Update(ctx, tx, id,
		req.Format, req.Language, req.EditionName, req.Narrator, req.Publisher,
		req.PublishDate, req.ISBN10, req.ISBN13, req.Description,
		req.DurationSeconds, req.PageCount, req.IsPrimary,
		req.NarratorContributorID,
	); err != nil {
		return nil, err
	}
	// CopyCount and AcquiredAt are per-library now and not managed on this
	// generic Update path; use library-scoped endpoints for those.

	if req.NarratorContributorID != nil {
		// Look up the book_id from the edition to link the contributor.
		edition, err := s.editions.FindByID(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := s.books.EnsureBookContributor(ctx, tx, edition.BookID, *req.NarratorContributorID, "narrator"); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing transaction: %w", err)
	}

	return s.editions.FindByID(ctx, id)
}

func (s *BookService) DeleteEdition(ctx context.Context, id uuid.UUID) error {
	return s.editions.Delete(ctx, id)
}

// ─── User interactions ────────────────────────────────────────────────────────

type InteractionRequest struct {
	ReadStatus   string
	Rating       *int
	Notes        string
	Review       string
	DateStarted  *time.Time
	DateFinished *time.Time
	IsFavorite   bool
	// Progress is the {pages_read?, percent?, position?} JSON body for
	// reading progress. Empty / nil clears the column.
	Progress []byte
}

func (s *BookService) GetInteraction(ctx context.Context, userID, editionID uuid.UUID) (*models.UserBookInteraction, error) {
	return s.editions.GetInteraction(ctx, userID, editionID)
}

// MergeInteractionRequest is the partial-update counterpart to
// InteractionRequest: every field is a pointer (nil string fields use ""
// as their "no value" sentinel, matching MergeInteraction's own NULLIF
// convention), and a nil/empty field preserves whatever the existing
// interaction row already holds instead of overwriting it. Intended for
// automated callers — CSV import, external sync — that only ever have
// partial data for a given update and must not clobber fields a human
// (or another sync) already filled in. See EditionRepo.MergeInteraction's
// own doc comment for why this exists alongside the full-replace
// InteractionRequest/UpsertInteraction pair.
type MergeInteractionRequest struct {
	ReadStatus   string
	Rating       *int
	Notes        string
	Review       string
	DateStarted  *time.Time
	DateFinished *time.Time
	IsFavorite   *bool
	Progress     []byte
}

func (s *BookService) MergeInteraction(ctx context.Context, userID, editionID uuid.UUID, req MergeInteractionRequest) (*models.UserBookInteraction, error) {
	var readStatusArg *string
	if req.ReadStatus != "" {
		readStatusArg = &req.ReadStatus
	}
	var ratingArg any
	if req.Rating != nil {
		ratingArg = *req.Rating
	}
	var notesArg *string
	if req.Notes != "" {
		notesArg = &req.Notes
	}
	var reviewArg *string
	if req.Review != "" {
		reviewArg = &req.Review
	}
	var startedArg any
	if req.DateStarted != nil {
		startedArg = *req.DateStarted
	}
	var finishedArg any
	if req.DateFinished != nil {
		finishedArg = *req.DateFinished
	}

	interaction, err := s.editions.MergeInteraction(ctx, userID, editionID,
		readStatusArg, ratingArg, notesArg, reviewArg,
		startedArg, finishedArg, req.IsFavorite, req.Progress,
	)
	if err != nil {
		return nil, err
	}
	if req.ReadStatus == "read" && s.suggestions != nil {
		if edition, eerr := s.editions.FindByID(ctx, editionID); eerr == nil && edition != nil {
			if _, derr := s.suggestions.DeleteForActionTaken(ctx, userID, edition.BookID, "read_next"); derr != nil {
				slog.Warn("deleting read_next suggestions after read",
					"user_id", userID, "book_id", edition.BookID, "error", derr)
			}
		}
	}
	return interaction, nil
}

func (s *BookService) UpsertInteraction(ctx context.Context, userID, editionID uuid.UUID, req InteractionRequest) (*models.UserBookInteraction, error) {
	interaction, err := s.editions.UpsertInteraction(ctx, userID, editionID,
		req.ReadStatus, req.Rating, req.Notes, req.Review,
		req.DateStarted, req.DateFinished, req.IsFavorite, req.Progress,
	)
	if err != nil {
		return nil, err
	}
	// Remove-on-action: if the user just logged a read on this edition,
	// drop any read_next suggestion rows they had pointing at the same book.
	// The edition's book_id is the key — a read of any edition of the book
	// counts as reading the book.
	if req.ReadStatus == "read" && s.suggestions != nil {
		if edition, eerr := s.editions.FindByID(ctx, editionID); eerr == nil && edition != nil {
			if _, derr := s.suggestions.DeleteForActionTaken(ctx, userID, edition.BookID, "read_next"); derr != nil {
				slog.Warn("deleting read_next suggestions after read",
					"user_id", userID, "book_id", edition.BookID, "error", derr)
			}
		}
	}
	return interaction, nil
}

func (s *BookService) DeleteInteraction(ctx context.Context, userID, editionID uuid.UUID) error {
	return s.editions.DeleteInteraction(ctx, userID, editionID)
}

// ─── Covers ───────────────────────────────────────────────────────────────────

// coverDir returns the directory on disk where covers for a book are stored.
func (s *BookService) coverDir(bookID uuid.UUID) string {
	return filepath.Join(s.coverPath, "books", bookID.String())
}

// extForMime returns a file extension for common image mime types.
func extForMime(mime string) string {
	switch strings.ToLower(mime) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".jpg"
	}
}

// GetBookCoverPath returns the absolute file path and mime type of the primary
// cover for a book, or repository.ErrNotFound if none exists.
func (s *BookService) GetBookCoverPath(ctx context.Context, bookID uuid.UUID) (filePath, mimeType string, err error) {
	cover, err := s.covers.FindPrimary(ctx, "book", bookID)
	if err != nil {
		return "", "", err
	}
	return filepath.Join(s.coverDir(bookID), cover.Filename), cover.MimeType, nil
}

// FetchCoverFromURL downloads a cover image from url, stores it on disk,
// and replaces any existing cover for the book.
func (s *BookService) FetchCoverFromURL(ctx context.Context, bookID, callerID uuid.UUID, sourceURL string) error {
	slog.Debug("fetching cover", "url", sourceURL, "book_id", bookID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return fmt.Errorf("building cover request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Librarium/1.0)")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("downloading cover: %w", err)
	}
	defer resp.Body.Close()

	slog.Debug("cover fetch response",
		"url", sourceURL,
		"status", resp.StatusCode,
		"content_type", resp.Header.Get("Content-Type"),
		"content_length", resp.Header.Get("Content-Length"),
	)

	if resp.StatusCode >= 400 {
		slog.Warn("cover fetch failed", "url", sourceURL, "status", resp.StatusCode, "book_id", bookID)
		return ErrUpstreamHTTP{StatusCode: resp.StatusCode}
	}

	mime := resp.Header.Get("Content-Type")
	if idx := strings.Index(mime, ";"); idx >= 0 {
		mime = strings.TrimSpace(mime[:idx])
	}
	if mime == "" || !strings.HasPrefix(mime, "image/") {
		mime = "image/jpeg"
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10 MB cap
	if err != nil {
		return fmt.Errorf("reading cover body: %w", err)
	}

	slog.Debug("cover fetched", "url", sourceURL, "bytes", len(data), "mime", mime, "book_id", bookID)
	return s.storeCover(ctx, bookID, callerID, data, mime, sourceURL)
}

// StoreCoverFromUpload stores raw image bytes as the book's cover.
func (s *BookService) StoreCoverFromUpload(ctx context.Context, bookID, callerID uuid.UUID, data []byte, mimeType string) error {
	return s.storeCover(ctx, bookID, callerID, data, mimeType, "")
}

func (s *BookService) storeCover(ctx context.Context, bookID, callerID uuid.UUID, data []byte, mimeType, sourceURL string) error {
	dir := s.coverDir(bookID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating cover dir: %w", err)
	}

	// Delete existing covers from disk
	existing, err := s.covers.DeleteByEntityID(ctx, "book", bookID)
	if err != nil {
		return fmt.Errorf("removing old cover records: %w", err)
	}
	for _, fn := range existing {
		_ = os.Remove(filepath.Join(dir, fn))
	}

	coverID := uuid.New()
	filename := coverID.String() + extForMime(mimeType)
	filePath := filepath.Join(dir, filename)
	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		return fmt.Errorf("writing cover file: %w", err)
	}

	now := time.Now()
	if err := s.covers.Insert(ctx, &models.CoverImage{
		ID:         coverID,
		EntityType: "book",
		EntityID:   bookID,
		Filename:   filename,
		MimeType:   mimeType,
		FileSize:   int64(len(data)),
		IsPrimary:  true,
		SourceURL:  sourceURL,
		CreatedBy:  &callerID,
		CreatedAt:  now,
	}); err != nil {
		return err
	}
	// Bump books.updated_at so polling clients detect the cover change
	// and the cache-busting URL version in the API response changes.
	return s.books.Touch(ctx, bookID)
}

// DeleteBookCover removes all covers for a book from disk and the database.
func (s *BookService) DeleteBookCover(ctx context.Context, bookID uuid.UUID) error {
	filenames, err := s.covers.DeleteByEntityID(ctx, "book", bookID)
	if err != nil {
		return err
	}
	dir := s.coverDir(bookID)
	for _, fn := range filenames {
		_ = os.Remove(filepath.Join(dir, fn))
	}
	return nil
}
