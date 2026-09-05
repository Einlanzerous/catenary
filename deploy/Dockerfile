# syntax=docker/dockerfile:1

# Production image: one self-contained static Go binary.
# Build from the repo root:
#   docker build -f deploy/Dockerfile -t catenary:latest .

# 1) Build the static binary. Pure Go (pgx, net/http) => CGO disabled, which is
#    what "single static Go binary" in CLAUDE.md actually means.
FROM golang:1.26-bookworm AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

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

# 2) Runtime: slim Alpine with ca-certificates, and busybox `wget` for the
#    compose healthcheck.
#
#    ALPINE RATHER THAN scratch/distroless, and that is a decision rather than
#    a default. CANT-16 records that the health-check and the image have to be
#    chosen together: a wget-based check against a scratch image can never pass,
#    the container stays `unhealthy` for ever, and anything gating on
#    `service_healthy` stalls behind it. The alternatives were a `catenary
#    healthcheck` subcommand or this. This matches Chronicle and keeps the
#    compose block identical in shape to every other Go service on the box.
FROM alpine:3.20 AS runtime
RUN apk add --no-cache ca-certificates
COPY --from=build /out/catenary /usr/local/bin/catenary
ENV CATENARY_PORT=4012
EXPOSE 4012
ENTRYPOINT ["catenary"]
CMD ["serve"]
