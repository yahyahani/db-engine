# ── Stage 1: build ────────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS builder

WORKDIR /src

# No external dependencies — only go.mod exists (no go.sum).
COPY go.mod ./
RUN go mod download

# Copy source and build a static binary.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
      -ldflags="-s -w" \
      -o /dashboard \
      ./cmd/dashboard

# ── Stage 2: runtime ──────────────────────────────────────────────────────────
FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=builder /dashboard .

# Default database directory inside the container.
# Mount a host directory here to persist data across container restarts.
RUN mkdir -p /data

EXPOSE 8080

ENTRYPOINT ["/app/dashboard"]
CMD ["-dir", "/data", "-port", "8080"]
