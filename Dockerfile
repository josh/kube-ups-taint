FROM golang:1.27.2-alpine3.23@sha256:9e45f0eb4a63ed37ad6e604950407558b7c738e34e015ae77a5d4ad369cde079 AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o kube-ups-taint .

FROM alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a

COPY --from=builder /app/kube-ups-taint /usr/local/bin/
RUN kube-ups-taint version

LABEL org.opencontainers.image.source="https://github.com/josh/kube-ups-taint"
LABEL org.opencontainers.image.description="Kubernetes UPS Taint Controller"
LABEL org.opencontainers.image.licenses="MIT"

USER 65534:65534

ENTRYPOINT ["kube-ups-taint"]
