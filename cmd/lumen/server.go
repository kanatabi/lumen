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
	"github.com/kanatabi/lumen/internal/authz"
	"github.com/kanatabi/lumen/internal/bao"
	"github.com/kanatabi/lumen/internal/config"
	"github.com/kanatabi/lumen/internal/media"
	"github.com/kanatabi/lumen/internal/role"
	"github.com/kanatabi/lumen/internal/service"
	"github.com/kanatabi/lumen/internal/throttle"
)

// serverCommand: the HTTP process, surfaces classified by LUMEN_ROLE —
// admin = the §8 control plane only; cms = §1 + the content desks;
// unset/all = everything (the legacy dev plane, today's behavior).
func serverCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "server",
		Short: "serve HTTP surfaces for LUMEN_ROLE (admin | cms | all=legacy dev)",
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
// The COMPOSITION ROOT is where the role becomes concrete: which services
// exist, which mounts they get, and what honest absence looks like.
func startHTTP(deps *dependencies) *http.Server {
	cfg := deps.config
	planes := cfg.Role.Planes()

	pub := public.New(service.New(deps.store, deps.engineForService()), deps.engine)
	pub.EnableDBReadiness(deps.ping)

	var adm *admin.Handler
	switch cfg.Role {
	case role.Admin:
		adm = buildControlPlane(deps)
	case role.CMS:
		adm = buildContentPlane(deps)
	default: // role.All — the legacy dev plane: every surface, local auth
		adm = buildAllPlane(deps)
	}

	handler := api.NewRouter(pub, adm, planes)
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("server: HTTP listening on %s (role=%s)", srv.Addr, cfg.Role)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()
	return srv
}

// buildControlPlane wires lumen-admin: the mint, the wizard, verify,
// users, the §9 relay. NO content routes, NO queue, NO engine — its
// database does not contain those tables, so a stray mount would answer
// 500s, which is exactly what honest absence prevents.
func buildControlPlane(deps *dependencies) *admin.Handler {
	cfg := deps.config
	svc := adminsvc.New(deps.store, nil, nil, cfg.SessionSecret, throttle.New(0))
	if bao.Enabled() {
		baoClient, err := bao.New()
		if err != nil {
			log.Fatalf("bao client: %v", err)
		}
		svc.EnableBao(baoClient)
		log.Print("secrets plane: configured (§9 relay live — admin plane)")
	} else {
		log.Print("secrets plane: not configured — /admin/bao absent (honest, §9)")
	}
	return admin.New(svc)
}

// buildContentPlane wires lumen-staging/production: the desks with the
// media relay and the queue, and REMOTE authz — every desk bearer is
// introspected against LUMEN_ADMIN_URL (cached ~30 s, fail closed; the
// trust note lives in internal/authz). The signing secret is not even
// bound for this role (config layer), so a pod that leaks it cannot leak
// this process.
func buildContentPlane(deps *dependencies) *admin.Handler {
	cfg := deps.config
	svc := adminsvc.New(deps.store, deps.queueClient, deps.engine, "", throttle.New(0))
	enableMedia(svc, cfg)
	intro := authz.NewRemote(cfg.AdminURL)
	log.Printf("authz: bearer introspection against %s (cache 30s, fail closed)", cfg.AdminURL)
	return admin.NewCMS(svc, intro)
}

// buildAllPlane is the legacy dev wiring — both planes in one process,
// local verification everywhere (desk middleware falls back to it when
// no introspector is configured).
func buildAllPlane(deps *dependencies) *admin.Handler {
	cfg := deps.config
	adminSvc := adminsvc.New(deps.store, deps.queueClient, deps.engine,
		cfg.SessionSecret, throttle.New(0)) // per-replica in-memory backoff (documented)
	enableMedia(adminSvc, cfg)

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

func enableMedia(svc *adminsvc.Service, cfg config.Config) {
	if cfg.Media.Bucket == "" {
		log.Print("media: not configured — POST /admin/photos answers 503 honestly (§5)")
		return
	}
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
	svc.EnableMedia(mediaStore, mediaCfg.Endpoint, mediaCfg.Bucket, mediaCfg.CDNBase)
	svc.SetMediaPrefix(cfg.Media.Prefix) // applied once at the upload site (§5 coherence)
	log.Printf("media: S3-compatible relay configured (bucket %s, prefix %q, dev create-bucket: %v)",
		cfg.Media.Bucket, cfg.Media.Prefix, cfg.Media.CreateBucket)
}
