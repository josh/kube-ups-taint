# AGENTS.md

Kubernetes controller that taints nodes from NUT UPS battery state so pods are shed during power outages.

## Tech Stack

- Go (see go.mod for current version)
- Kubernetes client-go
- Minimal built-in NUT network protocol client (no CGO)

## Build

```
# Build
go build -o kube-ups-taint .

# Lint
golangci-lint run

# Docker
docker build -t kube-ups-taint .
```

## Project Structure

- `main.go` - Config, reconcile loop, taint and annotation logic
- `nut.go` - NUT network protocol client
- `Dockerfile` - Multi-stage static build
- `charts/kube-ups-taint` - Helm chart

## Code Patterns

- JSON configuration file for settings, reloaded every interval
- `log/slog` for structured logging
- Kubernetes client-go for API interactions
- Node updates use get/update with conflict retry, not server-side apply (node taints are an atomic list)
- All state lives on the Node (taints and annotations) so replicas and restarts behave the same

## Boundaries

- Only touch taints and annotations under the `kube-ups-taint.josh.github.io/` prefix
- Never remove taints while a UPS cannot be read
- Keep the NUT client small and dependency free
- Chart self-tolerations must cover every taint the controller can set
