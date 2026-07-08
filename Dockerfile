# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /src

# Cache dependencies.
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build.
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /bin/piecesoflife .

# Runtime stage
FROM alpine:3.21

# ffmpeg remuxes browser-recorded WebM uploads (MediaRecorder output has no
# duration/seek cues, which breaks playback in some contexts).
# libheif-tools provides heif-convert, which transcodes HEIC and AVIF photo
# uploads (default camera formats on iPhones and many Androids) to JPEG.
# This image's ffmpeg 6.1 cannot decode tiled HEIC, so it is not a
# substitute.
RUN apk add --no-cache ca-certificates tzdata ffmpeg libheif-tools

COPY --from=builder /bin/piecesoflife /usr/local/bin/piecesoflife

# Create non-root user.
RUN addgroup -S app && adduser -S app -G app

# Create data directories.
RUN mkdir -p /data/db /data/uploads && chown -R app:app /data

USER app

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- "http://localhost:${PORT:-8080}/health" || exit 1

ENTRYPOINT ["piecesoflife"]
