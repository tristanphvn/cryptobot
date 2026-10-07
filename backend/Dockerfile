# ── Build stage ────────────────────────────────────────────
FROM golang:1.26.6-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /server ./cmd/server && \
    CGO_ENABLED=0 GOOS=linux go build -o /migrate-schema ./cmd/migrate-schema

# ── Runtime stage ──────────────────────────────────────────
FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /server /server
COPY --from=builder /migrate-schema /migrate-schema

EXPOSE 8081

# Default entrypoint runs the long-lived server. Override to run one-shot
# tools, e.g.:
#   docker compose run --rm --entrypoint /migrate-schema backend
ENTRYPOINT ["/server"]
