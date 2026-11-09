package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/kanatabi/lumen/internal/bao"
	"github.com/kanatabi/lumen/internal/config"
	"github.com/kanatabi/lumen/internal/httpapi"
	"github.com/kanatabi/lumen/internal/media"
	"github.com/kanatabi/lumen/internal/queue"
	"github.com/kanatabi/lumen/internal/search"
	"github.com/kanatabi/lumen/internal/service"
	"github.com/kanatabi/lumen/internal/store"
	"github.com/kanatabi/lumen/internal/throttle"
)

// roleCommand builds server/worker/all from one wiring — the composition
// root stays honest: same config, same deps, toggled by role.
func roleCommand(use, short string, serveHTTP, serveWorkers bool) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(*cobra.Command, []string) error {
			return runRole(serveHTTP, serveWorkers)
		},
	}
}

func runRole(serveHTTP, serveWorkers bool) error {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if serveHTTP {
		if err := cfg.ValidateServer(); err != nil {
			log.Fatalf("config: %v", err)
		}
	}

	pool, err := store.NewPool(context.Background(), cfg.DBURL)
	if err != nil {
		log.Fatalf("db pool: %v", err)
	}
	defer pool.Close()
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		log.Fatalf("db ping: %v", err)
	}
	pg := store.NewPG(pool)

	// BOTH roles need the engine (server reads through it, worker writes it)
	if cfg.MeiliURL == "" {
		log.Fatal("LUMEN_MEILI_URL required: Meilisearch is the read path (§4 ruling)")
	}
	searchEngine := search.New(cfg.MeiliURL, cfg.MeiliKey)
	bootCtx, bootCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer bootCancel()
	if err := searchEngine.Ensure(bootCtx); err != nil {
		log.Fatalf("meili bootstrap: %v", err)
	}
	queueClient, err := queue.New(bootCtx, pool, pg, searchEngine)
	if err != nil {
		log.Fatalf("queue: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if serveWorkers {
		if err := queueClient.Start(ctx); err != nil {
			log.Fatalf("queue start: %v", err)
		}
		log.Print("role: queue worker consuming")
	}

	var srv *http.Server
	if serveHTTP {
		adminDeps := buildAdminDeps(cfg, pg, queueClient)
		handler := httpapi.New(service.New(pg, searchEngine), searchEngine, adminDeps).Router()
		srv = &http.Server{
			Addr:              ":" + cfg.Port,
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			log.Printf("role: HTTP listening on %s", srv.Addr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("http: %v", err)
			}
		}()
	}

	<-ctx.Done()
	log.Print("shutdown signal — draining")
	if srv != nil {
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelShutdown()
		_ = srv.Shutdown(shutdownCtx)
	}
	if serveWorkers {
		stopCtx, cancelStop := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelStop()
		if err := queueClient.Stop(stopCtx); err != nil {
			log.Printf("queue stop: %v (leader lease will age out)", err)
		}
	}
	return nil
}

// buildAdminDeps wires the §8/§9/§5 surfaces (HTTP roles only).
func buildAdminDeps(cfg config.Config, pg *store.PG, queueClient *queue.Client) *httpapi.AdminDeps {
	adminDeps := &httpapi.AdminDeps{
		Pool:      pg,
		Queue:     queueClient,
		JWTSecret: cfg.SessionSecret,
		Limiter:   throttle.New(0), // per-replica in-memory backoff (documented)
	}
	if cfg.Media.Bucket != "" {
		mediaStore, _, err := media.NewS3(context.Background(), media.Config{
			Endpoint:     cfg.Media.Endpoint,
			Region:       cfg.Media.Region,
			Bucket:       cfg.Media.Bucket,
			CDNBase:      cfg.Media.CDNBase,
			AccessKey:    cfg.Media.AccessKey,
			SecretKey:    cfg.Media.SecretKey,
			ForcePath:    cfg.Media.ForcePath,
			CreateBucket: cfg.Media.CreateBucket,
		})
		if err != nil {
			log.Fatalf("media store: %v", err)
		}
		adminDeps.Media = mediaStore
		adminDeps.MediaEndpoint, adminDeps.MediaBucket, adminDeps.MediaCDNBase =
			cfg.Media.Endpoint, cfg.Media.Bucket, cfg.Media.CDNBase
		log.Printf("media: S3-compatible relay configured (bucket %s, dev create-bucket: %v)", cfg.Media.Bucket, cfg.Media.CreateBucket)
	} else {
		log.Print("media: not configured — POST /admin/photos answers 503 honestly (§5)")
	}
	if bao.Enabled() {
		baoClient, err := bao.New()
		if err != nil {
			log.Fatalf("bao client: %v", err)
		}
		adminDeps.Bao = baoClient
		log.Print("secrets plane: configured (§9 relay live)")
	} else {
		log.Print("secrets plane: not configured — /admin/bao absent (honest, §9)")
	}
	return adminDeps
}
