FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
COPY *.go ./
COPY cmd ./cmd
COPY caddymodule ./caddymodule
RUN CGO_ENABLED=0 go build -o /out/scopecache ./cmd/scopecache

FROM alpine:latest
RUN apk add --no-cache curl
COPY --from=builder /out/scopecache /usr/bin/scopecache
CMD ["/usr/bin/scopecache"]
