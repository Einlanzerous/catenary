# syntax=docker/dockerfile:1

# Production image: one self-contained static Go binary, with the web client
# embedded in it. Three stages: Node builds web/, Go builds the binary with that
# build inside it, and the runtime stage carries the binary alone.
# Build from the repo root:
#   docker build -f deploy/Dockerfile -t catenary:latest .

# 1) Build the web client (CANT-241). Pinned to the Node major ci.yml pins
#    (`node-version: "24"`), on the Go stage's Debian release.
#
#    `vite build` ALONE, not `npm run build`. That script's first step,
#    gen:check, byte-compares the generated Dart file too, and .dockerignore
#    keeps dart/ out of this context — it would report a stale file that is
#    merely absent. gen:check and vue-tsc already gate every pull request in
#    verify.sh; the image is not where types are checked.
#
#    Vite writes to /app/internal/webui/static/dist (web/vite.config.ts), the
#    directory internal/webui embeds. .dockerignore excludes any copy of it
#    built locally, so this stage is the image's only source of the client.
FROM node:24-bookworm-slim AS web
WORKDIR /app/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npx --no-install vite build

# 2) Build the static binary. Pure Go (pgx, net/http) => CGO disabled, which is
#    what "single static Go binary" in CLAUDE.md actually means. The client from
#    stage 1 lands beside internal/webui's committed placeholder, and the embed
#    carries it into the binary.
FROM golang:1.26-bookworm AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /app/internal/webui/static/dist ./internal/webui/static/dist

# Build identity. Both default to EMPTY rather than to a placeholder: an image
# built outside the publish workflow must not be able to claim a release
# version. cmd/catenary's buildVersion() maps empty back to "dev".
ARG VERSION=
ARG GIT_SHA=
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w \
      -X main.version=${VERSION} \
      -X main.commit=${GIT_SHA}" \
    -o /out/catenary ./cmd/catenary

# 3) Runtime: slim Alpine with ca-certificates, and busybox `wget` for the
#    compose healthcheck.
#
#    ALPINE RATHER THAN scratch/distroless, and that is a decision rather than
#    a default. CANT-16 records that the health-check and the image have to be
#    chosen together: a wget-based check against a scratch image can never pass,
#    the container stays `unhealthy` for ever, and anything gating on
#    `service_healthy` stalls behind it. The alternatives were a `catenary
#    healthcheck` subcommand or this. This matches Chronicle and keeps the
#    compose block identical in shape to every other Go service on the box.
#
#    Pinned to a SUPPORTED Alpine. 3.20 is past its two-year window, so
#    `apk add` there pulls from a branch no longer getting security updates —
#    on a public image. (Chronicle still pins 3.20; flagged there separately.)
FROM alpine:3.23 AS runtime
RUN apk add --no-cache ca-certificates
COPY --from=build /out/catenary /usr/local/bin/catenary

# Unprivileged. 4012 is above 1024 and the process writes nothing to disk —
# logs go to stdout, and the migrations and the web client are embedded — so
# root buys nothing here. Still one COPY: the binary is the whole service.
USER nobody

ENV CATENARY_PORT=4012
EXPOSE 4012
ENTRYPOINT ["catenary"]
CMD ["serve"]
