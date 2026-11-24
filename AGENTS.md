# Repository Guidelines

satmux: a relay that makes high-latency, low-bandwidth links (GEO satellite, airplane Wi-Fi) usable.
One binary: `satmux relay` runs on a ground VM, `satmux up` on the laptop. See README.md for the design.

## Layout
- `cmd/satmux`: entrypoint (version via ldflags)
- `internal/wire`: what both sides share: connection string, pin, headers, brotli codec, `Pump`
- `internal/relay`: ground side (HTTP/2 server: CONNECT tunnels, fetches)
- `internal/client`: laptop side (carrier pool, HTTP proxy + gate, SOCKS5); `e2e_test.go` runs both with injected latency
- `internal/sysproxy`: macOS system proxy via `networksetup`
- `internal/cli`: Kong commands

## Build & Test
- `make build`: builds `bin/satmux`
- `make test`: `go test -race ./...` (the e2e tests assert round-trip counts with 50 ms injected latency)
- `make lint`, `make fmt`
- `make ci`: fmt-check + lint + test + build (full local gate)

## Rules
- Standard library first. Dependencies: kong, andybalholm/brotli. Adding one needs a reason in the PR.
- Every latency claim needs an e2e test that counts round trips.
- Data on stdout (`--json`), logs on stderr. Exit codes: 0 ok, 1 error, 2 usage, 8 not reachable, 10 no relay configured.
- Never log tokens or connection strings.
- Conventional commits: feat(scope): ..., fix(scope): ..., docs: ...
