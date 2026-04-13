FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w \
      -X main.version=${VERSION} \
      -X main.gitCommit=${GIT_COMMIT} \
      -X main.buildDate=${BUILD_DATE}" \
    -o /s3nfsgw ./cmd/s3nfsgw

FROM alpine:3.19

RUN apk add --no-cache ca-certificates

COPY --from=builder /s3nfsgw /usr/local/bin/s3nfsgw

EXPOSE 2049 9090

RUN adduser -D -u 10001 -g "" appuser \
 && mkdir -p /var/lib/s3nfsgw /var/cache/s3gw \
 && chown -R appuser:appuser /var/lib/s3nfsgw /var/cache/s3gw

VOLUME ["/var/lib/s3nfsgw", "/var/cache/s3gw"]

USER appuser

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://localhost:9090/health || exit 1

ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.title="s3-filesystem-gateway" \
      org.opencontainers.image.description="NFSv4.1-to-S3 gateway exposing S3-compatible storage as an NFS filesystem" \
      org.opencontainers.image.source="https://github.com/rupivbluegreen/s3-filesystem-gateway" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${GIT_COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}"

ENTRYPOINT ["/usr/local/bin/s3nfsgw"]
