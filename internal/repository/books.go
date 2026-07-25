// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/fireball1725/librarium-api/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type BookRepo struct {
	db *pgxpool.Pool
}

func NewBookRepo(db *pgxpool.Pool) *BookRepo {
	return &BookRepo{db: db}
}

// ContributorInput represents a contributor-role pair to attach to a book.
type ContributorInput struct {
	ContributorID uuid.UUID
	Role          string
	DisplayOrder  int
}

// ─── Media types ──────────────────────────────────────────────────────────────

func (r *BookRepo) ListMediaTypes(ctx context.Context) ([]*models.MediaType, error) {
	const q = `SELECT id, name, display_name FROM media_types ORDER BY display_name`
	rows, err := r.db.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("listing media types: %w", err)
	}
	defer rows.Close()

	var out []*models.MediaType
	for rows.Next() {
		var pgID pgtype.UUID
		mt := &models.MediaType{}
		if err := rows.Scan(&pgID, &mt.Name, &mt.DisplayName); err != nil {
			return nil, fmt.Errorf("scanning media type: %w", err)
		}
		mt.ID = uuid.UUID(pgID.Bytes)
		out = append(out, mt)
	}
	return out, rows.Err()
}

// ─── Books ────────────────────────────────────────────────────────────────────

// booksSelect returns the reusable base SELECT for books. It uses correlated
// subqueries for contributors and tags to avoid cross-product row inflation
// when joining both at once. Append WHERE/ORDER BY/LIMIT as needed.
//
// When userStatusArg > 0, the query includes a user_read_status column using
// that positional argument index ($N) for the user ID. When 0, a constant
// empty string is selected. Column count is always the same regardless.
//
// When loanLibraryArg > 0, active_loan_count is scoped to that library_id
// arg (used by ListBooks). When 0, it counts active loans across every
// library the book belongs to (used by FindByID).
func booksSelect(userStatusArg, loanLibraryArg int) string {
	// All three caller-scoped subqueries (status / rating / progress) use
	// the same ORDER BY so they pick the same interaction row, keeping the
	// returned values internally consistent for users who own multiple
	// editions of the same book.
	const interactionPickOrder = `ORDER BY CASE ubi.read_status
		WHEN 'read' THEN 1
		WHEN 'reading' THEN 2
		WHEN 'did_not_finish' THEN 3
		ELSE 4
	END`

	var userReadStatusExpr, userRatingExpr, userProgressExpr string
	if userStatusArg > 0 {
		userReadStatusExpr = fmt.Sprintf(`COALESCE((
		SELECT ubi.read_status
		FROM book_editions be_rs
		JOIN user_book_interactions ubi ON ubi.book_edition_id = be_rs.id
		WHERE be_rs.book_id = b.id AND ubi.user_id = $%d
		%s
		LIMIT 1
	), '') AS user_read_status`, userStatusArg, interactionPickOrder)
		userRatingExpr = fmt.Sprintf(`COALESCE((
		SELECT ubi.rating
		FROM book_editions be_rt
		JOIN user_book_interactions ubi ON ubi.book_edition_id = be_rt.id
		WHERE be_rt.book_id = b.id AND ubi.user_id = $%d
		%s
		LIMIT 1
	), 0) AS user_rating`, userStatusArg, interactionPickOrder)
		userProgressExpr = fmt.Sprintf(`COALESCE((
		SELECT (ubi.progress->>'percent')::numeric
		FROM book_editions be_pg
		JOIN user_book_interactions ubi ON ubi.book_edition_id = be_pg.id
		WHERE be_pg.book_id = b.id AND ubi.user_id = $%d
		%s
		LIMIT 1
	), 0) AS user_progress_pct`, userStatusArg, interactionPickOrder)
	} else {
		userReadStatusExpr = `'' AS user_read_status`
		userRatingExpr = `0 AS user_rating`
		userProgressExpr = `0 AS user_progress_pct`
	}

	var activeLoanExpr string
	if loanLibraryArg > 0 {
		activeLoanExpr = fmt.Sprintf(`(
		SELECT COUNT(*) FROM loans
		WHERE book_id = b.id AND library_id = $%d AND returned_at IS NULL
	) AS active_loan_count`, loanLibraryArg)
	} else {
		activeLoanExpr = `(
		SELECT COUNT(*) FROM loans
		WHERE book_id = b.id AND returned_at IS NULL
	) AS active_loan_count`
	}

	return `
	SELECT
		b.id, b.title, b.subtitle,
		b.media_type_id, mt.display_name,
		b.description, b.created_at, b.updated_at,
		(
			SELECT COALESCE(
				json_agg(
					json_build_object(
						'contributor_id', c.id,
						'name', c.name,
						'role', bc.role,
						'display_order', bc.display_order
					) ORDER BY bc.display_order, c.name
				),
				'[]'::json
			)
			FROM book_contributors bc
			JOIN contributors c ON c.id = bc.contributor_id
			WHERE bc.book_id = b.id
		) AS contributors,
		(
			SELECT COALESCE(
				json_agg(
					json_build_object('id', t.id, 'name', t.name, 'color', t.color)
					ORDER BY t.name
				),
				'[]'::json
			)
			FROM book_tags bt
			JOIN tags t ON t.id = bt.tag_id
			WHERE bt.book_id = b.id
		) AS tags,
		(
			SELECT COALESCE(
				json_agg(
					json_build_object('id', g.id, 'name', g.name, 'created_at', g.created_at)
					ORDER BY g.name
				),
				'[]'::json
			)
			FROM book_genres bg
			JOIN genres g ON g.id = bg.genre_id
			WHERE bg.book_id = b.id
		) AS genres,
		EXISTS(
			SELECT 1 FROM cover_images ci
			WHERE ci.entity_type = 'book' AND ci.entity_id = b.id AND ci.is_primary = true
		) AS has_cover,
		(
			SELECT COALESCE(
				json_agg(
					json_build_object('series_id', s.id, 'series_name', s.name, 'position', bs.position)
					ORDER BY bs.position, s.name
				),
				'[]'::json
			)
			FROM book_series bs
			JOIN series s ON s.id = bs.series_id
			WHERE bs.book_id = b.id
		) AS series,
		(
			SELECT COALESCE(
				json_agg(
					json_build_object('id', sh.id, 'name', sh.name)
					ORDER BY sh.display_order, sh.name
				),
				'[]'::json
			)
			FROM book_shelves bsh
			JOIN shelves sh ON sh.id = bsh.shelf_id
			WHERE bsh.book_id = b.id
		) AS shelves,
		COALESCE((
			SELECT NULLIF(be.publisher, '')
			FROM book_editions be
			WHERE be.book_id = b.id AND be.is_primary = true
			LIMIT 1
		), '') AS publisher,
		(
			SELECT EXTRACT(year FROM be.publish_date)::int
			FROM book_editions be
			WHERE be.book_id = b.id AND be.is_primary = true AND be.publish_date IS NOT NULL
			LIMIT 1
		) AS publish_year,
		COALESCE((
			SELECT NULLIF(be.language, '')
			FROM book_editions be
			WHERE be.book_id = b.id AND be.is_primary = true
			LIMIT 1
		), '') AS language,
		` + userReadStatusExpr + `,
		` + userRatingExpr + `,
		` + userProgressExpr + `,
		` + activeLoanExpr + `
	FROM books b
	JOIN media_types mt ON mt.id = b.media_type_id`
}

