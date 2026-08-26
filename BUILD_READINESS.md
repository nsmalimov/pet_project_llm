# BUILD_READINESS — 2026-08-26

| item | value / result |
|---|---|
| Go version pinned | `go 1.23` in `go.mod` (no toolchain directive; builds on 1.23+) |
| Local toolchain used | `go1.27.0 darwin/arm64` |
| Prerequisites | Go ≥1.23, git, make; optional: Chrome (browser e2e), `claude` CLI (real agents), Docker (image build), macOS for SAFE_SANDBOX |
| `gofmt -l .` (excluding `old/`) | clean — enforced by `make fmt-check` and CI |
| `go vet ./...` | ok |
| `go build -o bin/orc ./cmd/orc` | ok |
| `go test ./...` | ok (all packages; engine ≈130 s, api ≈70 s) |
| `go test -race ./...` | ok on the last full run; `make verify` runs the race subset (store/proof/sandbox/engine concurrency tests) |
| `make verify` | `verify: OK` (build, fmt-check, vet, tests, race subset, product acceptance incl. browser e2e when Chrome is present) |
| CI | `.github/workflows/verify.yml` (ubuntu-latest, Go 1.23: fmt, vet, build, test, race, HTTP acceptance, binary starts + `/health`) — **NOT VERIFIED**: never executed on GitHub from this environment |
| Docker image | `Dockerfile` (golang:1.23-alpine build → alpine runtime) — **NOT VERIFIED**: Docker daemon not running here |
| Tests vs local state | every test uses `t.TempDir()`; shared Go build cache only (`PROOFLINE_SHARED_CACHE`); macOS-only and Chrome-only tests skip when the dependency is absent; live Claude probe gated by `PROOFLINE_LIVE=1` |
