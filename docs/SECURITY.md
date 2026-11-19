# Security

## Threat model

| Party | Trust | Can do |
|---|---|---|
| Players | untrusted, from the internet | open websockets to the player port, send any bytes |
| Streamers | semi-trusted; on shared servers, mutually distrustful | send any signalling message on the streamer port |
| SFUs | trusted infrastructure, but authenticated | act as player upstream and streamer downstream |
| Operators | trusted | config, secrets, REST API |
| Webhook / JWKS endpoints | trusted, operator-run | decide who connects |

What must hold:

1. Nobody connects in a role they were not granted.
2. No connection can read, address, spoof or disconnect a connection it is not paired with: a player
   only talks to the streamer it subscribed to, and a streamer only to its own subscribers.
3. No streamer can take another's id or intercept its players.
4. No single connection, or address, can exhaust memory, goroutines or file descriptors, or stall others.
5. Secrets do not leak into logs, URLs the server generates, process listings or saved configs.
6. Misconfiguration fails closed.

## Controls

### Authentication (1)

- Each listener authenticates for exactly one role, before the websocket upgrade (`401` otherwise).
- Methods: static tokens (≥16 chars, constant-time, all compared), JWT (`exp` required, explicit
  algorithm allow-list, no `none`, no HMAC/public-key confusion, issuer and audience checks), mTLS
  (verified chains only), webhook (fail closed, signed requests, no redirects, bounded response).
- A rejected credential is never downgraded to anonymous access.
- Optional `close_on_expiry` ends connections when their credential expires.

### Isolation (2, 3)

- **Explicit allow-list per role.** `pkg/signalling/routing.go` lists the message types each role may
  send. Everything else is dropped.
- **Server-stamped identities.** A player's messages are stamped with its own player id before
  forwarding; whatever id the player put in is overwritten. A streamer's or SFU's message to a player
  is delivered only if that player is subscribed to the sender, and `disconnectPlayer` only reaches the
  sender's own subscribers.
- **SFU multiplexing.** The player id in the multiplex header sent to Unreal comes from the SFU's own
  session for that player, never from player data. Replies from Unreal are delivered only to the
  player id they name.
