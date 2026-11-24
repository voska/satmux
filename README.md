# satmux

**Make airplane Wi-Fi usable.** satmux is a small relay for links where every round trip costs most of a second, such as geostationary-satellite in-flight Wi-Fi, maritime VSAT and remote sites. You run one copy on a cheap VM near where the satellite traffic lands on the internet, and one on your laptop. Web pages, `git`, package managers and API clients then need far fewer round trips, and far fewer bytes, to get anything done.

```
without satmux   laptop ── DNS ── TCP ── TLS ── request ══(4 × 700 ms over the satellite)══▶ website

with satmux      laptop ══(1 × 700 ms)══▶ relay VM ──(DNS, TCP, TLS on fiber, ~20 ms)──▶ website
                        warm, compressed, multiplexed
```

> **Credit where it's due:** this project is an independent Go reimplementation of [**Th0rgal/satmux**](https://github.com/Th0rgal/satmux), which first showed that warm, multiplexed connections to a ground VM fix GEO cabin Wi-Fi. Same idea and same name, rebuilt from first principles after measuring a real flight. It isn't affiliated with the original; go star it.

- One static binary for macOS, Linux and Windows, with no runtime dependencies.
- A relay costs a few dollars a month, or a few cents for a flight if you create it before boarding and delete it after landing.
- Your relay authenticates you with a shared token, and your laptop checks the relay against a pinned key. Nothing runs on anyone else's server.

## Results

