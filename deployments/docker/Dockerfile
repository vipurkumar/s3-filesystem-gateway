FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /s3nfsgw ./cmd/s3nfsgw

FROM alpine:3.19

RUN apk add --no-cache ca-certificates

COPY --from=builder /s3nfsgw /usr/local/bin/s3nfsgw

EXPOSE 2049

RUN adduser -D -u 10001 -g "" appuser
USER appuser

ENTRYPOINT ["/usr/local/bin/s3nfsgw"]
