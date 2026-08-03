# Build stage
FROM golang:1.23-alpine AS builder

# Install CA certificates for HTTPS requests during build (if needed)
RUN apk add --no-cache ca-certificates

# Set working directory
WORKDIR /app

# Copy dependency files and vendor directory first for better caching
COPY go.mod go.sum vendor/ ./
RUN go mod download  # Validates dependencies

# Copy source code
COPY . .

# Build static binary
RUN CGO_ENABLED=0 GOOS=linux go build -o stream-share .

# Runtime stage
FROM alpine:3.19

# Install CA certificates for runtime HTTPS, and ffmpeg (for ffprobe, used by
# the optional live-stream technical-info probe; see STREAM_TECH_PROBE_ENABLED,
# and for rendering the on-screen error slate; see ERROR_SLATE_ENABLED).
# ttf-dejavu supplies the font the slate's drawtext filter needs — without it
# slates are skipped and failed streams drop as they did before.
RUN apk add --no-cache ca-certificates ffmpeg ttf-dejavu

# Create non-root user for security
RUN adduser -D appuser

# Copy binary from builder stage
COPY --from=builder /app/stream-share /stream-share

# Ensure executable permissions
RUN chmod +x /stream-share

# Switch to non-root user
USER appuser

# Expose port (adjust if your app uses a specific port; based on code, it might be 8080 or similar)
EXPOSE 8080

# Container health reflects service READINESS: /healthz returns 200 once
# stream-share has finished starting up and is listening, and 503 (or a refused
# connection) before then. This is what makes the container report `healthy`, so
# other services can order on it with `depends_on: condition: service_healthy`.
# It intentionally does NOT reflect provider/VPN state — that lives on the
# authenticated /api/internal/health endpoint and is consumed by an external VPN
# watchdog (see docker-compose.yml). The check is trivial and never touches the
# provider. start-period is generous to cover slow first-time playlist fetches.
HEALTHCHECK --interval=10s --timeout=5s --start-period=60s --retries=3 \
  CMD wget -q -O /dev/null "http://127.0.0.1:${PORT:-8080}/healthz" || exit 1

# Set entrypoint
ENTRYPOINT ["/stream-share"]
