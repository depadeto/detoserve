# Contributing to DetoServe

Thank you for your interest in contributing! This document provides guidelines and instructions for contributing.

## How to Contribute

### Reporting Issues

- Use GitHub Issues to report bugs or suggest features
- Include steps to reproduce for bugs
- Include your environment details (OS, Go version, Node version, K8s version)

### Pull Requests

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/my-feature`)
3. Make your changes
4. Run tests and linting
5. Commit with clear messages (`git commit -m "Add KV cache TTL configuration"`)
6. Push to your fork (`git push origin feature/my-feature`)
7. Open a Pull Request against `main`

### PR Guidelines

- Keep PRs focused — one feature or fix per PR
- Update documentation if you change behavior
- Add tests for new functionality
- Follow the existing code style

## Development Setup

### Prerequisites

- Go 1.21+
- Node.js 18+
- Python 3.10+
- Docker & Docker Compose
- Make (optional)

### Getting Started

```bash
# Clone your fork
git clone https://github.com/YOUR_USERNAME/detoserve.git
cd detoserve

# Start local services
docker compose up -d

# Run the frontend
cd frontend && npm install && npm run dev

# Build a Go service (example: smart-router)
cd control-plane/smart-router && go build -o smart-router .
```

## Project Structure

| Directory | Language | Description |
|-----------|----------|-------------|
| `control-plane/smart-router/` | Go | Request routing with KV cache awareness |
| `control-plane/function-manager/` | Go | Function CRUD and instance lifecycle |
| `control-plane/tenant-manager/` | Go | Multi-tenant configuration |
| `control-plane/config-store/` | Go | GitOps config persistence |
| `control-plane/deployment-manager/` | Go | Deployment orchestration |
| `control-plane/skypilot-bridge/` | Python | SkyPilot SDK wrapper |
| `cluster-agent/` | Go | Cache reporter sidecar |
| `frontend/` | React/JS | Management UI |
| `manifests/` | YAML | Kubernetes manifests |
| `services/` | YAML | SkyPilot service templates |

## Areas Where Help Is Welcome

### High Impact

- **Smart Router** — Improve the scoring algorithm, add new routing signals
- **Testing** — Unit tests, integration tests, end-to-end tests
- **CI/CD** — GitHub Actions for build, test, lint, container image publish

### Medium Impact

- **Frontend** — Dashboard improvements, real-time status, metrics visualization
- **KAI Scheduler** — Queue policy templates for different workload patterns
- **Runtime support** — Additional inference engine integrations (TensorRT-LLM, etc.)

### Documentation

- Tutorials and guides
- API documentation
- Deployment guides for specific clouds

## Code Style

### Go

- Follow standard Go conventions (`gofmt`, `go vet`)
- Use meaningful variable and function names
- Keep functions focused and small

### JavaScript/React

- Use functional components with hooks
- Follow existing patterns in `App.jsx`

### Python

- Follow PEP 8
- Use type hints

## Commit Messages

Use clear, descriptive commit messages:

```
Add KV cache TTL configuration to Smart Router

- Add configurable TTL for prefix cache entries
- Default to 300s, configurable via CACHE_TTL_SECONDS env var
- Update docs with new configuration option
```

## License

By contributing, you agree that your contributions will be licensed under the Apache License 2.0.
