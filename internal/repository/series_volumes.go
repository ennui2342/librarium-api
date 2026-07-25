// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/fireball1725/librarium-api/internal/models"
	"github.com/fireball1725/librarium-api/internal/providers"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type SeriesVolumesRepo struct {
	db *pgxpool.Pool
}

func NewSeriesVolumesRepo(db *pgxpool.Pool) *SeriesVolumesRepo {
	return &SeriesVolumesRepo{db: db}
}

// List returns all volumes for a series, ordered by position.
func (r *SeriesVolumesRepo) List(ctx context.Context, seriesID uuid.UUID) ([]*models.SeriesVolume, error) {
	const q = `
		SELECT id, series_id, position, COALESCE(title,''), release_date,
		       COALESCE(cover_url,''), COALESCE(external_id,''), created_at, updated_at
		FROM series_volumes
		WHERE series_id = $1
		ORDER BY position`
	rows, err := r.db.Query(ctx, q, seriesID)
	if err != nil {
		return nil, fmt.Errorf("listing series volumes: %w", err)
	}
	defer rows.Close()

	var out []*models.SeriesVolume
	for rows.Next() {
		var (
			pgID       pgtype.UUID
			pgSeriesID pgtype.UUID
			pgDate     pgtype.Date
			vol        models.SeriesVolume
		)
		if err := rows.Scan(
			&pgID, &pgSeriesID, &vol.Position, &vol.Title,
			&pgDate, &vol.CoverURL, &vol.ExternalID,
			&vol.CreatedAt, &vol.UpdatedAt,
		); err != nil {
			return nil, err
		}
		vol.ID = uuid.UUID(pgID.Bytes)
		vol.SeriesID = uuid.UUID(pgSeriesID.Bytes)
		if pgDate.Valid {
			t := pgDate.Time
			vol.ReleaseDate = &t
		}
		out = append(out, &vol)
	}
	return out, rows.Err()
}

// Sync upserts the provider-fetched volumes. Does NOT delete volumes already in the table
// that are not in the provider list (so manually-added data is preserved).
//
// Continues past a single volume's failure instead of aborting the whole
// sync — a provider returning one malformed row (e.g. a ReleaseDate that
// doesn't actually match the "YYYY-MM-DD" the VolumeResult contract
// promises) previously discarded every other volume in the series along
// with it, since each row failing an INSERT stopped the loop immediately.
// Failures are collected and returned together so the caller still knows
// something was wrong, but whatever did parse is persisted regardless.
func (r *SeriesVolumesRepo) Sync(ctx context.Context, seriesID uuid.UUID, volumes []providers.VolumeResult) error {
	const q = `
		INSERT INTO series_volumes (id, series_id, position, title, release_date, cover_url, external_id)
		VALUES ($1, $2, $3, NULLIF($4,''), $5::date, NULLIF($6,''), NULLIF($7,''))
		ON CONFLICT (series_id, position) DO UPDATE
		SET title        = EXCLUDED.title,
		    release_date = COALESCE(EXCLUDED.release_date, series_volumes.release_date),
		    cover_url    = COALESCE(EXCLUDED.cover_url, series_volumes.cover_url),
		    external_id  = COALESCE(EXCLUDED.external_id, series_volumes.external_id),
		    updated_at   = NOW()`

	var errs []error
	for _, v := range volumes {
		id := uuid.New()
		var releaseDate *string
		if v.ReleaseDate != "" {
			releaseDate = &v.ReleaseDate
		}
		if _, err := r.db.Exec(ctx, q, id, seriesID, v.Position, v.Title, releaseDate, v.CoverURL, v.ExternalID); err != nil {
			errs = append(errs, fmt.Errorf("upserting series volume at position %v: %w", v.Position, err))
		}
	}
	return errors.Join(errs...)
}

// ListSeriesWithExternalSource returns all series that have an external_source set.
// Used by the background release checker.
func (r *SeriesVolumesRepo) ListSeriesWithExternalSource(ctx context.Context) ([]*models.Series, error) {
	const q = `
		SELECT s.id, s.library_id, s.name, COALESCE(s.description,''),
		       s.total_count, s.status, COALESCE(s.original_language,''), s.publication_year,
		       COALESCE(s.demographic,''), s.genres, COALESCE(s.url,''),
		       COALESCE(s.external_id,''), COALESCE(s.external_source,''),
		       0 AS book_count,
		       s.created_at, s.updated_at
		FROM series s
		WHERE s.external_source IS NOT NULL AND s.external_id IS NOT NULL`
	rows, err := r.db.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("listing series with external source: %w", err)
	}
	defer rows.Close()

	var out []*models.Series
	for rows.Next() {
		var (
			pgID        pgtype.UUID
			pgLibraryID pgtype.UUID
			pgTotal     pgtype.Int4
			pgPubYear   pgtype.Int4
			genres      []string
			ser         models.Series
		)
		if err := rows.Scan(
			&pgID, &pgLibraryID, &ser.Name, &ser.Description,
			&pgTotal, &ser.Status, &ser.OriginalLanguage, &pgPubYear,
			&ser.Demographic, &genres, &ser.URL,
			&ser.ExternalID, &ser.ExternalSource,
			&ser.BookCount,
			&ser.CreatedAt, &ser.UpdatedAt,
		); err != nil {
			return nil, err
		}
		ser.ID = uuid.UUID(pgID.Bytes)
		ser.LibraryID = uuid.UUID(pgLibraryID.Bytes)
		if pgTotal.Valid {
			v := int(pgTotal.Int32)
			ser.TotalCount = &v
		}
		if pgPubYear.Valid {
			v := int(pgPubYear.Int32)
			ser.PublicationYear = &v
		}
		if genres != nil {
			ser.Genres = genres
		} else {
			ser.Genres = []string{}
		}
		out = append(out, &ser)
	}
	return out, rows.Err()
}
