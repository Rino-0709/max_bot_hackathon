FROM golang:1.24-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/maxbot ./cmd/maxbot

FROM alpine:3.21

RUN apk add --no-cache tzdata wget && addgroup -S app && adduser -S app -G app

WORKDIR /app

COPY --from=builder /out/maxbot /app/maxbot

RUN mkdir -p /app/data && chown -R app:app /app

USER app

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1

ENTRYPOINT ["/app/maxbot"]