// Create inserts a new book (work). Library ownership is a separate concern —
// call AddToLibrary on the LibraryBookRepo after creating to associate the
// book with a library. A book with no library_books rows is a floating book
// (e.g. an un-owned suggestion).
func (r *BookRepo) Create(ctx context.Context, tx pgx.Tx, id uuid.UUID, title, subtitle string, mediaTypeID uuid.UUID, description string) error {
	const q = `
		INSERT INTO books (id, title, subtitle, media_type_id, description)
		VALUES ($1, $2, NULLIF($3,''), $4, NULLIF($5,''))`

	_, err := tx.Exec(ctx, q, id, title, subtitle, mediaTypeID, description)
	if err != nil {
		return fmt.Errorf("inserting book: %w", err)
	}
	return nil
}

func (r *BookRepo) SetContributors(ctx context.Context, tx pgx.Tx, bookID uuid.UUID, contributors []ContributorInput) error {
	if _, err := tx.Exec(ctx, `DELETE FROM book_contributors WHERE book_id = $1`, bookID); err != nil {
		return fmt.Errorf("clearing book contributors: %w", err)
	}

	// Deduplicate by (contributor_id, role) — providers sometimes return the same
	// author name more than once, which resolves to the same contributor ID.
	type contribKey struct {
		id   uuid.UUID
		role string
	}
	seen := make(map[contribKey]struct{}, len(contributors))

	for _, c := range contributors {
		k := contribKey{c.ContributorID, c.Role}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}

		_, err := tx.Exec(ctx,
			`INSERT INTO book_contributors (book_id, contributor_id, role, display_order)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (book_id, contributor_id, role) DO NOTHING`,
			bookID, c.ContributorID, c.Role, c.DisplayOrder,
		)
		if err != nil {
			return fmt.Errorf("inserting book contributor: %w", err)
		}
	}
	return nil
}

// EnsureBookContributor adds a contributor-role pair to a book if it does not
// already exist. Safe to call inside or outside a transaction; pass nil tx to
// use the pool directly.
func (r *BookRepo) EnsureBookContributor(ctx context.Context, tx pgx.Tx, bookID, contributorID uuid.UUID, role string) error {
	const q = `
		INSERT INTO book_contributors (book_id, contributor_id, role, display_order)
		SELECT $1, $2, $3, COALESCE((SELECT MAX(display_order) FROM book_contributors WHERE book_id = $1), -1) + 1
		ON CONFLICT (book_id, contributor_id, role) DO NOTHING`
	var err error
	if tx != nil {
		_, err = tx.Exec(ctx, q, bookID, contributorID, role)
	} else {
		_, err = r.db.Exec(ctx, q, bookID, contributorID, role)
	}
	if err != nil {
		return fmt.Errorf("ensuring book contributor: %w", err)
	}
	return nil
}

// FindByID returns a single book by id. callerID + libraryID are
// optional; when non-Nil they hydrate the caller-scoped subqueries
// (user_read_status / user_rating / user_progress_pct + per-library
// active_loan_count) that mirror the list endpoint. Pass uuid.Nil for
// internal lookups that don't need them.
func (r *BookRepo) FindByID(ctx context.Context, id uuid.UUID, callerID uuid.UUID, libraryID uuid.UUID) (*models.Book, error) {
	args := []any{id}
	callerArg := 0
	loanLibraryArg := 0
	if callerID != uuid.Nil {
		args = append(args, callerID)
		callerArg = len(args)
	}
	if libraryID != uuid.Nil {
		args = append(args, libraryID)
		loanLibraryArg = len(args)
	}

	q := booksSelect(callerArg, loanLibraryArg) + ` WHERE b.id = $1`

	book, err := scanBook(r.db.QueryRow(ctx, q, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("finding book: %w", err)
	}
	return book, nil
}

// ListByContributor returns all books in a library that have the given contributor
// linked via book_contributors, ordered by title.
// callerID may be uuid.Nil to skip user_read_status population.
func (r *BookRepo) ListByContributor(ctx context.Context, libraryID, contributorID, callerID uuid.UUID) ([]*models.Book, error) {
	var args []any
	var q string
	const scope = `
		JOIN library_books lb ON lb.book_id = b.id
		WHERE lb.library_id = $1
		  AND EXISTS (
		    SELECT 1 FROM book_contributors bc2
		    WHERE bc2.book_id = b.id AND bc2.contributor_id = $2
		  )
		ORDER BY natural_sort_key(b.title)`
	if callerID != uuid.Nil {
		q = booksSelect(3, 1) + scope
		args = []any{libraryID, contributorID, callerID}
	} else {
		q = booksSelect(0, 1) + scope
		args = []any{libraryID, contributorID}
	}

	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("listing books by contributor: %w", err)
	}
	defer rows.Close()

	var out []*models.Book
	for rows.Next() {
		book, err := scanBook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, book)
	}
	return out, rows.Err()
}

// BookFingerprint summarizes the books collection in a library. Clients
// compare this to a stored fingerprint to decide whether to resync.
type BookFingerprint struct {
	Total         int     `json:"total"`
	MaxUpdatedAt  *string `json:"max_updated_at"` // RFC3339; nil when library is empty
}

// Fingerprint returns the total book count and the most recent updated_at
// (formatted RFC3339) for a library. MaxUpdatedAt is nil when empty.
func (r *BookRepo) Fingerprint(ctx context.Context, libraryID uuid.UUID) (BookFingerprint, error) {
	const q = `
		SELECT COUNT(*), MAX(b.updated_at)
		FROM books b
		JOIN library_books lb ON lb.book_id = b.id
		WHERE lb.library_id = $1`
	var fp BookFingerprint
	var maxTS pgtype.Timestamptz
	if err := r.db.QueryRow(ctx, q, libraryID).Scan(&fp.Total, &maxTS); err != nil {
		return fp, fmt.Errorf("book fingerprint: %w", err)
	}
	if maxTS.Valid {
		s := maxTS.Time.UTC().Format("2006-01-02T15:04:05.000Z")
		fp.MaxUpdatedAt = &s
	}
	return fp, nil
}

// ListBookLetters returns the distinct first letters (uppercased) of sort_title
// for all books in the library. Only alpha characters are returned.
func (r *BookRepo) ListBookLetters(ctx context.Context, libraryID uuid.UUID) ([]string, error) {
	const q = `
		SELECT DISTINCT upper(substr(sort_title(b.title), 1, 1)) AS letter
		FROM books b
		JOIN library_books lb ON lb.book_id = b.id
		WHERE lb.library_id = $1
		  AND sort_title(b.title) ~ '^[A-Za-z]'
		ORDER BY letter`
	rows, err := r.db.Query(ctx, q, libraryID)
	if err != nil {
		return nil, fmt.Errorf("listing book letters: %w", err)
	}
	defer rows.Close()
	var letters []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		letters = append(letters, l)
	}
	return letters, rows.Err()
}

// FilterCondition represents a single parsed search condition.
type FilterCondition struct {
	Field string `json:"field"` // "title" | "tag" | "type" | "contributor" | "genre" | "series" | "shelf" | "publisher" | "language"
	Op    string `json:"op"`    // "contains" | "not_contains" | "equals" | "not_equals" | "regex" | "phrase"
	Value string `json:"value"`
}

// ConditionGroup is a set of conditions with their own join mode.
// Multiple groups in a query are always ANDed together at the top level.
type ConditionGroup struct {
	Mode       string            `json:"mode"`       // "AND" | "OR"
	Conditions []FilterCondition `json:"conditions"`
}

