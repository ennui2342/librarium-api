// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

// @title           Librarium API
// @version         26.4.0
// @description     Backend API for Librarium, a privacy-focused tracker for your physical book, manga, and comic collection. Powers the self-hosted edition today; iOS Lite and Librarium Plus (hosted) are on the roadmap. All protected endpoints require a Bearer JWT (or `lbrm_pat_*` personal access token) in the Authorization header.
// @termsOfService  https://librarium.press

// @contact.name   Librarium
// @contact.url    https://librarium.press

// @license.name  AGPL-3.0
// @license.url   https://www.gnu.org/licenses/agpl-3.0.html

// @host      localhost:8080
// @BasePath  /api/v1

// @securityDefinitions.apikey  BearerAuth
// @in                          header
// @name                        Authorization
// @description                 Enter: Bearer {token}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	// Embed the IANA timezone database so the distroless runtime image can
	// resolve names like "America/Toronto" without a filesystem zoneinfo.
	_ "time/tzdata"

	"github.com/fireball1725/librarium-api/internal/ai"
	"github.com/fireball1725/librarium-api/internal/api"
	"github.com/fireball1725/librarium-api/internal/config"
	"github.com/fireball1725/librarium-api/internal/db"
	"github.com/fireball1725/librarium-api/internal/jobs"
	"github.com/fireball1725/librarium-api/internal/models"
	"github.com/fireball1725/librarium-api/internal/providers"
	bookProviders "github.com/fireball1725/librarium-api/internal/providers/books"
	mangaProviders "github.com/fireball1725/librarium-api/internal/providers/manga"
	"github.com/fireball1725/librarium-api/internal/repository"
	"github.com/fireball1725/librarium-api/internal/service"
	"github.com/fireball1725/librarium-api/internal/tui"
	"github.com/fireball1725/librarium-api/internal/version"
	"github.com/fireball1725/librarium-api/internal/workers"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