- **Strict decoding.** Each message is decoded into its typed struct and re-encoded, so fields outside
  the protocol are never relayed (no smuggling of extra properties to a peer's JSON parser). Field
  sizes are bounded (SDP 128 KiB, ids 128 bytes, candidates 2 KiB), layer indices and SCTP stream ids
  are range-checked, and streamer ids are restricted to letters, digits, space and `_ . : @ + ( ) / -`,
  because they are shown in UIs and written to logs.
- **Scopes and reserved ids** (see [AUTH.md](AUTH.md)): players only see and join permitted
  streamers; a streamer with reserved ids always gets exactly those and evicts squatters, and nobody
  else may take a reserved id even while its owner is offline. A refused subscribe looks the same as
  a missing streamer, and a rename out of a subscriber's scope disconnects it.
- **Players never offer to an SFU.** The SFU takes offers as its streamer's, so a player's offer to an
  SFU is dropped by the server, and the SFU also ignores offers and ICE candidates naming its players.
- **Separate listeners.** Streamer and SFU ports are separate sockets, on loopback by default, and
  wilbur refuses to expose them while anonymous.
- **Origin check.** Browser websocket upgrades must come from the server's own origin or
  `allowed_origins`, which stops other sites opening connections with a visitor's cookies. On the
  streamer and SFU ports the Origin only has to name the same host, because Unreal's websocket
  client sends its own address without the port (`http://127.0.0.1` for `127.0.0.1:8888`); a page on
  another site still cannot connect.

### Resource limits (4)

| Limit | Default |
|---|---|
| Websocket frame size | 256 KiB (`1009` close above) |
| Message rate per connection | players 50/s, burst 200; streamers and SFUs 500/s, burst 2000. Excess is throttled, not dropped |
| Invalid messages | 20, then `1008` close |
| Outbound queue per connection | 256 (1024 for streamers and SFUs). A full queue disconnects that peer rather than blocking the sender |
| Keepalive | websocket ping every 20 s; no pong in 15 s closes |
| Connections per address | 64 (`max_conns_per_ip`), checked before authentication so floods never reach a webhook or JWKS endpoint |
| Players, streamers, subscribers | configurable caps |
| HTTP | header timeout 10 s, 16 KiB headers, per-address rate limit (600/min) on every listener, websocket upgrades included |
| Webhook | at most 32 calls in flight; excess attempts refused |
| Websocket compression | disabled (no compression bombs, no CRIME-style leaks) |
| SFU | per-player teardown frees everything; data channel messages capped at 256 KiB with bounded queues; keyframe requests to the streamer rate limited however many players ask; every goroutine recovers from panics |

All routing state sits behind one mutex, and nothing blocks while holding it: every send is a
non-blocking enqueue. Hooks run outside the lock.

### Secrets (5)

- Tokens, HMAC keys, TURN and webhook secrets are read from files (`*_file`). Inline values work but
  produce a warning. A secret file readable by other users produces a warning.
- The REST API reads credentials from the `Authorization` header only.
- Logs record subject and auth method, never tokens or query strings. SDP and candidates are not
  logged above debug.
- TURN credentials are minted per connection with an expiry (coturn `use-auth-secret`), so a leaked
  one stops working. Streamers and SFUs get long-lived ones by default (`turn.streamer_ttl`, ten years
  as in the reference), because they are configured once per connection; shorten it if your
  streamers reconnect regularly.

### Fail closed (6)

- Unknown config keys are errors.
- `admin` cannot be anonymous, and the API cannot be enabled without an authenticator.
- An unreachable webhook or JWKS endpoint refuses connections.
- TLS is 1.2 minimum, and client certificates are verified against the configured CA only.

### Web

- Static files are served through `os.Root`: neither `..` nor a symlink can escape the web root.
  No directory listings, no dotfiles, GET/HEAD only.
- Headers: a Content-Security-Policy suited to the stock frontend, `X-Content-Type-Options`,
  `Referrer-Policy: no-referrer`, `Cross-Origin-Opener-Policy`, `Permissions-Policy`, and HSTS when
  serving TLS.
- `X-Forwarded-For` is only believed from `trusted_proxies`, and then only up to the first untrusted hop.

### Containers

The Dockerfile builds static binaries into `distroless/static:nonroot` images: no shell, no package
manager, a non-root user. The compose example adds a read-only root filesystem, drops all
capabilities and sets `no-new-privileges`.

## Deployment checklist

- [ ] Players reach wilbur over TLS (wilbur's own `tls`, or a proxy listed in `trusted_proxies`).
- [ ] The streamer and SFU listeners are on loopback or a private network, and authenticated
      (tokens, JWT or mTLS) if anything else can reach them.
- [ ] Streamers have reserved ids (`publish_as`), so they cannot be squatted.
- [ ] Players are authenticated if the stream is not public, scoped with `streamers` if the server is shared.
- [ ] The REST API is off, or on with an admin credential that only operators hold.
- [ ] Secrets are in files readable only by the service user.
- [ ] TURN uses `turn.secret_file`, not static credentials in `peer_options`.
- [ ] The SFU's media port(s) are the only UDP open to players, and it uses `public_ips` behind NAT.
- [ ] The SFU connects with `wss://` and verifies the server (`tls.ca_file` for a private CA), and
      never runs with `insecure_skip_verify`.
- [ ] Connection and subscriber caps suit your capacity.

## Out of scope

- Media is end-to-end DTLS-SRTP between peers (or peer and SFU); the signalling server never sees it.
  The SFU necessarily decrypts and re-encrypts media.
- Input from players to Unreal travels over the data channel. Whether a player may control the
  application is Unreal's decision (`-AllowPixelStreamingCommands`, input ownership), not the
  signalling server's.
- DDoS at the network layer belongs in front of the service.
