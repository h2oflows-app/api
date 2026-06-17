FROM golang:1.25-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /api             ./cmd/server && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -o /seed-flows      ./cmd/seed-flow-ranges && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -o /backfill-river-gnis ./cmd/backfill-river-gnis

# --- runtime ---
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /api             /api
COPY --from=builder /seed-flows      /seed-flows
COPY --from=builder /backfill-river-gnis /backfill-river-gnis
COPY --from=builder /src/migrations  /migrations

ENV MIGRATIONS_PATH=/migrations
EXPOSE 8080
ENTRYPOINT ["/api"]
