# Changelog

## Unreleased

- First version: `satmux relay` (ground VM) and `satmux up` (laptop).
  - HTTP/2-over-TLS 1.3 carriers with a pinned relay key and a shared token.
  - Tunnels (HTTP CONNECT, SOCKS5) that open without a round trip.
  - One-round-trip gate fetches, with brotli compression in both directions.
  - CPU-aware choice of compression level, from the relay's own calibration and the client's measured link rate.
  - macOS system proxy integration that restores your settings on exit.
  - `status`, `bench` and `env` commands.