type ListBooksOpts struct {
	Query      string
	Page       int
	PerPage    int
	Sort       string // "title" | "created_at" | "media_type"; default "title"
	SortDir    string // "asc" | "desc"; default "asc"
	Letter     string // single char: 'a'-'z' matches LIKE 'letter%'
	TagFilter  string // filter to books that have a tag with this exact name (case-insensitive)
	TypeFilter string // filter by media type display name (case-insensitive), e.g. "Novel"
	IsRegex    bool   // if true, use b.title ~* $query instead of ILIKE
	Groups     []ConditionGroup // from query language parser; groups are ANDed together
	CallerID   uuid.UUID        // when non-zero, includes user_read_status for this user
}

func (r *BookRepo) List(ctx context.Context, libraryID uuid.UUID, opts ListBooksOpts) ([]*models.Book, int, error) {
	if opts.PerPage <= 0 || opts.PerPage > 200 {
		opts.PerPage = 25
	}
	if opts.Page <= 0 {
		opts.Page = 1
	}
	offset := (opts.Page - 1) * opts.PerPage

	// Validate sort — title sort uses natural_sort_key() which strips leading articles,
	// lowercases, and pads digit sequences so #2 sorts before #10.
	sortCol := "natural_sort_key(b.title)"
	switch opts.Sort {
	case "created_at":
		sortCol = "b.created_at"
	case "media_type":
		sortCol = "lower(mt.display_name)"
	case "publish_date":
		sortCol = "(SELECT be.publish_date FROM book_editions be WHERE be.book_id = b.id AND be.is_primary = true AND be.publish_date IS NOT NULL LIMIT 1)"
	case "author":
		// Sort by the first contributor's lowercased name (display
		// order ascending so the "primary" credit wins). Books with
		// no contributors fall to the end via NULLS LAST below.
		sortCol = `(
			SELECT lower(c.name)
			FROM book_contributors bc
			JOIN contributors c ON c.id = bc.contributor_id
			WHERE bc.book_id = b.id
			ORDER BY bc.display_order, c.name
			LIMIT 1
		)`
	}
	sortDir := "ASC"
	if opts.SortDir == "desc" {
		sortDir = "DESC"
	}

	args := []any{libraryID} // $1
	argIdx := 2

	// Build WHERE conditions
	conditions := []string{}

	// Text search / regex
	if opts.Query != "" {
		if opts.IsRegex {
			conditions = append(conditions, fmt.Sprintf("b.title ~* $%d", argIdx))
			args = append(args, opts.Query)
			argIdx++
		} else {
			conditions = append(conditions, fmt.Sprintf(
				"(b.title ILIKE '%%' || $%d || '%%' "+
					"OR regexp_replace(lower(b.title), '[^a-z0-9]', '', 'g') LIKE '%%' || regexp_replace(lower($%d), '[^a-z0-9]', '', 'g') || '%%' "+
					"OR EXISTS (SELECT 1 FROM book_contributors bc_q JOIN contributors c_q ON c_q.id = bc_q.contributor_id WHERE bc_q.book_id = b.id AND c_q.name ILIKE '%%' || $%d || '%%'))",
				argIdx, argIdx+1, argIdx+2,
			))
			args = append(args, opts.Query, opts.Query, opts.Query)
			argIdx += 3
		}
	}

	// Letter filter — uses sort_title() so "The Way..." filters under W, not T.
	if opts.Letter != "" {
		if opts.Letter == "#" {
			conditions = append(conditions, fmt.Sprintf("sort_title(b.title) ~* $%d", argIdx))
			args = append(args, `^\d`)
			argIdx++
		} else {
			conditions = append(conditions, fmt.Sprintf("lower(sort_title(b.title)) LIKE $%d || '%%'", argIdx))
			args = append(args, strings.ToLower(opts.Letter))
			argIdx++
		}
	}

	// Tag filter
	if opts.TagFilter != "" {
		conditions = append(conditions, fmt.Sprintf(`EXISTS (
            SELECT 1 FROM book_tags bt2
            JOIN tags t2 ON t2.id = bt2.tag_id
            WHERE bt2.book_id = b.id AND LOWER(t2.name) = LOWER($%d)
        )`, argIdx))
		args = append(args, opts.TagFilter)
		argIdx++
	}

	// Type filter (by media type display name)
	if opts.TypeFilter != "" {
		conditions = append(conditions, fmt.Sprintf("LOWER(mt.display_name) = LOWER($%d)", argIdx))
		args = append(args, opts.TypeFilter)
		argIdx++
	}

	// Structured query condition groups (from query language parser).
	// Each group is ANDed with the others; conditions within a group use group.Mode.
	// Values are capped at 200 chars to limit any ReDoS exposure from regex patterns.
	for _, group := range opts.Groups {
		var parts []string
		for _, cond := range group.Conditions {
			if len(cond.Value) > 200 {
				continue // skip pathologically long values
			}
			switch cond.Field {
			case "title":
				switch cond.Op {
				case "contains", "phrase":
					parts = append(parts, fmt.Sprintf(
						"(b.title ILIKE '%%' || $%d || '%%' "+
							"OR regexp_replace(lower(b.title), '[^a-z0-9 ]', '', 'g') ILIKE '%%' || regexp_replace(lower($%d), '[^a-z0-9 ]', '', 'g') || '%%' "+
							"OR EXISTS (SELECT 1 FROM book_contributors bc_q JOIN contributors c_q ON c_q.id = bc_q.contributor_id WHERE bc_q.book_id = b.id AND c_q.name ILIKE '%%' || $%d || '%%'))",
						argIdx, argIdx+1, argIdx+2,
					))
					args = append(args, cond.Value, cond.Value, cond.Value)
					argIdx += 3
				case "not_contains":
					parts = append(parts, fmt.Sprintf("b.title NOT ILIKE '%%' || $%d || '%%'", argIdx))
					args = append(args, cond.Value)
					argIdx++
				case "regex":
					parts = append(parts, fmt.Sprintf("b.title ~* $%d", argIdx))
					args = append(args, cond.Value)
					argIdx++
				}
			case "type":
				switch cond.Op {
				case "equals":
					parts = append(parts, fmt.Sprintf("LOWER(mt.display_name) = LOWER($%d)", argIdx))
					args = append(args, cond.Value)
					argIdx++
				case "not_equals":
					parts = append(parts, fmt.Sprintf("LOWER(mt.display_name) != LOWER($%d)", argIdx))
					args = append(args, cond.Value)
					argIdx++
				}
			case "tag":
				switch cond.Op {
				case "equals":
					parts = append(parts, fmt.Sprintf(`EXISTS (
                    SELECT 1 FROM book_tags bt2 JOIN tags t2 ON t2.id = bt2.tag_id
                    WHERE bt2.book_id = b.id AND LOWER(t2.name) = LOWER($%d)
                )`, argIdx))
					args = append(args, cond.Value)
					argIdx++
				case "not_equals":
					parts = append(parts, fmt.Sprintf(`NOT EXISTS (
                    SELECT 1 FROM book_tags bt2 JOIN tags t2 ON t2.id = bt2.tag_id
                    WHERE bt2.book_id = b.id AND LOWER(t2.name) = LOWER($%d)
                )`, argIdx))
					args = append(args, cond.Value)
					argIdx++
				}
			case "contributor":
				switch cond.Op {
				case "contains":
					parts = append(parts, fmt.Sprintf(`EXISTS (
                    SELECT 1 FROM book_contributors bc2 JOIN contributors c2 ON c2.id = bc2.contributor_id
                    WHERE bc2.book_id = b.id AND c2.name ILIKE '%%' || $%d || '%%'
                )`, argIdx))
					args = append(args, cond.Value)
					argIdx++
				case "not_contains":
					parts = append(parts, fmt.Sprintf(`NOT EXISTS (
                    SELECT 1 FROM book_contributors bc2 JOIN contributors c2 ON c2.id = bc2.contributor_id
                    WHERE bc2.book_id = b.id AND c2.name ILIKE '%%' || $%d || '%%'
                )`, argIdx))
					args = append(args, cond.Value)
					argIdx++
				}
			case "genre":
				switch cond.Op {
				case "equals":
					parts = append(parts, fmt.Sprintf(`EXISTS (
                    SELECT 1 FROM book_genres bg2 JOIN genres g2 ON g2.id = bg2.genre_id
                    WHERE bg2.book_id = b.id AND LOWER(g2.name) = LOWER($%d)
                )`, argIdx))
					args = append(args, cond.Value)
					argIdx++
				case "not_equals":
					parts = append(parts, fmt.Sprintf(`NOT EXISTS (
                    SELECT 1 FROM book_genres bg2 JOIN genres g2 ON g2.id = bg2.genre_id
                    WHERE bg2.book_id = b.id AND LOWER(g2.name) = LOWER($%d)
                )`, argIdx))
					args = append(args, cond.Value)
					argIdx++
				}
			case "series":
				switch cond.Op {
				case "equals":
					parts = append(parts, fmt.Sprintf(`EXISTS (
                    SELECT 1 FROM book_series bs2 JOIN series s2 ON s2.id = bs2.series_id
                    WHERE bs2.book_id = b.id AND LOWER(s2.name) = LOWER($%d)
                )`, argIdx))
					args = append(args, cond.Value)
					argIdx++
				case "not_equals":
					parts = append(parts, fmt.Sprintf(`NOT EXISTS (
                    SELECT 1 FROM book_series bs2 JOIN series s2 ON s2.id = bs2.series_id
                    WHERE bs2.book_id = b.id AND LOWER(s2.name) = LOWER($%d)
                )`, argIdx))
					args = append(args, cond.Value)
					argIdx++
				}
			case "shelf":
				switch cond.Op {
				case "equals":
					parts = append(parts, fmt.Sprintf(`EXISTS (
                    SELECT 1 FROM book_shelves bsh2 JOIN shelves sh2 ON sh2.id = bsh2.shelf_id
                    WHERE bsh2.book_id = b.id AND LOWER(sh2.name) = LOWER($%d)
                )`, argIdx))
					args = append(args, cond.Value)
					argIdx++
				case "not_equals":
					parts = append(parts, fmt.Sprintf(`NOT EXISTS (
                    SELECT 1 FROM book_shelves bsh2 JOIN shelves sh2 ON sh2.id = bsh2.shelf_id
                    WHERE bsh2.book_id = b.id AND LOWER(sh2.name) = LOWER($%d)
                )`, argIdx))
					args = append(args, cond.Value)
					argIdx++
				}
			case "publisher":
				switch cond.Op {
				case "equals":
					parts = append(parts, fmt.Sprintf(`EXISTS (
                    SELECT 1 FROM book_editions be2
                    WHERE be2.book_id = b.id AND be2.is_primary = true AND LOWER(be2.publisher) = LOWER($%d)
                )`, argIdx))
					args = append(args, cond.Value)
					argIdx++
				case "not_equals":
					parts = append(parts, fmt.Sprintf(`NOT EXISTS (
                    SELECT 1 FROM book_editions be2
                    WHERE be2.book_id = b.id AND be2.is_primary = true AND LOWER(be2.publisher) = LOWER($%d)
                )`, argIdx))
					args = append(args, cond.Value)
					argIdx++
				}
			case "language":
				switch cond.Op {
				case "equals":
					parts = append(parts, fmt.Sprintf(`EXISTS (
                    SELECT 1 FROM book_editions be2
                    WHERE be2.book_id = b.id AND be2.is_primary = true AND LOWER(be2.language) = LOWER($%d)
                )`, argIdx))
					args = append(args, cond.Value)
					argIdx++
				case "not_equals":
					parts = append(parts, fmt.Sprintf(`NOT EXISTS (
                    SELECT 1 FROM book_editions be2
                    WHERE be2.book_id = b.id AND be2.is_primary = true AND LOWER(be2.language) = LOWER($%d)
                )`, argIdx))
					args = append(args, cond.Value)
					argIdx++
				}
			case "has_cover":
				switch cond.Op {
				case "equals":
					parts = append(parts, `EXISTS (
                    SELECT 1 FROM cover_images ci
                    WHERE ci.entity_type = 'book' AND ci.entity_id = b.id AND ci.is_primary = true
                )`)
				case "not_equals":
					parts = append(parts, `NOT EXISTS (
                    SELECT 1 FROM cover_images ci
                    WHERE ci.entity_type = 'book' AND ci.entity_id = b.id AND ci.is_primary = true
                )`)
				}
			}
		}
		if len(parts) > 0 {
			sep := " AND "
			if group.Mode == "OR" {
				sep = " OR "
			}
			joined := strings.Join(parts, sep)
			if len(parts) > 1 {
				joined = "(" + joined + ")"
			}
			conditions = append(conditions, joined)
		}
	}

	// Library scope routes through the library_books junction.
	where := "WHERE lb.library_id = $1"
	for _, c := range conditions {
		where += " AND " + c
	}
	scopeJoin := " JOIN library_books lb ON lb.book_id = b.id "

	// Count
	var total int
	countQ := "SELECT COUNT(*) FROM books b JOIN media_types mt ON mt.id = b.media_type_id " + scopeJoin + where
	if err := r.db.QueryRow(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting books: %w", err)
	}

	// Determine user_read_status arg index for the list query
	var selectQuery string
	if opts.CallerID != uuid.Nil {
		args = append(args, opts.CallerID)
		userArgIdx := argIdx
		argIdx++
		selectQuery = booksSelect(userArgIdx, 1)
	} else {
		selectQuery = booksSelect(0, 1)
	}

	// List
	args = append(args, opts.PerPage, offset)
	nullsClause := ""
	if opts.Sort == "publish_date" || opts.Sort == "author" {
		nullsClause = " NULLS LAST"
	}
	listQ := selectQuery + scopeJoin + where +
		fmt.Sprintf(" ORDER BY %s %s%s LIMIT $%d OFFSET $%d", sortCol, sortDir, nullsClause, argIdx, argIdx+1)

	rows, err := r.db.Query(ctx, listQ, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("listing books: %w", err)
	}
	defer rows.Close()

	var books []*models.Book
	for rows.Next() {
		b, err := scanBook(rows)
		if err != nil {
			return nil, 0, err
		}
		books = append(books, b)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return books, total, nil
}

// SearchSuggestions returns up to 5 book titles in the library whose
// (title || subtitle) is similar to the supplied query, ranked by trgm
// distance. Used by the books-search "did you mean" fallback when a
// literal match returns nothing. Empty result if pg_trgm has no candidates
// above the default similarity threshold (0.3).
func (r *BookRepo) SearchSuggestions(ctx context.Context, libraryID uuid.UUID, query string) ([]string, error) {
	const q = `
		SELECT DISTINCT ON (lower(b.title)) b.title
		FROM books b
		JOIN library_books lb ON lb.book_id = b.id
		WHERE lb.library_id = $1
		  AND lower(b.title || ' ' || COALESCE(b.subtitle, '')) % lower($2)
		ORDER BY lower(b.title), lower(b.title || ' ' || COALESCE(b.subtitle, '')) <-> lower($2)
		LIMIT 5`
	rows, err := r.db.Query(ctx, q, libraryID, query)
	if err != nil {
		return nil, fmt.Errorf("fuzzy search: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *BookRepo) Update(ctx context.Context, tx pgx.Tx, id uuid.UUID, title, subtitle string, mediaTypeID uuid.UUID, description string) error {
	const q = `
		UPDATE books
		SET title        = $2,
		    subtitle     = NULLIF($3, ''),
		    media_type_id = $4,
		    description  = NULLIF($5, '')
		WHERE id = $1`

	_, err := tx.Exec(ctx, q, id, title, subtitle, mediaTypeID, description)
	if err != nil {
		return fmt.Errorf("updating book: %w", err)
	}
	return nil
}

// GetDescription returns the current description text for a book; empty
// string when null or missing. Used by the AI metadata enrichment worker
// to feed the cleanup prompt without pulling the whole book.
func (r *BookRepo) GetDescription(ctx context.Context, id uuid.UUID) (string, error) {
	var desc *string
	if err := r.db.QueryRow(ctx, `SELECT description FROM books WHERE id = $1`, id).Scan(&desc); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("get description: %w", err)
	}
	if desc == nil {
		return "", nil
	}
	return *desc, nil
}

// UpdateDescription writes a new description value (empty string clears it).
// Bumps updated_at via the row-level trigger so cache busters see the change.
func (r *BookRepo) UpdateDescription(ctx context.Context, id uuid.UUID, description string) error {
	if _, err := r.db.Exec(ctx, `UPDATE books SET description = NULLIF($2, '') WHERE id = $1`, id, description); err != nil {
		return fmt.Errorf("update description: %w", err)
	}
	return nil
}

// Touch bumps updated_at on the book row (the DB trigger sets it to NOW()).
// Used to ensure cover changes are visible to change-detection fingerprinting.
func (r *BookRepo) Touch(ctx context.Context, id uuid.UUID) error {
	_, err := r.db.Exec(ctx, `UPDATE books SET updated_at = NOW() WHERE id = $1`, id)
	return err
}

func (r *BookRepo) Delete(ctx context.Context, id uuid.UUID) error {
	result, err := r.db.Exec(ctx, `DELETE FROM books WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("deleting book: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ─── Scanner ──────────────────────────────────────────────────────────────────

func scanBook(s scanner) (*models.Book, error) {
	var (
		pgID           pgtype.UUID
		pgMediaTypeID  pgtype.UUID
		pgSubtitle     pgtype.Text
		pgDesc         pgtype.Text
		mediaTypeName  string
		contribJSON    []byte
		tagsJSON       []byte
		genresJSON     []byte
		seriesJSON     []byte
		shelvesJSON    []byte
		publisher      pgtype.Text
		publishYear    pgtype.Int4
		language       pgtype.Text
		userReadStatus string
		userRating     int
		userProgress   pgtype.Numeric
		activeLoans    int
		b              models.Book
	)

	err := s.Scan(
		&pgID, &b.Title, &pgSubtitle,
		&pgMediaTypeID, &mediaTypeName,
		&pgDesc, &b.CreatedAt, &b.UpdatedAt,
		&contribJSON, &tagsJSON, &genresJSON, &b.HasCover,
		&seriesJSON, &shelvesJSON, &publisher, &publishYear, &language,
		&userReadStatus, &userRating, &userProgress, &activeLoans,
	)
	if err != nil {
		return nil, err
	}

	b.ID = uuid.UUID(pgID.Bytes)
	b.MediaTypeID = uuid.UUID(pgMediaTypeID.Bytes)
	b.MediaType = mediaTypeName
	b.Subtitle = pgSubtitle.String
	b.Description = pgDesc.String
	b.Publisher = publisher.String
	b.Language = language.String
	b.UserReadStatus = userReadStatus
	b.UserRating = userRating
	if userProgress.Valid {
		f, _ := userProgress.Float64Value()
		b.UserProgressPct = f.Float64
	}
	b.ActiveLoanCount = activeLoans
	if publishYear.Valid {
		y := int(publishYear.Int32)
		b.PublishYear = &y
	}

	b.Contributors = []models.BookContributor{}
	if len(contribJSON) > 0 {
		if err := json.Unmarshal(contribJSON, &b.Contributors); err != nil {
			return nil, fmt.Errorf("unmarshaling contributors: %w", err)
		}
	}

	b.Tags = []models.Tag{}
	if len(tagsJSON) > 0 {
		if err := json.Unmarshal(tagsJSON, &b.Tags); err != nil {
			return nil, fmt.Errorf("unmarshaling tags: %w", err)
		}
	}

	b.Genres = []models.Genre{}
	if len(genresJSON) > 0 {
		if err := json.Unmarshal(genresJSON, &b.Genres); err != nil {
			return nil, fmt.Errorf("unmarshaling genres: %w", err)
		}
	}

	b.Series = []models.BookSeriesRef{}
	if len(seriesJSON) > 0 {
		if err := json.Unmarshal(seriesJSON, &b.Series); err != nil {
			return nil, fmt.Errorf("unmarshaling series: %w", err)
		}
	}

	b.Shelves = []models.BookShelfRef{}
	if len(shelvesJSON) > 0 {
		if err := json.Unmarshal(shelvesJSON, &b.Shelves); err != nil {
			return nil, fmt.Errorf("unmarshaling shelves: %w", err)
		}
	}

	return &b, nil
}

// ListBooksMissingCover returns every book id that has no primary cover
// image associated with it. Used by the cover-backfill scheduled job to
// find candidates for metadata enrichment.
func (r *BookRepo) ListBooksMissingCover(ctx context.Context, limit int) ([]uuid.UUID, error) {
	if limit <= 0 {
		limit = 1000
	}
	const q = `
		SELECT b.id
		  FROM books b
		 WHERE NOT EXISTS (
		         SELECT 1 FROM cover_images ci
		          WHERE ci.entity_type = 'book'
		            AND ci.entity_id   = b.id
		            AND ci.is_primary  = TRUE
		       )
		 ORDER BY b.created_at DESC
		 LIMIT $1`
	rows, err := r.db.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("listing books missing cover: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var pgID pgtype.UUID
		if err := rows.Scan(&pgID); err != nil {
			return nil, err
		}
		out = append(out, uuid.UUID(pgID.Bytes))
	}
	return out, rows.Err()
}

// TitlesByIDs returns a map of book ID → title for the given IDs.
// Missing IDs are simply absent from the result map.
func (r *BookRepo) TitlesByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]string{}, nil
	}
	rows, err := r.db.Query(ctx, `SELECT id, title FROM books WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("fetching book titles: %w", err)
	}
	defer rows.Close()

	out := make(map[uuid.UUID]string, len(ids))
	for rows.Next() {
		var pgID pgtype.UUID
		var title string
		if err := rows.Scan(&pgID, &title); err != nil {
			return nil, fmt.Errorf("scanning book title: %w", err)
		}
		out[uuid.UUID(pgID.Bytes)] = title
	}
	return out, rows.Err()
}

// ─── Dashboard queries ────────────────────────────────────────────────────────

// CurrentlyReadingBook is a lightweight book summary for dashboard use.
type CurrentlyReadingBook struct {
	BookID      uuid.UUID
	LibraryID   uuid.UUID
	LibraryName string
	Title       string
	HasCover    bool
	UpdatedAt   pgtype.Timestamptz
	Authors     string // comma-separated display names
}

// CurrentlyReading returns up to limit books where the user has read_status='reading',
// ordered by when the interaction was last updated (most recent first).
// Spans all libraries the user is a member of.
func (r *BookRepo) CurrentlyReading(ctx context.Context, userID uuid.UUID, limit int) ([]*CurrentlyReadingBook, error) {
	// Under M2M a book may live in several libraries the user is a member of.
	// Pick the earliest-added library as the book's representative (inner
	// DISTINCT ON), then order the final result by last-interaction time.
	q := `
		WITH user_book AS (
			SELECT DISTINCT ON (lb.book_id)
				lb.book_id, lb.library_id
			FROM library_books lb
			JOIN library_memberships lm ON lm.library_id = lb.library_id AND lm.user_id = $1
			ORDER BY lb.book_id, lb.added_at ASC
		)
		SELECT
			b.id, ub.library_id, l.name,
			b.title, b.updated_at,
			EXISTS(
				SELECT 1 FROM cover_images ci
				WHERE ci.entity_type = 'book' AND ci.entity_id = b.id AND ci.is_primary = true
			) AS has_cover,
			COALESCE((
				SELECT string_agg(c.name, ', ' ORDER BY bc.display_order)
				FROM book_contributors bc
				JOIN contributors c ON c.id = bc.contributor_id
				WHERE bc.book_id = b.id
			), '') AS authors
		FROM books b
		JOIN user_book ub ON ub.book_id = b.id
		JOIN libraries l ON l.id = ub.library_id
		WHERE EXISTS (
			SELECT 1 FROM book_editions be
			JOIN user_book_interactions ubi ON ubi.book_edition_id = be.id
			WHERE be.book_id = b.id AND ubi.user_id = $1 AND ubi.read_status = 'reading'
		)
		ORDER BY (
			SELECT MAX(ubi.updated_at)
			FROM book_editions be
			JOIN user_book_interactions ubi ON ubi.book_edition_id = be.id
			WHERE be.book_id = b.id AND ubi.user_id = $1
		) DESC NULLS LAST
		LIMIT $2`

	rows, err := r.db.Query(ctx, q, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("querying currently reading: %w", err)
	}
	defer rows.Close()

	var out []*CurrentlyReadingBook
	for rows.Next() {
		var b CurrentlyReadingBook
		var pgBookID, pgLibID pgtype.UUID
		if err := rows.Scan(
			&pgBookID, &pgLibID, &b.LibraryName,
			&b.Title, &b.UpdatedAt,
			&b.HasCover, &b.Authors,
		); err != nil {
			return nil, err
		}
		b.BookID = uuid.UUID(pgBookID.Bytes)
		b.LibraryID = uuid.UUID(pgLibID.Bytes)
		out = append(out, &b)
	}
	return out, rows.Err()
}

// RecentlyAddedBook is a lightweight book summary for the recently-added dashboard module.
type RecentlyAddedBook struct {
	BookID      uuid.UUID
	LibraryID   uuid.UUID
	LibraryName string
	Title       string
	HasCover    bool
	CreatedAt   pgtype.Timestamptz
	Authors     string
	ReadStatus  string
}

// RecentlyAdded returns the most recently added books across all libraries the user
// is a member of.
func (r *BookRepo) RecentlyAdded(ctx context.Context, userID uuid.UUID, limit int) ([]*RecentlyAddedBook, error) {
	q := `
		WITH user_book AS (
			SELECT DISTINCT ON (lb.book_id)
				lb.book_id, lb.library_id, lb.added_at
			FROM library_books lb
			JOIN library_memberships lm ON lm.library_id = lb.library_id AND lm.user_id = $1
			ORDER BY lb.book_id, lb.added_at ASC
		)
		SELECT
			b.id, ub.library_id, l.name,
			b.title, ub.added_at,
			EXISTS(
				SELECT 1 FROM cover_images ci
				WHERE ci.entity_type = 'book' AND ci.entity_id = b.id AND ci.is_primary = true
			) AS has_cover,
			COALESCE((
				SELECT string_agg(c.name, ', ' ORDER BY bc.display_order)
				FROM book_contributors bc
				JOIN contributors c ON c.id = bc.contributor_id
				WHERE bc.book_id = b.id
			), '') AS authors,
			COALESCE((
				SELECT ubi.read_status
				FROM book_editions be_rs
				JOIN user_book_interactions ubi ON ubi.book_edition_id = be_rs.id
				WHERE be_rs.book_id = b.id AND ubi.user_id = $1
				ORDER BY CASE ubi.read_status
					WHEN 'read'           THEN 1
					WHEN 'reading'        THEN 2
					WHEN 'did_not_finish' THEN 3
					ELSE 4
				END
				LIMIT 1
			), '') AS read_status
		FROM books b
		JOIN user_book ub ON ub.book_id = b.id
		JOIN libraries l ON l.id = ub.library_id
		ORDER BY ub.added_at DESC
		LIMIT $2`

	rows, err := r.db.Query(ctx, q, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("querying recently added: %w", err)
	}
	defer rows.Close()

	var out []*RecentlyAddedBook
	for rows.Next() {
		var b RecentlyAddedBook
		var pgBookID, pgLibID pgtype.UUID
		if err := rows.Scan(
			&pgBookID, &pgLibID, &b.LibraryName,
			&b.Title, &b.CreatedAt,
			&b.HasCover, &b.Authors, &b.ReadStatus,
		); err != nil {
			return nil, err
		}
		b.BookID = uuid.UUID(pgBookID.Bytes)
		b.LibraryID = uuid.UUID(pgLibID.Bytes)
		out = append(out, &b)
	}
	return out, rows.Err()
}

// PicksOfTheDay returns a deterministic pseudo-random set of unread books for the
// user, seeded by the given day. Passing the same daySeed all day returns the
// same picks; it rotates automatically when the caller advances the seed.
// Books already read, in progress, or marked did-not-finish are excluded. If
// mediaTypes is empty, all media types are eligible.
func (r *BookRepo) PicksOfTheDay(ctx context.Context, userID uuid.UUID, mediaTypes []string, daySeed string, limit int) ([]*RecentlyAddedBook, error) {
	q := `
		WITH user_book AS (
			SELECT DISTINCT ON (lb.book_id)
				lb.book_id, lb.library_id, lb.added_at
			FROM library_books lb
			JOIN library_memberships lm ON lm.library_id = lb.library_id AND lm.user_id = $1
			ORDER BY lb.book_id, lb.added_at ASC
		)
		SELECT
			b.id, ub.library_id, l.name,
			b.title, ub.added_at,
			EXISTS(
				SELECT 1 FROM cover_images ci
				WHERE ci.entity_type = 'book' AND ci.entity_id = b.id AND ci.is_primary = true
			) AS has_cover,
			COALESCE((
				SELECT string_agg(c.name, ', ' ORDER BY bc.display_order)
				FROM book_contributors bc
				JOIN contributors c ON c.id = bc.contributor_id
				WHERE bc.book_id = b.id
			), '') AS authors,
			'' AS read_status
		FROM books b
		JOIN user_book ub ON ub.book_id = b.id
		JOIN libraries l ON l.id = ub.library_id
		JOIN media_types mt ON mt.id = b.media_type_id
		WHERE (cardinality($2::text[]) = 0 OR mt.name = ANY($2))
		  AND NOT EXISTS (
			SELECT 1 FROM book_editions be
			JOIN user_book_interactions ubi ON ubi.book_edition_id = be.id
			WHERE be.book_id = b.id
			  AND ubi.user_id = $1
			  AND ubi.read_status IN ('read', 'reading', 'did_not_finish')
		  )
		ORDER BY md5(b.id::text || $3)
		LIMIT $4`

	rows, err := r.db.Query(ctx, q, userID, mediaTypes, daySeed, limit)
	if err != nil {
		return nil, fmt.Errorf("querying picks of the day: %w", err)
	}
	defer rows.Close()

	var out []*RecentlyAddedBook
	for rows.Next() {
		var b RecentlyAddedBook
		var pgBookID, pgLibID pgtype.UUID
		if err := rows.Scan(
			&pgBookID, &pgLibID, &b.LibraryName,
			&b.Title, &b.CreatedAt,
			&b.HasCover, &b.Authors, &b.ReadStatus,
		); err != nil {
			return nil, err
		}
		b.BookID = uuid.UUID(pgBookID.Bytes)
		b.LibraryID = uuid.UUID(pgLibID.Bytes)
		out = append(out, &b)
	}
	return out, rows.Err()
}

// DashboardStats holds aggregate reading statistics for a user.
type DashboardStats struct {
	TotalBooks         int
	BooksRead          int
	BooksReading       int
	BooksAddedThisYear int
	BooksReadThisYear  int
	FavoritesCount     int
	// MonthlyReads is a 12-entry series of (month, count) for the trailing
	// 12 months, oldest first. Months with no reads still appear with count=0.
	MonthlyReads []MonthlyReadBucket
}

// MonthlyReadBucket is one month of the reading sparkline.
type MonthlyReadBucket struct {
	Month string // "YYYY-MM"
	Count int
}

// GetDashboardStats returns aggregate book and reading counts for the user across
// all libraries they are a member of.
//
// "Read this year" / the monthly sparkline only count interactions with a
// known date_finished. There is no fallback to updated_at: that column
// reflects whenever the row was last written for any reason (an import, a
// metadata refresh, an unrelated bulk correction), not when the book was
// actually finished, and using it as a stand-in silently misattributes
// books with no known finish date to "just read" the moment anything
// touches their row.
func (r *BookRepo) GetDashboardStats(ctx context.Context, userID uuid.UUID) (*DashboardStats, error) {
	var s DashboardStats
	err := r.db.QueryRow(ctx, `
		WITH user_book AS (
			SELECT DISTINCT ON (lb.book_id) lb.book_id, lb.added_at
			FROM library_books lb
			JOIN library_memberships lm ON lm.library_id = lb.library_id AND lm.user_id = $1
			ORDER BY lb.book_id, lb.added_at ASC
		)
		SELECT
			COUNT(DISTINCT b.id) AS total_books,
			COUNT(DISTINCT CASE WHEN ubi.read_status = 'read'    THEN b.id END) AS books_read,
			COUNT(DISTINCT CASE WHEN ubi.read_status = 'reading' THEN b.id END) AS books_reading,
			COUNT(DISTINCT CASE WHEN ub.added_at >= date_trunc('year', NOW()) THEN b.id END) AS added_this_year,
			COUNT(DISTINCT CASE
				WHEN ubi.read_status = 'read'
				 AND ubi.date_finished IS NOT NULL
				 AND ubi.date_finished::timestamptz >= date_trunc('year', NOW())
				THEN b.id END) AS read_this_year,
			COUNT(DISTINCT CASE WHEN ubi.is_favorite THEN b.id END) AS favorites_count
		FROM books b
		JOIN user_book ub ON ub.book_id = b.id
		LEFT JOIN book_editions be ON be.book_id = b.id
		LEFT JOIN user_book_interactions ubi ON ubi.book_edition_id = be.id AND ubi.user_id = $1`,
		userID,
	).Scan(&s.TotalBooks, &s.BooksRead, &s.BooksReading, &s.BooksAddedThisYear,
		&s.BooksReadThisYear, &s.FavoritesCount)
	if err != nil {
		return nil, fmt.Errorf("querying dashboard stats: %w", err)
	}

	// Monthly sparkline: generate 12 months, left-join actual reads per month
	// so months with no reads still show as 0.
	rows, err := r.db.Query(ctx, `
		WITH months AS (
			SELECT generate_series(
				date_trunc('month', NOW()) - INTERVAL '11 months',
				date_trunc('month', NOW()),
				INTERVAL '1 month'
			) AS m
		),
		reads AS (
			SELECT
				date_trunc('month', ubi.date_finished::timestamptz) AS m,
				COUNT(DISTINCT b.id) AS c
			FROM books b
			JOIN library_books lb ON lb.book_id = b.id
			JOIN library_memberships lm ON lm.library_id = lb.library_id AND lm.user_id = $1
			JOIN book_editions be ON be.book_id = b.id
			JOIN user_book_interactions ubi ON ubi.book_edition_id = be.id AND ubi.user_id = $1
			WHERE ubi.read_status = 'read'
			  AND ubi.date_finished IS NOT NULL
			  AND ubi.date_finished::timestamptz >= date_trunc('month', NOW()) - INTERVAL '11 months'
			GROUP BY 1
		)
		SELECT to_char(months.m, 'YYYY-MM'), COALESCE(reads.c, 0)
		FROM months
		LEFT JOIN reads ON reads.m = months.m
		ORDER BY months.m`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying monthly reads: %w", err)
	}
	defer rows.Close()

	s.MonthlyReads = make([]MonthlyReadBucket, 0, 12)
	for rows.Next() {
		var b MonthlyReadBucket
		if err := rows.Scan(&b.Month, &b.Count); err != nil {
			return nil, fmt.Errorf("scanning monthly bucket: %w", err)
		}
		s.MonthlyReads = append(s.MonthlyReads, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &s, nil
}

// ─── Continue Series ─────────────────────────────────────────────────────────

// ContinueSeriesBook is the next book in a series the user has started.
type ContinueSeriesBook struct {
	SeriesID         uuid.UUID
	SeriesName       string
	Position         float64
	LastReadPosition float64
	BookID           uuid.UUID
	LibraryID        uuid.UUID
	LibraryName      string
	Title            string
	Authors          string
	HasCover         bool
	UpdatedAt        pgtype.Timestamptz
	ReadStatus       string // 'reading' or '' (unread)
}

// ContinueSeries returns the next unread (or currently-reading) book in each series
// where the user has already finished at least one book. Skips series where the next
// book is not in the user's library.
func (r *BookRepo) ContinueSeries(ctx context.Context, userID uuid.UUID, limit int) ([]*ContinueSeriesBook, error) {
	q := `
		WITH user_read_positions AS (
			-- Highest position the user has finished in each series
			SELECT bs.series_id, MAX(bs.position) AS max_pos
			FROM book_series bs
			JOIN books b ON b.id = bs.book_id
			JOIN library_books lb ON lb.book_id = b.id
			JOIN library_memberships lm ON lm.library_id = lb.library_id AND lm.user_id = $1
			JOIN book_editions be ON be.book_id = b.id
			JOIN user_book_interactions ubi ON ubi.book_edition_id = be.id AND ubi.user_id = $1
			WHERE ubi.read_status = 'read'
			GROUP BY bs.series_id
		),
		next_books AS (
			-- The lowest position > max_pos that the user has NOT yet marked 'read'
			SELECT DISTINCT ON (bs.series_id)
				bs.series_id, bs.book_id, bs.position, urp.max_pos,
				lb.library_id
			FROM book_series bs
			JOIN user_read_positions urp ON urp.series_id = bs.series_id
			JOIN books b ON b.id = bs.book_id
			JOIN library_books lb ON lb.book_id = b.id
			JOIN library_memberships lm ON lm.library_id = lb.library_id AND lm.user_id = $1
			WHERE bs.position > urp.max_pos
			  AND NOT EXISTS (
				SELECT 1
				FROM book_editions be2
				JOIN user_book_interactions ubi2 ON ubi2.book_edition_id = be2.id
				WHERE be2.book_id = bs.book_id
				  AND ubi2.user_id = $1
				  AND ubi2.read_status = 'read'
			  )
			ORDER BY bs.series_id, bs.position ASC
		)
		SELECT
			s.id, s.name, nb.position, nb.max_pos,
			b.id, nb.library_id, l.name, b.title, b.updated_at,
			EXISTS(
				SELECT 1 FROM cover_images ci
				WHERE ci.entity_type = 'book' AND ci.entity_id = b.id AND ci.is_primary = true
			),
			COALESCE((
				SELECT string_agg(c.name, ', ' ORDER BY bc.display_order)
				FROM book_contributors bc
				JOIN contributors c ON c.id = bc.contributor_id
				WHERE bc.book_id = b.id
			), ''),
			COALESCE((
				SELECT ubi.read_status
				FROM book_editions be_rs
				JOIN user_book_interactions ubi ON ubi.book_edition_id = be_rs.id
				WHERE be_rs.book_id = b.id AND ubi.user_id = $1
				ORDER BY CASE ubi.read_status
					WHEN 'reading' THEN 1
					WHEN 'did_not_finish' THEN 2
					ELSE 3
				END
				LIMIT 1
			), '')
		FROM next_books nb
		JOIN series s  ON s.id  = nb.series_id
		JOIN books  b  ON b.id  = nb.book_id
		JOIN libraries l ON l.id = nb.library_id
		ORDER BY nb.position ASC, lower(s.name)
		LIMIT $2`

	rows, err := r.db.Query(ctx, q, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("querying continue series: %w", err)
	}
	defer rows.Close()

	var out []*ContinueSeriesBook
	for rows.Next() {
		var b ContinueSeriesBook
		var pgSeriesID, pgBookID, pgLibID pgtype.UUID
		if err := rows.Scan(
			&pgSeriesID, &b.SeriesName, &b.Position, &b.LastReadPosition,
			&pgBookID, &pgLibID, &b.LibraryName, &b.Title, &b.UpdatedAt,
			&b.HasCover, &b.Authors, &b.ReadStatus,
		); err != nil {
			return nil, err
		}
		b.SeriesID = uuid.UUID(pgSeriesID.Bytes)
		b.BookID = uuid.UUID(pgBookID.Bytes)
		b.LibraryID = uuid.UUID(pgLibID.Bytes)
		out = append(out, &b)
	}
	return out, rows.Err()
}

// ─── Recently Finished ───────────────────────────────────────────────────────

// FinishedBook is a book the user has marked as read, with finish metadata.
type FinishedBook struct {
	BookID      uuid.UUID
	LibraryID   uuid.UUID
	LibraryName string
	Title       string
	Authors     string
	HasCover    bool
	FinishedAt  pgtype.Timestamptz
	Rating      pgtype.Int2
	IsFavorite  bool
}

// RecentlyFinished returns the most recently finished books for the user.
// De-duplicates by book (multiple editions read collapse to a single row).
func (r *BookRepo) RecentlyFinished(ctx context.Context, userID uuid.UUID, limit int) ([]*FinishedBook, error) {
	q := `
		WITH user_book AS (
			SELECT DISTINCT ON (lb.book_id) lb.book_id, lb.library_id
			FROM library_books lb
			JOIN library_memberships lm ON lm.library_id = lb.library_id AND lm.user_id = $1
			ORDER BY lb.book_id, lb.added_at ASC
		),
		finished AS (
			SELECT DISTINCT ON (b.id)
				b.id AS book_id,
				ub.library_id,
				b.title,
				COALESCE(ubi.date_finished::timestamptz, ubi.updated_at) AS finished_at,
				ubi.rating,
				ubi.is_favorite
			FROM books b
			JOIN user_book ub ON ub.book_id = b.id
			JOIN book_editions be ON be.book_id = b.id
			JOIN user_book_interactions ubi ON ubi.book_edition_id = be.id AND ubi.user_id = $1
			WHERE ubi.read_status = 'read'
			ORDER BY b.id, COALESCE(ubi.date_finished::timestamptz, ubi.updated_at) DESC
		)
		SELECT
			f.book_id, f.library_id, l.name, f.title, f.finished_at, f.rating, f.is_favorite,
			EXISTS(
				SELECT 1 FROM cover_images ci
				WHERE ci.entity_type = 'book' AND ci.entity_id = f.book_id AND ci.is_primary = true
			),
			COALESCE((
				SELECT string_agg(c.name, ', ' ORDER BY bc.display_order)
				FROM book_contributors bc
				JOIN contributors c ON c.id = bc.contributor_id
				WHERE bc.book_id = f.book_id
			), '')
		FROM finished f
		JOIN libraries l ON l.id = f.library_id
		ORDER BY f.finished_at DESC
		LIMIT $2`

	rows, err := r.db.Query(ctx, q, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("querying recently finished: %w", err)
	}
	defer rows.Close()

	var out []*FinishedBook
	for rows.Next() {
		var b FinishedBook
		var pgBookID, pgLibID pgtype.UUID
		if err := rows.Scan(
			&pgBookID, &pgLibID, &b.LibraryName, &b.Title, &b.FinishedAt,
			&b.Rating, &b.IsFavorite, &b.HasCover, &b.Authors,
		); err != nil {
			return nil, err
		}
		b.BookID = uuid.UUID(pgBookID.Bytes)
		b.LibraryID = uuid.UUID(pgLibID.Bytes)
		out = append(out, &b)
	}
	return out, rows.Err()
}

// BooksWithoutCover returns the subset of input book IDs that have no
// primary cover image on file. Used by the import worker so a "fetch
// covers" pass only queues books that actually need a cover lookup —
// without this the post-import cover batch shows "0/1410" while every
// row immediately no-ops, which reads as broken.
func (r *BookRepo) BooksWithoutCover(ctx context.Context, bookIDs []uuid.UUID) ([]uuid.UUID, error) {
	if len(bookIDs) == 0 {
		return nil, nil
	}
	const q = `
		SELECT b.id
		FROM   unnest($1::uuid[]) AS b(id)
		WHERE  NOT EXISTS (
			SELECT 1 FROM cover_images ci
			WHERE  ci.entity_type = 'book'
			   AND ci.entity_id   = b.id
			   AND ci.is_primary  = true
		)`
	rows, err := r.db.Query(ctx, q, bookIDs)
	if err != nil {
		return nil, fmt.Errorf("filtering books without cover: %w", err)
	}
	defer rows.Close()
	out := make([]uuid.UUID, 0, len(bookIDs))
	for rows.Next() {
		var pgID pgtype.UUID
		if err := rows.Scan(&pgID); err != nil {
			return nil, fmt.Errorf("scanning book id: %w", err)
		}
		out = append(out, uuid.UUID(pgID.Bytes))
	}
	return out, rows.Err()
}

// SetBookTags replaces all tags for a book within the given transaction.
func (r *BookRepo) SetBookTags(ctx context.Context, tx pgx.Tx, bookID uuid.UUID, tagIDs []uuid.UUID) error {
	if _, err := tx.Exec(ctx, `DELETE FROM book_tags WHERE book_id = $1`, bookID); err != nil {
		return fmt.Errorf("clearing book tags: %w", err)
	}
	for _, tid := range tagIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO book_tags (book_id, tag_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			bookID, tid,
		); err != nil {
			return fmt.Errorf("inserting book tag: %w", err)
		}
	}
	return nil
}