func main() {
	cfg := config.Load()

	// Refuse to start without a JWT signing secret. An empty secret would
	// cause every token to be signed with `[]byte("")`, which is trivially
	// forgeable — catastrophic auth bypass. Caller must set JWT_SECRET to a
	// cryptographically random value (≥32 bytes recommended).
	if cfg.JWTSecret == "" {
		fmt.Fprintln(os.Stderr, "FATAL: JWT_SECRET is required and must not be empty")
		os.Exit(1)
	}

	baseCtx, cancelBaseCtx := context.WithCancel(context.Background())
	defer cancelBaseCtx()

	// ── TUI or plain logger ───────────────────────────────────────────────────
	var collector *tui.Collector
	if cfg.TUI {
		collector = tui.NewCollector()
		tuiHandler := tui.NewSlogHandler(collector, cfg.LogLevel)
		slog.SetDefault(slog.New(tuiHandler))
	} else {
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level: cfg.LogLevel,
		})))
	}

	slog.Info("librarium-api", "version", version.Version)

	// ── Migrations ────────────────────────────────────────────────────────────
	slog.Info("running database migrations")
	if err := db.Migrate(cfg.DatabaseURL); err != nil {
		slog.Error("migration failed", "error", err)
		os.Exit(1)
	}

	// ── Database pool ─────────────────────────────────────────────────────────
	pool, err := db.Connect(baseCtx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("database connection failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	slog.Info("database connected")

	// ── River migrations ──────────────────────────────────────────────────────
	slog.Info("running river migrations")
	riverMigrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		slog.Error("river migrator creation failed", "error", err)
		os.Exit(1)
	}
	if _, err := riverMigrator.Migrate(baseCtx, rivermigrate.DirectionUp, &rivermigrate.MigrateOpts{}); err != nil {
		slog.Error("river migration failed", "error", err)
		os.Exit(1)
	}

	// ── One-time backfills after migrations ───────────────────────────────────
	// Contributor sort_name was added in migration 000003 with a default of ''.
	// Derive and populate it for existing rows so sort-by-author works immediately.
	if err := backfillContributorSortNames(baseCtx, repository.NewContributorRepo(pool)); err != nil {
		slog.Warn("contributor sort_name backfill failed", "error", err)
	}

	// ── River workers ─────────────────────────────────────────────────────────
	// Build a standalone provider service for the worker (same config, separate instance).
	settingsRepo := repository.NewSettingsRepo(pool)
	registry := providers.NewRegistry()
	registry.Register(bookProviders.NewTestProvider())
	registry.Register(bookProviders.NewOpenLibraryProvider())
	registry.Register(bookProviders.NewGoogleBooksProvider())
	registry.Register(bookProviders.NewISBNdbProvider())
	registry.Register(bookProviders.NewHardcoverProvider())
	registry.Register(bookProviders.NewISFDBProvider())
	registry.Register(mangaProviders.NewMangaDexProvider())
	providerSvc := service.NewProviderService(registry, settingsRepo, repository.NewBookRepo(pool), repository.NewEditionRepo(pool))
	if err := providerSvc.LoadAll(baseCtx); err != nil {
		slog.Warn("failed to load provider settings", "error", err)
	}

	// AI providers: Anthropic, OpenAI, Ollama, and Osaurus. Exactly one is
	// active at a time; the admin picks via the Connections page.
	aiRegistry := ai.NewRegistry()
	aiRegistry.Register(ai.NewAnthropicProvider())
	aiRegistry.Register(ai.NewOpenAIProvider())
	aiRegistry.Register(ai.NewOllamaProvider())
	aiRegistry.Register(ai.NewOsaurusProvider())
	aiSvc := service.NewAIService(aiRegistry, settingsRepo)
	if err := aiSvc.LoadAll(baseCtx); err != nil {
		slog.Warn("failed to load AI provider settings", "error", err)
	}
	aiUserSvc := service.NewAIUserService(repository.NewUserAISettingsRepo(pool))
	jobSvc := service.NewJobService(settingsRepo)
	aiSuggestionsRepo := repository.NewAISuggestionsRepo(pool)
	// workerBookSvc is constructed immediately below; the suggestions worker
	// needs it to fetch floating-book covers after creation. Build workerBookSvc
	// first so we can pass it through.

	workerBookSvc := service.NewBookService(
		pool,
		repository.NewBookRepo(pool),
		repository.NewLibraryBookRepo(pool),
		repository.NewContributorRepo(pool),
		repository.NewEditionRepo(pool),
		repository.NewTagRepo(pool),
		repository.NewGenreRepo(pool),
		repository.NewCoverRepo(pool),
		aiSuggestionsRepo,
		cfg.CoverStoragePath,
	)
	jobRepo := repository.NewJobRepo(pool)
	suggestionsSvc := service.NewSuggestionsService(
		pool,
		aiSuggestionsRepo,
		jobRepo,
		repository.NewBookRepo(pool),
		repository.NewEditionRepo(pool),
		workerBookSvc,
		aiRegistry, aiSvc, jobSvc, aiUserSvc, providerSvc,
	)
	importWorker := workers.NewImportWorker(
		pool,
		repository.NewImportJobRepo(pool),
		repository.NewBookRepo(pool),
		repository.NewLibraryBookRepo(pool),
		repository.NewContributorRepo(pool),
		repository.NewEditionRepo(pool),
		repository.NewTagRepo(pool),
		repository.NewGenreRepo(pool),
		repository.NewEnrichmentBatchRepo(pool),
		nil, // riverClient set below after construction
	)
	metadataWorker := workers.NewMetadataWorker(
		repository.NewBookRepo(pool),
		repository.NewContributorRepo(pool),
		repository.NewEditionRepo(pool),
		repository.NewGenreRepo(pool),
		providerSvc,
		workerBookSvc,
	)
	aiMetadataRepo := repository.NewAIMetadataRepo(pool)
	aiMetadataSvc := service.NewAIMetadataService(aiRegistry, aiMetadataRepo)
	enrichmentBatchWorker := workers.NewEnrichmentBatchWorker(
		repository.NewEnrichmentBatchRepo(pool),
		repository.NewBookRepo(pool),
		metadataWorker,
		aiMetadataSvc,
	)
	aiSuggestionsWorker := workers.NewAISuggestionsWorker(suggestionsSvc)

	riverWorkers := river.NewWorkers()
	river.AddWorker(riverWorkers, importWorker)
	river.AddWorker(riverWorkers, metadataWorker)
	river.AddWorker(riverWorkers, enrichmentBatchWorker)
	river.AddWorker(riverWorkers, aiSuggestionsWorker)

	riverClient, err := river.NewClient[pgx.Tx](riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 5},
		},
		Workers: riverWorkers,
	})
	if err != nil {
		slog.Error("river client creation failed", "error", err)
		os.Exit(1)
	}
	importWorker.SetRiverClient(riverClient)

	if err := riverClient.Start(baseCtx); err != nil {
		slog.Error("river client start failed", "error", err)
		os.Exit(1)
	}
	slog.Info("river worker started")

	// ── Unified job scheduler ────────────────────────────────────────────────
	// Walks job_schedules on a 30s tick and fires each kind's Enqueue hook
	// when its cron expression is due. Kinds register their Enqueue below.

	// ── HTTP server ───────────────────────────────────────────────────────────
	addr := cfg.Host + ":" + cfg.Port
	// Only pass the collector when TUI is active — passing a nil *Collector as
	// the MetricsCollector interface produces a non-nil interface value, which
	// bypasses the nil check in the logger middleware and causes a panic.
	var metrics api.MetricsCollector
	if collector != nil {
		metrics = collector
	}
	jobRegistry := jobs.NewRegistry()
	// AI suggestions is the first scheduled kind. Its Enqueue replicates
	// the previous per-user-fanout behaviour: walk opted-in users, skip
	// any whose last run is inside the cadence window, enqueue a River
	// job for everyone else.
	jobRegistry.Register(&jobs.Definition{
		Kind:        jobs.KindAISuggestions,
		DisplayName: "AI suggestions",
		Description: "Generates per-user book suggestions using the active AI provider.",
		Schedulable: true,
		DefaultCron: "0 3 * * *",
		Enqueue: func(ctx context.Context, trig jobs.TriggerCtx, _ json.RawMessage) error {
			cfg, err := jobSvc.GetAISuggestionsConfig(ctx)
			if err != nil {
				return fmt.Errorf("load ai-suggestions config: %w", err)
			}
			if !cfg.Enabled {
				return nil
			}
			users, err := aiSuggestionsRepo.ListOptedInUsers(ctx)
			if err != nil {
				return fmt.Errorf("list opted-in users: %w", err)
			}
			cutoff := time.Now().Add(-time.Duration(cfg.IntervalMinutes) * time.Minute)
			triggeredBy := string(trig.TriggeredBy)
			if triggeredBy == "" {
				triggeredBy = "scheduler"
			}
			// Admin-triggered Run Now is an explicit request — bypass the
			// per-user cadence cooldown so a user whose last run is still
			// inside the window still gets enqueued. The cooldown exists
			// to keep the scheduler tick from re-running the same users.
			bypassCooldown := triggeredBy == string(models.JobTriggeredByAdmin)
			for _, u := range users {
				if !bypassCooldown {
					last, err := aiSuggestionsRepo.LastRunAt(ctx, u.UserID)
					if err != nil {
						slog.Warn("ai scheduler: last-run lookup failed", "user_id", u.UserID, "error", err)
						continue
					}
					if !last.IsZero() && last.After(cutoff) {
						continue
					}
				}
				if _, err := riverClient.Insert(ctx,
					models.AISuggestionsJobArgs{UserID: u.UserID, TriggeredBy: triggeredBy}, nil); err != nil {
					slog.Warn("ai scheduler: enqueue failed", "user_id", u.UserID, "error", err)
				}
			}
			return nil
		},
	})
	// Import and enrichment are run from user actions (no scheduled
	// fanout yet), but register them for display on the jobs admin page.
	jobRegistry.Register(&jobs.Definition{
		Kind:        jobs.KindImport,
		DisplayName: "CSV import",
		Description: "Bulk imports rows from a CSV into the user's library.",
		Schedulable: false,
	})
	jobRegistry.Register(&jobs.Definition{
		Kind:        jobs.KindEnrichment,
		DisplayName: "Metadata enrichment",
		Description: "Fetches missing metadata / covers from providers for a batch of books.",
		Schedulable: false,
	})
	// Cover backfill — first scheduled kind shipped through the unified
	// framework. Walks the catalog for books with no primary cover and
	// queues cover-only enrichment batches for them, chunked to avoid
	// one giant job. Non-fatal per-chunk — a single failing ISBN lookup
	// shouldn't stall the sweep.
	const coverBackfillChunk = 50
	coverBookRepo := repository.NewBookRepo(pool)
	coverEnrichmentRepo := repository.NewEnrichmentBatchRepo(pool)
	jobRegistry.Register(&jobs.Definition{
		Kind:        jobs.KindCoverBackfill,
		DisplayName: "Cover backfill",
		Description: "Finds books without a cover image and queues cover-only metadata enrichment for them.",
		Schedulable: true,
		DefaultCron: "0 3 * * *",
		Enqueue: func(ctx context.Context, trig jobs.TriggerCtx, _ json.RawMessage) error {
			// Pick an owner for the enrichment batches. Admin Run-Now sets
			// trig.CreatedBy; cron-triggered runs don't, so we fall back
			// to the first instance admin (the enrichment_batches.created_by
			// column is NOT NULL today — relaxing that is a separate
			// change).
			owner := uuid.Nil
			if trig.CreatedBy != nil {
				owner = *trig.CreatedBy
			} else {
				if err := pool.QueryRow(ctx,
					`SELECT id FROM users WHERE is_instance_admin = true ORDER BY created_at LIMIT 1`,
				).Scan(&owner); err != nil {
					slog.Warn("cover backfill: no admin user to attribute to", "error", err)
					return err
				}
			}
			ids, err := coverBookRepo.ListBooksMissingCover(ctx, 1000)
			if err != nil {
				return fmt.Errorf("list books missing cover: %w", err)
			}
			// Stamp the parent umbrella with the total book count so history
			// shows a real number instead of an empty progress blob. The
			// parent job's "work" is enumerate + dispatch, so we count it
			// as processed once the enqueue loop completes successfully.
			if progressJSON, perr := json.Marshal(map[string]any{
				"processed": len(ids),
				"total":     len(ids),
			}); perr == nil {
				_ = jobRepo.UpdateProgress(ctx, trig.JobID, progressJSON)
			}
			if len(ids) == 0 {
				slog.Info("cover backfill: no books missing covers")
				return nil
			}
			// Bulk-lookup titles once up-front so each item row carries a
			// human-readable label, matching the manual "Search covers" path.
			titles, _ := coverBookRepo.TitlesByIDs(ctx, ids)
			for i := 0; i < len(ids); i += coverBackfillChunk {
				end := i + coverBackfillChunk
				if end > len(ids) {
					end = len(ids)
				}
				chunk := ids[i:end]
				batch := &models.EnrichmentBatch{
					ID:         uuid.New(),
					LibraryID:  nil, // catalog-wide — not scoped to a library
					CreatedBy:  owner,
					Type:       models.EnrichmentBatchTypeCover,
					Force:      false,
					Status:     models.EnrichmentBatchPending,
					BookIDs:    chunk,
					TotalBooks: len(chunk),
				}
				if err := coverEnrichmentRepo.Create(ctx, batch); err != nil {
					slog.Warn("cover backfill: creating batch failed", "error", err)
					continue
				}
				items := make([]models.EnrichmentBatchItem, 0, len(chunk))
				for _, id := range chunk {
					bookID := id
					items = append(items, models.EnrichmentBatchItem{
						ID:        uuid.New(),
						BatchID:   batch.ID,
						BookID:    &bookID,
						BookTitle: titles[bookID],
						Status:    models.EnrichmentItemPending,
					})
				}
				if err := coverEnrichmentRepo.CreateItems(ctx, items); err != nil {
					slog.Warn("cover backfill: creating items failed", "batch_id", batch.ID, "error", err)
					continue
				}
				if _, err := riverClient.Insert(ctx,
					models.EnrichmentBatchJobArgs{BatchID: batch.ID}, nil); err != nil {
					slog.Warn("cover backfill: enqueue failed", "batch_id", batch.ID, "error", err)
				}
			}
			slog.Info("cover backfill: enqueued", "books", len(ids))
			return nil
		},
	})

	// Kick the scheduler off after registry is populated.
	jobRepoForSched := repository.NewJobRepo(pool)
	// Seed default schedule rows for any schedulable kind that doesn't
	// have one yet. Ensures new kinds (e.g. cover_backfill) show up in
	// the admin UI on first boot after upgrade without a custom
	// migration per kind — the Definition itself carries the default
	// cron, and the seed is idempotent via the kind UNIQUE constraint.
	for _, def := range jobRegistry.All() {
		if !def.Schedulable {
			continue
		}
		if _, err := jobRepoForSched.GetSchedule(baseCtx, string(def.Kind)); err == nil {
			continue // already there
		}
		cron := def.DefaultCron
		if cron == "" {
			cron = "0 3 * * *"
		}
		if err := jobRepoForSched.UpsertSchedule(baseCtx, &models.JobSchedule{
			Kind:    string(def.Kind),
			Cron:    cron,
			Enabled: false, // admin flips this on from the UI
		}); err != nil {
			slog.Warn("seeding schedule failed", "kind", def.Kind, "error", err)
		}
	}
	scheduler := jobs.NewScheduler(jobRegistry, jobRepoForSched)
	go scheduler.Run(baseCtx)

	handler := api.NewRouter(baseCtx, pool, cfg, riverClient, metrics, api.RouterDeps{
		JobRegistry: jobRegistry,
		AISvc:       aiSvc,
		ProviderSvc: providerSvc,
	})
	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
		// Cap request headers at 1 MiB so a malformed client can't pin a
		// goroutine on an unbounded header read. Per-handler limits cover
		// bodies (multipart, JSON); this catches everything else.
		MaxHeaderBytes: 1 << 20,
	}
	if collector != nil {
		srv.ConnState = func(_ net.Conn, state http.ConnState) {
			switch state {
			case http.StateNew:
				collector.TrackConn(+1)
			case http.StateClosed, http.StateHijacked:
				collector.TrackConn(-1)
			}
		}
	}

	go func() {
		slog.Info("starting librarium-api", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// ── TUI or wait for signal ────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	if cfg.TUI && collector != nil {
		// Poll the import job queue every 2 seconds and push stats to the TUI.
		go func() {
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					collector.UpdateQueueStats(pollQueueStats(baseCtx, pool))
				case <-baseCtx.Done():
					return
				}
			}
		}()
		tui.Run(collector, quit)
	} else {
		<-quit
	}

	// ── Graceful shutdown ─────────────────────────────────────────────────────
	slog.Info("shutting down")
	cancelBaseCtx()

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutCancel()

	if err := riverClient.StopAndCancel(shutCtx); err != nil {
		slog.Warn("river stop error", "error", err)
	}
	if err := srv.Shutdown(shutCtx); err != nil {
		slog.Error("forced shutdown", "error", err)
	}
	slog.Info("stopped")
}

