# ps-secure-signalling

A security-focused rewrite of the Unreal Engine Pixel Streaming **signalling server** ("Wilbur") and
**SFU**, in Go. It speaks the same signalling protocol (1.3.0) as the reference implementation in
[PixelStreamingInfrastructure](https://github.com/EpicGames/PixelStreamingInfrastructure), so
unmodified UE streamers (Pixel Streaming and Pixel Streaming 2), the stock browser frontend and
existing SFUs work with it unchanged.

What is different is everything around the protocol:

- **Authentication per role.** Streamers, players, SFUs and the REST API each authenticate separately,
  with pre-shared tokens, JWTs (HMAC, JWKS or OIDC discovery), TLS client certificates, a webhook to
  your own service, or your own `auth.Authenticator`.
- **Authorization and isolation.** Identities can be scoped to the streamers they may see and join, and
  can reserve the streamer ids they publish as, so nobody can squat on or impersonate a streamer.
  Every relayed message is checked against the subscription graph and stamped by the server, so no
  peer can address or speak for a connection it is not paired with.
- **Resource limits everywhere.** Message size, message rate, bounded per-connection send queues (a
  slow peer cannot stall anyone else), connection caps per role and per address, keepalives.
- **Safe by default.** Streamer and SFU ports bind to loopback; the server refuses to start if they are
  exposed while accepting anonymous connections; config keys are strict; secrets come from files.
- **A pure-Go SFU** built on [Pion](https://github.com/pion/webrtc): no native worker to build, one
  static binary on Linux, Windows and macOS, simulcast with bandwidth-driven layer selection, and both
  Pixel Streaming 2's multiplexed and the original plugin's per-player data channels.
- **Small and hookable.** The signalling server is a library of `http.Handler`s with hooks for
  connect, disconnect, every message, subscribe and streamer-id authorization, and per-peer ICE
  configuration. The binaries are thin assemblies of the packages.

## Quick start

```bash
go build -o bin/ ./cmd/...            # wilbur, sfu, teststreamer (Go 1.24+; fetches 1.27 if needed)

# The player page is the stock frontend, built from PixelStreamingInfrastructure:
#   Frontend/implementations/typescript → npm run build → SignallingWebServer/www
cp -r ../PixelStreamingInfrastructure/SignallingWebServer/www ./www

bin/wilbur                            # players on :8080, streamers on 127.0.0.1:8888, SFUs on 127.0.0.1:8889
bin/sfu -signalling-url ws://127.0.0.1:8889     # optional
```

Point Unreal at it as usual: `-PixelStreamingURL=ws://127.0.0.1:8888` (PS1) or
`-PixelStreamingConnectionURL=ws://127.0.0.1:8888` (PS2), and open `http://localhost:8080/`
(`/?StreamerId=SFU` to watch through the SFU).

No Unreal at hand? `deploy/make-test-media.sh testmedia && bin/teststreamer -media testmedia` streams
a test pattern the way Pixel Streaming 2 does (`-ps1` for the original plugin's behaviour).

For anything beyond your own machine, start from [`deploy/wilbur.example.yaml`](deploy/wilbur.example.yaml)
and [`deploy/sfu.example.yaml`](deploy/sfu.example.yaml) and read [docs/SECURITY.md](docs/SECURITY.md).

## Layout

| Path | What |
|---|---|
| `pkg/protocol` | The signalling messages as typed structs, with validation and field limits |
| `pkg/wsconn` | A websocket with read limits, rate limiting, a bounded send queue and keepalive |
| `pkg/auth` | `Authenticator`, `Identity`, `Chain`; static tokens, JWT/JWKS/OIDC, mTLS, webhook |
| `pkg/signalling` | The signalling server: registration, subscriptions, routing, hooks |
| `pkg/web` | Static file serving, the admin REST API, security headers, rate limiting |
| `pkg/turn` | Time-limited TURN credentials (coturn `use-auth-secret`) |
| `pkg/config` | Secret files and TLS settings shared by the binaries |
| `pkg/sfu` | The SFU: upstream and player peer connections, simulcast forwarding, data channel relays |
| `cmd/wilbur` | Signalling + web server binary and its YAML config |
| `cmd/sfu` | SFU binary |
| `cmd/teststreamer` | A synthetic streamer for testing without Unreal |
| `examples/embed` | Using the signalling server as a library: a custom authenticator and hooks |
| `deploy/` | Example configs, Dockerfile, compose file, dev certificate and test media scripts |

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): how it fits together, and how to extend or embed it
- [docs/AUTH.md](docs/AUTH.md): authentication methods, identity scopes, the webhook contract
- [docs/SECURITY.md](docs/SECURITY.md): threat model, the controls in place, deployment guidance
- [docs/COMPATIBILITY.md](docs/COMPATIBILITY.md): what matches the reference implementation, what
  deliberately does not, and what has been tested

## Development

```bash
go test -race ./...
go vet ./...
GOOS=windows go build ./...
```
