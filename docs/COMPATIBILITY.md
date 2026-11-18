# Compatibility with the reference implementation

Compared against PixelStreamingInfrastructure `master` (Wilbur in `SignallingWebServer/` +
`Signalling/`, the SFU in `SFU/`), signalling protocol 1.3.0.

## Unchanged

- Every message type and field in `Common/protobuf/signalling_messages.proto`, with the same flows:
  `identify` → `endpointId` → `endpointIdConfirm`; `config` on connect with `protocolVersion: "1.3.0"`;
  `listStreamers`/`streamerList` (players see only identified streamers, SFUs see all);
  `subscribe`/`unsubscribe`/`subscribeFailed` (same texts); `playerConnected{dataChannel: true, sfu}`;
  `playerDisconnected`; `streamerIdChanged`; `streamerDisconnected` to SFUs; offer/answer/candidate
  relay; `ping`/`pong`; `disconnectPlayer` (close code 1011 with the reason); `layerPreference`
  (player → streamer, and streamer → its SFUs); the data channel handshake; `startStreaming`/`stopStreaming`.
- Player ids `Player0`, `Player1`, ...; a streamer's placeholder id `UnknownStreamer`; a taken id gets
  a number appended (unless reserved, see below).
- When a streamer leaves, its players' websockets are closed (that is how the frontend learns), and
  its SFUs are told with `streamerDisconnected`. When an SFU stops streaming, its players are closed.
- `max_subscribers` counts players only; SFUs do not take a slot.
- Player websockets are accepted on any path of the player port, next to the web server.
- The original protocol test suite (`Extras/SS_Test`) passes unchanged against wilbur.
- `pkg/protocol/schema_test.go` checks the message types against `signalling_messages.proto`.
  Three fields that real traffic carries are missing from the proto: `playerId` on
  `dataChannelRequest`, `peerDataChannelsReady` and `streamerDataChannels`. Two fields are declared
  narrower than real traffic: `ping`/`pong` `time` is `int32` but holds millisecond timestamps, and
  `playerId` is a string but PS1 sends numbers.

## Deliberately different

### Signalling

| Behaviour | Reference | Here | Why |
|---|---|---|---|
| Fields not in the protocol | relayed | dropped | no smuggling to the peer's parser |
| `playerId` in messages delivered to a player | left in (from streamers) | removed | it is routing data; recipients know who they are |
| Unknown message types | logged | dropped | allow-list per role |
| Streamer id characters | anything | letters, digits, space, `_ . : @ + ( ) / -`; ≤128 bytes | ids are shown in UIs and logged |
| Suffix for a taken id | max existing suffix + 1 | lowest free number | simpler; both unique |
| Reserved ids (`publish_as`) | n/a | exact id, takes over from the current holder; refused to everyone else | anti-squatting |
| `offer` from a player subscribed to an SFU | relayed (the SFU then resets itself) | dropped | a player could knock an SFU offline |
| Subscribe to a streamer outside your scope | n/a | "Streamer X does not exist." | no existence oracle |
| `iceCandidate` from an SFU | dropped | relayed to the named player, else upstream | lets SFUs trickle; the new SFU does not need it |
| `answer` from an SFU | `playerId` defaults to the SFU's | always the SFU's | an SFU only answers its own upstream |
| `disconnectPlayer` from an SFU | ignored | closes that SFU's own subscriber | lets the SFU refuse players |
| Player that never subscribes | auto-subscribed to the first streamer | only with `legacy_auto_subscribe: true` (and scoped) | explicit is safer; the stock frontend always subscribes |
| Browser websocket from another origin | accepted | refused unless in `allowed_origins` | cross-site websocket hijacking |
| `stats` (deprecated) | printed | dropped | |
| Liveness | optional protocol-level `ping` to players (`player_keepalive_timeout`) | websocket ping/pong on every connection | browsers and UE answer websocket pings automatically |

### Configuration and operations

Configuration is one strict YAML file instead of command-line flags and `config.json`:

| Wilbur | wilbur.yaml |
|---|---|
| `--player_port`, `--streamer_port`, `--sfu_port`, `--streamer_host`, `--sfu_host` | `listeners.{player,streamer,sfu}.addr` |
| `--serve`, `--http_root`, `--homepage` | `listeners.player.serve_static`, `static.root`, `static.homepage` |
| `--https`, `--https_port`, `--ssl_key_path`, `--ssl_cert_path`, `--https_redirect` | `listeners.player.tls`, `listeners.player.http_redirect_addr` |
| `--rest_api` | `listeners.player.api` (requires an admin credential) |
| `--peer_options` | `signalling.peer_options` / `peer_options_file` |
| `--turn_secret[_file]`, `--turn_ttl` | `turn.secret_file`, `turn.ttl` |
| `--player_token[_file]`, `--streamer_token[_file]` | `auth.tokens` with `roles: [player]` / `[streamer]` |
| `--max_players` | `signalling.max_subscribers` |
| `--log_level_console` | `log.level` (`log.format: json` for aggregators) |