Measured on a real flight (Nov 2026): Panasonic in-flight Wi-Fi, M1 MacBook, relay on a 2-vCPU Hetzner VM in Hillsboro OR (about 25 ms from the cabin's internet exit), link round trip 0.7–1.5 s. Each value is the median of 4 rounds, with the order alternating between the original satmux and this one. Every request uses a cold connection, like a first page load.

| page | direct | original satmux, tunnel | **satmux, tunnel** | original satmux, gate | **satmux, gate** |
|---|---|---|---|---|---|
| news.ycombinator.com | 1.53 s | 1.54 s | **1.48 s** | 0.93 s | **0.85 s** |
| api.github.com (repo JSON) | 2.59 s | 2.05 s | **1.94 s** | 1.29 s | 1.48 s ¹ |
| pypi.org JSON API (193 KB) | 1.68 s | 2.70 s | **1.48 s** | 1.27 s | **0.79 s** |
| developer.mozilla.org | 2.36 s | 3.38 s | **2.61 s** | 1.46 s | **1.32 s** |
| github.com repository page | 15.67 s ² | 2.20 s | **2.19 s** | 2.33 s | **2.12 s** |
| en.wikipedia.org article (433 KB) | 2.52 s | 2.24 s | **2.11 s** | 1.96 s | **0.99 s** |
| 200 KB JSON POST (an API call) | 6–7.7 s | — | — | not supported (body dropped) | **≈1 s** ³ |

¹ GitHub's API itself takes 460–780 ms to answer, even to the relay, and that swamps the difference. Over 12 alternating rounds the medians were 1.90 s for satmux and 2.22 s for the original.
² Direct connections to github.com repeatedly stalled in the cabin's port-443 proxy.
³ Measured with this project's Python prototype, which uses the same request-body compression.

The two paths:
- **Tunnel** is what browsers use. TLS stays end to end, so it costs two round trips.
- **Gate** is the one-round-trip, compressed path for CLI tools and API clients.


## Is this for you?

**Yes**, if your link is a **geostationary (GEO) satellite**. Round trips run 550–800 ms, so even a small page takes seconds. You can check with `ping -c 5 1.1.1.1` (anything over about 500 ms is GEO) and `curl ipinfo.io` (the `org` names the operator). Typical cases:
- Airline Wi-Fi from Panasonic (AS64294), Viasat, Inmarsat GX/SITA (AS206433), Gogo 2Ku, Thales/Inmarsat, Anuvu or Intelsat.
- Ships on VSAT, oil rigs, remote research stations.
- Rural GEO broadband (HughesNet, Viasat).

**Probably not**, on **Starlink or other LEO** links (round trips of 30–60 ms) or ordinary broadband. Round trips there are cheap, so there's little to remove. Some airlines now fly Starlink: if `ping` shows under 100 ms, you don't need this.

## Why airplane Wi-Fi is slow

To work out where the time goes, we measured a real flight on Panasonic in-flight Wi-Fi (Nov 2026):

| what we measured | value |
|---|---|
| Round trip to anywhere | 700–800 ms (the speed of light to 35,786 km and back, twice, plus queueing) |
| Throughput of one TCP connection | about 22 KB/s at busy times |
| Throughput of the whole seat (all connections together) | about 45 KB/s at busy times; bursts of several hundred KB/s when the beam was quiet |
| UDP | blocked (no QUIC, WireGuard, Tailscale direct paths or mosh) |
| TCP on port 443 | intercepted by an on-board proxy that sometimes resets or stalls connections under load |
| DNS | intercepted by the cabin, costing one more satellite round trip |
| TCP on other ports | also proxied: the ground side answers in about 150 ms, but end-to-end is still one satellite round trip |

That gives two scarce resources, **round trips** and **bytes**, and almost nothing else matters.

Fetching one HTTPS page from a site you haven't connected to yet costs:

| step | direct | satmux tunnel (browsers) | satmux gate (CLI tools, API clients) |
|---|---|---|---|
| DNS lookup | 1 round trip | 0 (done on the relay) | 0 |
| TCP handshake | 1 | 0 (relay connections are already open) | 0 |
| TLS handshake | 1 (TLS 1.3) | 1 (TLS stays end to end) | 0 (done on the relay) |
| request and response | 1 | 1 | 1 |
| **total** | **4 round trips, about 3 s** | **2, about 1.5 s** | **1, about 0.75 s** |
| bytes over the satellite | as the site sends them (gzip or none) | as the site sends them | brotli, tuned to your link and the relay's CPU; at quality 11, 14–25% smaller than gzip on the pages we measured |

The ground leg (relay to website) runs on fiber, where a round trip takes milliseconds.

## How it works

```
 laptop                               relay VM, near the satellite's exit
┌──────────────────────────────────┐     ┌──────────────────────────────────┐
│ apps                             │     │                                  │
│  ├▶ 127.0.0.1:8080  HTTP proxy   │     │ CONNECT ─▶ TCP to any host       │
│  │                  + gate       │════▶│ fetch   ─▶ pooled HTTP/1.1 + 2   │──▶ internet
│  └▶ 127.0.0.1:1080  SOCKS5       │     │            brotli back           │
│                                  │     │                                  │
│ satmux up                        │     │ satmux relay                     │
└──────────────────────────────────┘     └──────────────────────────────────┘
  4 warm carriers: HTTP/2 over TLS 1.3, pinned relay key, token on every request
```

**Carriers.** The client keeps a few long-lived connections ("carriers", 4 by default) open to the relay. Each one is plain HTTP/2 over TLS 1.3, so satmux has no wire protocol of its own:
- HTTP/2 multiplexes many requests over each connection and gives each one its own flow control, so a slow download doesn't block a small page load.
- HTTP/2 PINGs detect a dead connection within about 20 s; the client then reconnects with backoff.
- Header compression (HPACK) means a repeated `Cookie` or `Authorization` header costs a byte or two after the first request.
- Cancelling a request locally cancels it on the relay.

Requests are spread over the carriers by load. Each TCP connection over the satellite gets its own share of the seat, and one lost packet stalls only its own connection.

There are **two paths** through a carrier.

1. **Tunnel** (HTTP `CONNECT` or SOCKS5): a raw TCP tunnel to any host and port.
   - The client answers your application's `CONNECT` at once and sends the tunnel request together with the application's first bytes (a TLS ClientHello) in a single burst. Opening a tunnel therefore costs no round trip; an end-to-end test checks this by counting round trips.
   - TLS stays end to end between your application and the website. The relay sees only hostnames and encrypted bytes.
   - Browsers use this path.

2. **Gate** (`http://127.0.0.1:8080/https://example.com/path`, or any plain-`http://` request through the proxy): the whole request goes to the relay in one burst, method, headers and body included. The relay then:
   - does DNS, TCP, TLS and HTTP on fiber, with pooled HTTP/2 connections to origins;
   - streams the response back compressed with **brotli**, at a level chosen per response (see below).

   Request bodies are compressed on the way up too. JSON typically shrinks about 5×; pypi's 120 KB API response, for example, compresses to 23 KB.

**Compression that streams.** The relay compresses responses as they arrive, flushing whenever the origin pauses for more than 5 ms. Server-sent events and other streams are never held back, and the encoder keeps its context across flushes, so the result is within about 4% of compressing the whole body at once. On real pages, brotli 11 beat gzip by 14–25%, and beat LZMA too:

| page | original | gzip -9 | LZMA, whole body | satmux (brotli 11, streaming) |
|---|---|---|---|---|
| news.ycombinator.com | 34 KB | 5.6 KB | 5.1 KB | **4.6 KB** |
| pypi.org JSON API | 120 KB | 27.2 KB | 23.1 KB | **22.6 KB** |
| developer.mozilla.org | 117 KB | 16.3 KB | 13.8 KB | **12.7 KB** |
| github.com repository page | 265 KB | 40.0 KB | 31.1 KB | **30.1 KB** |
| en.wikipedia.org article | 433 KB | 77.2 KB | 61.3 KB | **59.7 KB** |

Content that's already compressed (images, video, archives, responses the origin gzipped anyway) passes through untouched.

**CPU-aware tuning.** Quality 11 is the smallest, but it's slow: on a 2-vCPU cloud VM it compresses about 0.3 MB/s, against 8 MB/s at quality 9 and 23 MB/s at quality 5. When the link is fast, compressing harder makes pages *slower*. satmux measures both sides and picks the level that delivers each response soonest:

- At startup, the relay calibrates every brotli level on its own CPU (speed and ratio, in well under a second) and logs the table.
- The client measures how fast the link actually delivers bytes, and sends that rate with each request. Transfers paced by something other than the link, such as the relay's encoder or an application inside a tunnel, only count as lower bounds, so a slow compressor can't talk the estimate into "slow link, use more CPU".
- For each response, the relay picks the level that minimizes `max(compress time, transmit time) + time to compress the first chunk`, accounting for other compressions running at the same time.

On that VM this means quality 11 only for links slower than about 50 KB/s, quality 9 down to roughly 5 MB/s, and quality 4–5 above that. Until the client has measured anything, it assumes a fast link, which favours cheap levels. Request bodies go through the same choice on the client, using the client's own calibration.

## Quick start

You need:
- A Linux VM with a public IP, anywhere with a datacenter near the satellite's exit point. 1 vCPU and 1 GB of RAM is plenty.
- A laptop running macOS, Linux or Windows.

### 1. Find where your link exits

On the plane (or other satellite link), with satmux not running:

```bash
curl ipinfo.io
```

The `city` and `org` show where your traffic lands on the internet. Rent the VM in the closest cloud region:

| exit (example) | good relay regions |
|---|---|
| US West (e.g. Panasonic via Millbrae, CA) | Hillsboro OR, San Jose, Los Angeles |
| US East | Ashburn VA, New York |
| Europe (e.g. Inmarsat GX via SITA in Amsterdam) | Amsterdam, Frankfurt, Falkenstein, London |
| Middle East / Asia | Singapore, Mumbai, Tokyo |

Precision doesn't matter much. The satellite hop is 700 ms and the relay-to-exit hop is tens of milliseconds, so "same region" is good enough. If you set the relay up before you fly, guess from the route: the exit is usually in the country where the airline's satellite provider runs its ground stations.

### 2. Start the relay

**Any VM, by hand:**

```bash
# on the VM (amd64 shown; use the arm64 build on Arm VMs)
curl -fsSL https://github.com/voska/satmux/releases/latest/download/satmux_linux_amd64.tar.gz | sudo tar -xz -C /usr/local/bin satmux
sudo curl -fsSL -o /etc/systemd/system/satmux.service https://raw.githubusercontent.com/voska/satmux/main/deploy/satmux.service
sudo systemctl daemon-reload && sudo systemctl enable --now satmux
sudo SATMUX_STATE_DIR=/var/lib/satmux satmux relay invite
```

The last command prints your **connection string**:

```
satmux://Xb7...4Q@203.0.113.7:8443?pin=3mF0lQ9h2Vx8pK1cRj7sT5wYbN4dZ6aE0uG8iL2oHqM
```

Treat it like a password. Anyone who has it can use your relay.

Open **TCP port 8443** in the provider's firewall. Don't use 443 for the relay: cabin Wi-Fi often intercepts that port.

**Hetzner Cloud in one command** (`hcloud` CLI; a cpx11 is about $0.03/hour in US regions, less in the EU):

```bash
hcloud server create --name satmux --type cpx11 --image ubuntu-24.04 --location hil \
  --ssh-key you --user-data-from-file deploy/cloud-init.yaml
# a minute later:
ssh root@SERVER_IP SATMUX_STATE_DIR=/var/lib/satmux satmux relay invite
```

`deploy/cloud-init.yaml` works on any provider that accepts cloud-init user data (DigitalOcean, Vultr, AWS, GCP, Linode and others). It installs the latest release, enables BBR and starts the service.

### 3. Connect the laptop

```bash
brew install voska/tap/satmux      # or download a release, or: go install github.com/voska/satmux/cmd/satmux@latest
satmux join 'satmux://...'         # saves the connection string (file mode 0600)
satmux up --system-proxy           # macOS: points the system proxy at satmux; restores it on exit
```

That's it: Safari, Chrome, Arc, Slack and other apps that follow the system proxy now go through the relay. Press Ctrl-C to stop, and your previous proxy settings come back.

Check it:

```bash
satmux status
satmux bench      # times a few pages direct, through a tunnel, and through the gate
```

## Using it

### Browsers and Mac apps

`satmux up --system-proxy` covers them. You can also turn it on and off by hand while `satmux up` runs in another terminal:

```bash
satmux proxy on
satmux proxy off      # also the fix if satmux ever crashed and left the proxy pointing at nothing
satmux proxy status
```

Local addresses, private networks and Tailscale (`100.64.0.0/10`, `*.ts.net`, `*.tailscale.com`) bypass the proxy. That way your tailnet and SSH sessions never depend on satmux.

### Command-line tools

Most CLI tools ignore the macOS system proxy and read environment variables instead:

```bash
eval "$(satmux env)"      # HTTPS_PROXY, HTTP_PROXY, NO_PROXY for this shell
git clone https://github.com/...   # git, curl, pip, npm, cargo, go, brew: all now tunnel through the relay
```

### API clients: use the gate

The gate is the fastest path: one round trip, with compression in both directions. Point any HTTP client's base URL at it:

```bash
curl http://127.0.0.1:8080/https://api.github.com/repos/golang/go

# OpenAI- or Anthropic-style SDKs and CLIs that take a base URL:
export ANTHROPIC_BASE_URL=http://127.0.0.1:8080/https://api.anthropic.com
export OPENAI_BASE_URL=http://127.0.0.1:8080/https://api.openai.com/v1
```

Server-sent events (streaming LLM responses) stream through token by token. Request bodies go up compressed, which matters on a seat where upload is as scarce as download.

### Anything else that speaks SOCKS5

```bash
curl -x socks5h://127.0.0.1:1080 https://example.com
ssh -o ProxyCommand='nc -X 5 -x 127.0.0.1:1080 %h %p' user@host   # see the SSH note below
```

### SSH and interactive sessions

An SSH session is one long-lived connection, so it has no handshakes left to remove. What helps instead:

```
# ~/.ssh/config
Host myserver
    Compression yes          # terminal redraws compress 3-10x
    ServerAliveInterval 15   # notice a dead link in seconds, not minutes
```

Mosh is better still, if the link allows UDP. Most GEO cabin Wi-Fi doesn't.

## Commands

| command | where | what it does |
|---|---|---|
| `satmux relay` | VM | run the relay (`--listen :8443`, `--state-dir`, `--public-addr`, `--quality` to force a level) |
| `satmux relay invite` | VM | print the connection string |
| `satmux join <string>` | laptop | save the connection string |
| `satmux up` | laptop | connect and serve the local proxy (`--system-proxy`, `--carriers 4`, `--http 127.0.0.1:8080`, `--socks 127.0.0.1:1080`, `--quality` to force a level instead of auto) |
| `satmux status` | laptop | carriers, round-trip times, bytes delivered vs. bytes over the link |
| `satmux bench [url...]` | laptop | direct vs. tunnel vs. gate timings |
| `satmux env` | laptop | shell exports for CLI tools |
| `satmux proxy on\|off\|status` | laptop | macOS system proxy |
| `satmux version` | both | version |

Every command takes `--json` for machine-readable output on stdout; logs go to stderr. Exit codes:

| code | meaning |
|---|---|
| 0 | ok |
| 1 | error |
| 2 | bad usage |
| 8 | client or relay not reachable (retry) |
| 10 | no relay configured |

Settings can also come from the environment: `SATMUX_SERVER`, `SATMUX_HTTP`, `SATMUX_SOCKS`, `SATMUX_LISTEN`, `SATMUX_STATE_DIR`, `SATMUX_PUBLIC_ADDR`, `SATMUX_CONFIG_DIR`, `SATMUX_LOG_LEVEL`.

## Security

**What's protected**
- **Laptop to relay:** TLS 1.3. The client checks the relay's public key against the pin in the connection string, and the relay checks the token on every request. Other passengers on the cabin Wi-Fi see encrypted traffic to one IP and port.
- **Tunnels (browsers, `HTTPS_PROXY` tools, SOCKS):** TLS stays end to end between your application and the website. The relay sees hostnames and encrypted bytes, nothing more.

**What the relay sees**
- **Gate requests** (`http://127.0.0.1:8080/https://...`, plain-`http://` proxy requests): the relay makes the request for you, so it sees URLs, headers (including `Authorization` and cookies) and bodies in plaintext. It's your VM, but choose consciously which traffic uses the gate. Logs never contain bodies, headers or the token; URLs appear only in debug logs and in messages about failed requests.

**What the token grants**
- **The token is a key to an open proxy.** Whoever has the connection string can send traffic from your VM's IP, including to services on the VM's own network, such as cloud metadata endpoints. Keep it secret. If it leaks, rotate it by deleting the state directory and restarting the relay; that issues a new key and token.
- **Scanners see little.** Without the token, the relay answers every request with 404. Its TLS certificate is self-signed with the name "satmux relay".

The relay runs under systemd as a dynamic, unprivileged user with a read-only filesystem (see `deploy/satmux.service`).

## Tuning and troubleshooting

**No carriers come up** (`satmux status` shows `0/4`):
- Log in to the captive portal first; satmux can't do that for you.
- Check that the relay port is reachable: `nc -vz RELAY_IP 8443`. Some networks only allow a few ports. Try `--listen :8080`, `:993` or `:22` on the relay (a port its own SSH isn't using), then rejoin with the new string from `satmux relay invite --listen ...`.
- A pin error in the client log means the connection string doesn't match the relay. Run `satmux relay invite` again.

**Pages fail after `satmux up` crashed:** the system proxy still points at it. Run `satmux proxy off`.

**Compression level:** automatic by default (see CPU-aware tuning). `satmux status --json` shows the measured link rate. To force a level, use `satmux up --quality N` (per client) or `satmux relay --quality N` (for everyone).

**Large downloads:** each TCP connection through a cabin gets only part of the seat's bandwidth, and a single download rides one carrier. Downloaders that split files into ranges (`aria2c -x 4`, for example) spread across carriers automatically.

**`--carriers`:** 4 suits a seat-limited link. More carriers help many simultaneous downloads; fewer save keepalive bytes.

## Limitations

- **TCP only.** UDP (QUIC/HTTP/3, WebRTC calls, games, WireGuard) isn't carried. Browsers fall back to TCP automatically.
- **Browsers still pay for their own TLS handshake** (2 round trips for a new site, versus 4 direct), because satmux doesn't intercept TLS. The gate path avoids this for tools you can point at it.
- **It can't beat physics.** One round trip to geostationary orbit and back is the floor. satmux only removes the extra round trips.
- The system proxy integration is macOS-only. On Linux and Windows, use `satmux env` or your desktop's proxy settings with `127.0.0.1:8080`.

## Roadmap

- **Shared-history compression.** Both sides already hold every response; reusing previous responses from the same site as a compression dictionary (as in RFC 9842) would shrink repeat visits and API calls dramatically.
- **Striping large downloads across carriers**, to get past per-connection shaping.
- An opt-in TLS-intercepting mode with a local CA, so browsers also get one-round-trip, compressed fetches.
- DNS through the relay, for apps that resolve names themselves before connecting through SOCKS.

## Prior art

- **[Th0rgal/satmux](https://github.com/Th0rgal/satmux)** (Python) is the original. It showed that warm, multiplexed carriers to a ground VM fix GEO cabin Wi-Fi, and this project exists because of it. This version started as a rewrite after measuring the original's weak points on a real flight:
  - per-chunk zlib that was often larger than the origin's own gzip;
  - no request bodies through its one-round-trip path;
  - a 30-second cap on responses;
  - custom stream cipher instead of TLS;
  - no detection of dead carriers.
- **Performance-enhancing proxies** ([RFC 3135](https://www.rfc-editor.org/rfc/rfc3135)) are what satellite operators already run. They split TCP at both ends of the satellite hop, so the ends don't wait on its acknowledgements. They don't remove application round trips like DNS, TLS handshakes or HTTP requests. satmux works one layer up.
- **[hikyaku](https://github.com/SukkaW/hikyaku)**, **[sshuttle](https://github.com/sshuttle/sshuttle)** and Shadowsocks-family proxies multiplex or tunnel traffic. They aren't built around round-trip elimination and compression for very-high-latency links.
- **[mosh](https://mosh.org)** is the right answer for interactive shells on lossy links that allow UDP.

## Building from source

```bash
git clone https://github.com/voska/satmux && cd satmux
make build      # bin/satmux
make test       # unit + end-to-end tests with injected latency (-race)
make ci         # format check, lint, tests, build
```

Requires Go 1.26 or later. Dependencies: [kong](https://github.com/alecthomas/kong) for the CLI and [andybalholm/brotli](https://github.com/andybalholm/brotli). Everything else uses the Go standard library.

The end-to-end tests run a real relay and client with 50 ms of latency injected each way, and assert round-trip counts: a gate fetch takes one round trip, and a tunnel's first byte echoes back in one round trip. A change that adds a round trip fails CI.

## License

MIT
