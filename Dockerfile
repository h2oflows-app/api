FROM golang:1.25-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /api             ./cmd/server && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -o /backfill-run-state  ./cmd/backfill-run-state && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -o /backfill-centerline ./cmd/backfill-centerline && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -o /backfill-run-elevation ./cmd/backfill-run-elevation && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -o /backfill-watchlist-slugs ./cmd/backfill-watchlist-slugs && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -o /backfill-river-topology ./cmd/backfill-river-topology

# --- runtime ---
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /api             /api
COPY --from=builder /backfill-run-state  /backfill-run-state
COPY --from=builder /backfill-centerline /backfill-centerline
COPY --from=builder /backfill-run-elevation /backfill-run-elevation
COPY --from=builder /backfill-watchlist-slugs /backfill-watchlist-slugs
COPY --from=builder /backfill-river-topology /backfill-river-topology
COPY --from=builder /src/migrations  /migrations

ENV MIGRATIONS_PATH=/migrations
EXPOSE 8080
ENTRYPOINT ["/api"]
