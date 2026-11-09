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

	"github.com/kanatabi/lumen/internal/adminsvc"
	"github.com/kanatabi/lumen/internal/api"
	"github.com/kanatabi/lumen/internal/api/admin"
	"github.com/kanatabi/lumen/internal/api/public"
	"github.com/kanatabi/lumen/internal/bao"
	"github.com/kanatabi/lumen/internal/media"
	"github.com/kanatabi/lumen/internal/service"
	"github.com/kanatabi/lumen/internal/throttle"
)

// serverCommand: the §1/§8/§9 HTTP process. It ENQUEUES index jobs inside
// write transactions but never consumes them — that is the worker's scope.
func serverCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "server",
		Short: "serve HTTP only (§1 + §8 + §9); no queue consumption",
		RunE:  func(*cobra.Command, []string) error { return runServer() },
	}
}

func runServer() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	deps, err := initDependencies(ctx)
	if err != nil {
		return err
	}
	defer deps.Close()
	if err := deps.config.ValidateServer(); err != nil {
		return err
	}
	srv := startHTTP(deps)

	// Wait for shutdown signal
	<-ctx.Done()
	log.Print("server: shutdown signal — draining")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("server: shutdown: %v", err)
	}

	return nil
}

// startHTTP is the reusable HTTP lifecycle primitive (runAll composes it
// with the worker's) — build, attach, listen in background; caller Shutdowns.
func startHTTP(deps *dependencies) *http.Server {
	adminHandler := buildAdmin(deps)
	handler := api.NewRouter(
		public.New(service.New(deps.store, deps.engine), deps.engine),
		adminHandler,
	)
	srv := &http.Server{
		Addr:              ":" + deps.config.Port,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("server: HTTP listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()
	return srv
}

// buildAdmin wires the admin business service + its HTTP handler. The
// composition root is the ONLY place that knows which subsystems exist:
// media/bao absence are logged, never faked; no session secret is checked
// here (ValidateServer did) and no store call reaches past adminsvc.
func buildAdmin(deps *dependencies) *admin.Handler {
	cfg := deps.config
	adminSvc := adminsvc.New(deps.store, deps.queueClient, deps.engine,
		cfg.SessionSecret, throttle.New(0)) // per-replica in-memory backoff (documented)
	if cfg.Media.Bucket != "" {
		mediaCfg := media.Config{
			Endpoint:     cfg.Media.Endpoint,
			Region:       cfg.Media.Region,
			Bucket:       cfg.Media.Bucket,
			CDNBase:      cfg.Media.CDNBase,
			AccessKey:    cfg.Media.AccessKey,
			SecretKey:    cfg.Media.SecretKey,
			ForcePath:    cfg.Media.ForcePath,
			CreateBucket: cfg.Media.CreateBucket,
		}

		mediaStore, _, err := media.NewS3(context.Background(), mediaCfg)
		if err != nil {
			log.Fatalf("media store: %v", err)
		}

		adminSvc.EnableMedia(mediaStore, mediaCfg.Endpoint, mediaCfg.Bucket, mediaCfg.CDNBase)
		log.Printf("media: S3-compatible relay configured (bucket %s, dev create-bucket: %v)", cfg.Media.Bucket, cfg.Media.CreateBucket)
	} else {
		log.Print("media: not configured — POST /admin/photos answers 503 honestly (§5)")
	}

	if bao.Enabled() {
		baoClient, err := bao.New()
		if err != nil {
			log.Fatalf("bao client: %v", err)
		}

		adminSvc.EnableBao(baoClient)
		log.Print("secrets plane: configured (§9 relay live)")
	} else {
		log.Print("secrets plane: not configured — /admin/bao absent (honest, §9)")
	}

	return admin.New(adminSvc)
}
