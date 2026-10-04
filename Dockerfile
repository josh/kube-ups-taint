FROM golang:1.27.1-alpine3.23@sha256:d9e2f2f07b10cc922da3e80e035c3058810b328d5aef82d2c63680967c5e2ec9 AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o kube-ups-taint .

FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

COPY --from=builder /app/kube-ups-taint /usr/local/bin/
RUN kube-ups-taint version

LABEL org.opencontainers.image.source="https://github.com/josh/kube-ups-taint"
LABEL org.opencontainers.image.description="Kubernetes UPS Taint Controller"
LABEL org.opencontainers.image.licenses="MIT"

USER 65534:65534

ENTRYPOINT ["kube-ups-taint"]
