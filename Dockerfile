# syntax=docker/dockerfile:1.4

# Build stage with comprehensive caching
FROM golang:1.26-alpine AS builder

WORKDIR /app

# Install build dependencies (cached layer)
RUN apk add --no-cache git ca-certificates tzdata

# Copy only what the build needs, rather than the whole context: pulumi/ is a
# second Go module and would break `go build ./...` if it landed in this tree.
# vendor/ makes the build hermetic and offline — no module download.
COPY go.mod go.sum ./
COPY vendor/ ./vendor/
COPY cmd/ ./cmd/
COPY internal/ ./internal/

# The version stamped into the binary. Passed in rather than derived here: the
# build copies four paths and not .git, so no git metadata exists in this stage.
# `make docker-build` supplies it from scripts/version.sh.
ARG VERSION=dev

# Build the application from vendored deps — no module download needed
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -mod=vendor -a -installsuffix cgo \
    -ldflags="-w -s -extldflags \"-static\" -X main.version=${VERSION}" \
    -o accretion-api ./cmd/accretion-api

# Final stage - minimal runtime image
FROM alpine:3.19 AS runtime

# Install runtime dependencies (cached layer)
RUN apk --no-cache add ca-certificates tzdata

# Create non-root user for security
RUN addgroup -g 1001 -S appuser && \
    adduser -S -D -H -u 1001 -h /app -s /sbin/nologin -G appuser -g appuser appuser

WORKDIR /app

# Copy binary from builder stage
COPY --from=builder /app/accretion-api .

# Set execute permissions
RUN chmod +x accretion-api

# Copy web static files from build context
COPY web ./web

# Copy prompt templates
COPY prompts/ ./prompts/

# Change ownership to non-root user
RUN chown -R appuser:appuser /app

# Switch to non-root user
USER appuser

# The API listener, which serves HTTPS by default (api.tls.enabled). The UI
# listener (8080) is off by default; publish it too if config.ui.enabled is
# turned on.
EXPOSE 41337
# The same subcommand the Kubernetes probes run. It resolves the config, so it
# covers every enabled listener and speaks the right scheme — unlike the wget
# call this replaces, which hardcoded http and one port, and which BusyBox wget
# could not have pointed at a private CA anyway (no --ca-certificate flag).
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["./accretion-api", "healthcheck"]

CMD ["./accretion-api", "server"]
