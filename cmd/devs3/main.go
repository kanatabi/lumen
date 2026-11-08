// cmd/devs3 is the LOCAL-DEV S3-protocol endpoint (gofakes3, in-memory).
// It exists because the usual suspects turned hostile to zero-config dev:
// MinIO withdrew public images, LocalStack v4+ requires an auth token.
// The protocol is what matters — the relay speaks real SigV4 S3 here, and
// will speak it identically to the (HUMAN-GATE #9) production provider.
// In-memory by design: a dev stand-in that accumulates state is just a
// second database nobody backed up.
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

func main() {
	addr := os.Getenv("LUMEN_MEDIA_DEV_ADDR")
	if addr == "" {
		addr = ":4566"
	}
	backend := s3mem.New()
	if bucket := os.Getenv("LUMEN_MEDIA_BUCKET"); bucket != "" {
		if err := backend.CreateBucket(bucket); err != nil {
			log.Fatal(err)
		}
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           gofakes3.New(backend).Server(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("devs3 (gofakes3, in-memory) on %s — DEV ONLY, nothing persists", addr)
	log.Fatal(srv.ListenAndServe())
}
