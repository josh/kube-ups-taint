FROM golang:1.27.4-alpine3.23@sha256:f70850ef6ce341e8b6427f677049145d63c0b7ce002d4d7e3d464828b1b96dcd AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o kube-ups-taint .

FROM alpine:3.24.4@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf

COPY --from=builder /app/kube-ups-taint /usr/local/bin/
RUN kube-ups-taint version

LABEL org.opencontainers.image.source="https://github.com/josh/kube-ups-taint"
LABEL org.opencontainers.image.description="Kubernetes UPS Taint Controller"
LABEL org.opencontainers.image.licenses="MIT"

USER 65534:65534

ENTRYPOINT ["kube-ups-taint"]
