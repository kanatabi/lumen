package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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

// buildVersion is stamped via -ldflags -X main.buildVersion in the image
// build (Dockerfile ARG VERSION); "dev" for local runs.
var buildVersion = "dev"

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	var src store.Source
	var engine service.Engine
	var eng *search.Service
	var qc *queue.Client
	var adminDeps *httpapi.AdminDeps
	if cfg.FixturePath != "" { // offline dev twin — logs say so, loudly
		m, err := store.Mem(cfg.FixturePath)
		if err != nil {
			log.Fatalf("fixture store: %v", err)
		}
		src = m
		engTwin, ok := m.(service.Engine)
		if !ok {
			log.Fatal("fixture twin must implement the engine seam")
		}
		engine = engTwin
		log.Printf("store+engine: IN-MEMORY twin %s (dev only — not a real lumen)", cfg.FixturePath)
	} else {
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
		src = pg

		// the engine is the read path (§4); queue+worker keep it a faithful
		// projection of truth. Pure-engine posture: absent config is a
		// start-up failure, not a silent fallback.
		if cfg.MeiliURL == "" {
			log.Fatal("LUMEN_MEILI_URL required: Meilisearch is the read path (§4 ruling) — fixture mode is the offline escape hatch")
		}
		eng = search.New(cfg.MeiliURL, cfg.MeiliKey)
		engine = eng
		bootCtx, bootCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer bootCancel()
		if err := eng.Ensure(bootCtx); err != nil {
			log.Fatalf("meili bootstrap: %v", err)
		}
		qc, err = queue.New(bootCtx, pool, pg, eng)
		if err != nil {
			log.Fatalf("queue: %v", err)
		}
		if err := qc.Start(bootCtx); err != nil {
			log.Fatalf("queue start: %v", err)
		}
		defer func() { _ = qc.Stop(context.Background()) }()
		adminDeps = &httpapi.AdminDeps{
			Pool:      pg,
			Queue:     qc,
			JWTSecret: cfg.SessionSecret,
			Limiter:   throttle.New(0), // per-replica in-memory backoff (documented)
		}
		if cfg.Media.Bucket != "" {
			ms, _, err := media.NewS3(bootCtx, media.Config{
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
			adminDeps.Media = ms
			adminDeps.MediaEndpoint, adminDeps.MediaBucket, adminDeps.MediaCDNBase =
				cfg.Media.Endpoint, cfg.Media.Bucket, cfg.Media.CDNBase
			if cfg.Media.CreateBucket {
				log.Printf("media: S3-compatible relay configured (bucket %s, dev create-bucket on)", cfg.Media.Bucket)
			} else {
				log.Printf("media: S3-compatible relay configured (bucket %s)", cfg.Media.Bucket)
			}
		} else {
			log.Print("media: not configured — POST /admin/photos answers 503 honestly (§5)")
		}
		if bao.Enabled() {
			bc, err := bao.New()
			if err != nil {
				log.Fatalf("bao client: %v", err)
			}
			adminDeps.Bao = bc
			log.Print("secrets plane: configured (§9 relay live)")
		} else {
			log.Print("secrets plane: not configured — /admin/bao absent (honest, §9)")
		}
		log.Print("store: postgres · engine: meilisearch · queue: river · admin: jwt sessions")
	}

	handler := httpapi.New(service.New(src, engine), eng, adminDeps).Router()
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("lumen listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx) // drain in-flight reads; the cluster restarts us on a schedule
	log.Print("lumen stopped")
}
