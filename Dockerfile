FROM golang:1.25-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /api             ./cmd/server && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -o /seed-reaches    ./cmd/seed-reaches && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -o /seed-descs      ./cmd/seed-reach-descriptions && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -o /seed-flows      ./cmd/seed-flow-ranges && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -o /import-kml      ./cmd/import-kml && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -o /embed-reaches   ./cmd/embed-reaches && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -o /backfill-comids ./cmd/backfill-comids

# --- runtime ---
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /api             /api
COPY --from=builder /seed-reaches    /seed-reaches
COPY --from=builder /seed-descs      /seed-descs
COPY --from=builder /seed-flows      /seed-flows
COPY --from=builder /import-kml      /import-kml
COPY --from=builder /embed-reaches   /embed-reaches
COPY --from=builder /backfill-comids /backfill-comids
COPY --from=builder /src/migrations  /migrations

ENV MIGRATIONS_PATH=/migrations
EXPOSE 8080
ENTRYPOINT ["/api"]
