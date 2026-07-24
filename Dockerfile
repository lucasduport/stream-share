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
# the optional live-stream technical-info probe; see STREAM_TECH_PROBE_ENABLED)
RUN apk add --no-cache ca-certificates ffmpeg

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

# Set entrypoint
ENTRYPOINT ["/stream-share"]