// pollQueueStats queries both import_jobs and enrichment_batches for a queue snapshot.
func pollQueueStats(ctx context.Context, pool *pgxpool.Pool) tui.QueueStats {
	var s tui.QueueStats

	// Count by status across both job tables.
	countRows, err := pool.Query(ctx, `
		SELECT status, COUNT(*) FROM import_jobs      GROUP BY status
		UNION ALL
		SELECT status, COUNT(*) FROM enrichment_batches GROUP BY status`)
	if err != nil {
		return s
	}
	defer countRows.Close()
	for countRows.Next() {
		var status string
		var n int
		if err := countRows.Scan(&status, &n); err != nil {
			continue
		}
		switch status {
		case "pending":
			s.Pending += n
		case "processing":
			s.Processing += n
		case "done":
			s.Done += n
		case "failed":
			s.Failed += n
		}
	}

	// Active jobs from both tables, mapped to a common shape for display.
	activeRows, err := pool.Query(ctx, `
		SELECT ij.id::text, l.name,
		       ij.status, ij.total_rows     AS total, ij.processed_rows     AS processed,
		       ij.failed_rows, ij.skipped_rows, ij.updated_at
		FROM   import_jobs ij
		JOIN   libraries l ON l.id = ij.library_id
		WHERE  ij.status IN ('pending', 'processing')
		UNION ALL
		SELECT eb.id::text, l.name,
		       eb.status, eb.total_books AS total, eb.processed_books AS processed,
		       eb.failed_books, 0 AS skipped_rows, eb.updated_at
		FROM   enrichment_batches eb
		JOIN   libraries l ON l.id = eb.library_id
		WHERE  eb.status IN ('pending', 'processing')
		ORDER  BY updated_at DESC
		LIMIT  5`)
	if err != nil {
		return s
	}
	defer activeRows.Close()
	for activeRows.Next() {
		var info tui.ActiveJobInfo
		var rawID string
		if err := activeRows.Scan(
			&rawID, &info.LibraryName,
			&info.Status, &info.TotalRows, &info.ProcessedRows,
			&info.FailedRows, &info.SkippedRows, &info.UpdatedAt,
		); err != nil {
			continue
		}
		if len(rawID) >= 8 {
			info.ID = rawID[:8]
		} else {
			info.ID = rawID
		}
		s.Active = append(s.Active, info)
	}
	return s
}

// backfillContributorSortNames derives and persists sort_name for contributors
// that don't have one. Safe to run on every startup — it's a no-op once every
// row is populated.
func backfillContributorSortNames(ctx context.Context, repo *repository.ContributorRepo) error {
	missing, err := repo.ListMissingSortName(ctx)
	if err != nil {
		return err
	}
	if len(missing) == 0 {
		return nil
	}
	slog.Info("backfilling contributor sort_name", "count", len(missing))
	for _, c := range missing {
		var sortName string
		if c.IsCorporate {
			sortName = c.Name
		} else {
			sortName = service.DeriveSortName(c.Name)
		}
		if err := repo.SetSortName(ctx, c.ID, sortName); err != nil {
			slog.Warn("backfill sort_name failed", "contributor_id", c.ID, "name", c.Name, "error", err)
		}
	}
	return nil
}
