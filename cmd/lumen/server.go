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
	"github.com/kanatabi/lumen/internal/httpapi"
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
	adminDeps := buildAdminDeps(deps)
	handler := httpapi.New(service.New(deps.store, deps.engine), deps.engine, adminDeps).Router()
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

// buildAdminDeps wires the §8/§9/§5 surfaces (HTTP roles only).
func buildAdminDeps(deps *dependencies) *httpapi.AdminDeps {
	cfg := deps.config
	adminDeps := &httpapi.AdminDeps{
		Pool:      deps.store,
		Queue:     deps.queueClient,
		JWTSecret: cfg.SessionSecret,
		Limiter:   throttle.New(0), // per-replica in-memory backoff (documented)
	}

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

		adminDeps.Media = mediaStore
		adminDeps.MediaEndpoint = cfg.Media.Endpoint
		adminDeps.MediaBucket = cfg.Media.Bucket
		adminDeps.MediaCDNBase = cfg.Media.CDNBase

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