Not carried over:

- **Log files and rotation** (`--log_folder`, `--log_max_*`). Logs go to stdout; leave files and
  rotation to systemd, Docker or your collector.
- **`--console_messages`**: message types are logged at `debug`. Payloads (SDP) are never logged.
- **`--stdin`, `--save`, `--log_config`.**
- **REST API shape.** The same paths (`/api/status`, `/api/streamers[/{id}]`, `/api/players[/{id}]`)
  but different JSON: entries carry `role`, `identity`, `remoteAddress`, ids, and subscriber ids.
  `/api/config` and the OpenAPI explorer are gone (the former exposed configuration).
- **CORS** for the API: it is for operators' tooling, not browsers on other origins.

## SFU

Same: the signalling behaviour (`identify` → `endpointId` → `listStreamers` → `subscribe` to
`subscribe_streamer_id` or the first streamer; retry every `retry_subscribe_delay`; `startStreaming`/
`stopStreaming`), the player offer with `sfu: true` and `scalabilityMode`, data channel setup for both
streamer kinds, `layerPreference` where 0 is the largest layer, and the old flags `--signallingURL` and
`--PublicIP`. The `config.js` fields map to `sfu.yaml`: `signallingURL` → `signalling_url`, `SFUId` →
`sfu_id`, `subscribeStreamerId` → `subscribe_streamer_id`, `retrySubscribeDelaySecs` →
`retry_subscribe_delay`, the mediasoup port range → `ice.port_min`/`port_max`.

Different:

| | mediasoup SFU | This SFU |
|---|---|---|
| Implementation | Node + native mediasoup worker | pure Go (Pion v4) |
| ICE | ICE-lite | full ICE, candidates in the SDP; optional single UDP port (`ice.udp_port`) |
| Simulcast order | needed PS2 layers reversed when RTX was present (#1195) | sorts rids numerically: PS1 and PS2 both work, with or without RTX |
| Layer adaptation | mediasoup's BWE | Pion's send-side BWE, step down on two low estimates, probe up with backoff; preference is a ceiling |
| Temporal layers / SVC dropping | supported | not implemented: single-encoding streams (incl. VP9 SVC) are passed through; `temporalLayer` ignored |
| RTX | supported | not negotiated; NACKed packets are retransmitted on the original stream |
| PS1 data channels | separate send/recv stream ids | one bidirectional stream id per player (PS1 accepts this) |
| Authentication to signalling | none | bearer token and/or client certificate, `wss://` with CA pinning |
| A refused or failed player | left hanging | `disconnectPlayer`, so the frontend reconnects (needs the new wilbur to take effect) |
| Offers and ICE candidates naming one of its players | offer taken as the streamer's | dropped |
| Streamer connection failure | waited | tears down, `stopStreaming`, `unsubscribe`, searches again |

## Tested

All on one Linux host, with the stock frontend's built `www` (Chrome 153 and Playwright's Firefox 159,
headless) and `cmd/teststreamer` standing in for Unreal:

| Scenario | Chrome | Firefox |
|---|---|---|
| Direct: streamer → wilbur → player (video, data channel, latency test round trip) | ✓ | ✓ |
| Through the SFU, PS2-style streamer (multiplexed channels), quality Low/Medium/High switching | ✓ | ✓ |
| Through the SFU, PS1-style streamer (per-player channels, `simulcast1..3` smallest first) | ✓ | ✓ |
| Three players through the SFU plus one direct, at once | ✓ ✓ ✓ | ✓ |
| TLS everywhere, mTLS-only streamer port, token-only players, admin-only API | ✓ | |
| SFU over `wss://` with a client certificate, pinned to its id, scoped player through it | ✓ | |
| `Extras/SS_Test` protocol suite against wilbur | ✓ (Node) | |

**Not yet tested against real Unreal Engine.** The test streamer mimics PS2/PS1 signalling, simulcast
and data channel framing as read from the engine source, but UE 5.x itself (EpicRtc for PS2,
libwebrtc for PS1), H.264 and VP9 streams, audio from UE, bandwidth-limited links (BWE behaviour) and
Windows have not been exercised. Those are the first things to run.
