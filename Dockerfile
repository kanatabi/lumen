# lumen — Go read+admin API container.
# Build artifacts are self-contained (no runtime deps beyond libc-free binary),
# so the runtime stage is a static scratch layer running non-root.
# Image NAME stays `lumen` for the CI `kustomize edit set image` hook
# (vortex doctrine); tags deploy as sha-<short> — CI pipeline itself is
# INFRA Phase 8, this file is the thing that gets built.

FROM docker.io/library/golang:1.26 AS build
WORKDIR /src
# pinned module cache first (layer reuse), then the sources
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO off → static binary → scratch-safe; version stamp = commit via ldflags
# (CI passes -build-arg; default keeps local builds working)
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.buildVersion=${VERSION}" -o /out/lumen ./cmd/lumen

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/lumen /usr/local/bin/lumen
ENV PORT=8080
EXPOSE 8080
# no service account baked here; the cluster decides user via securityContext
# (vortex doctrine), and the DB/engine/Bao endpoints arrive as ESO-rendered env
ENTRYPOINT ["/usr/local/bin/lumen"]
