# Architecture

```
             ┌──────────────────────────────── wilbur ────────────────────────────────┐
 browsers ──▶│ player listener :8080 ─┬─ websocket ─▶ signalling.Server.Handler(player) │
             │                        ├─ /api/  ─────▶ web.API          (admin role)    │
             │                        └─ static ─────▶ web.Static       (os.Root)       │
 Unreal ────▶│ streamer listener 127.0.0.1:8888 ────▶ signalling.Server.Handler(streamer)
 SFU ───────▶│ sfu listener 127.0.0.1:8889 ─────────▶ signalling.Server.Handler(sfu)    │
             └────────────────────────────────────────────────────────────────────────┘
                     every Handler: auth.Authenticator ─▶ Hooks.OnConnect ─▶ upgrade ─▶ wsconn.Conn
```

## Packages, bottom up

**`pkg/protocol`** holds one Go struct per message in `signalling_messages.proto`. `Decode` reads the
`type`, unmarshals into that struct and validates field sizes; `Encode` sets `type` and marshals. Since
everything passes through the structs, unknown fields are dropped. `PlayerID` accepts JSON strings
and numbers (old streamers send numbers), and `ping.time` is echoed as the exact number received
(browsers send millisecond timestamps that overflow the proto's `int32`).

**`pkg/wsconn`** wraps a `coder/websocket` connection. `Run` reads frames on the caller's goroutine,
so one connection's messages are handled in order and never concurrently, and throttles them with a
token bucket. `Send` encodes and enqueues without blocking. A writer goroutine drains the queue, and a
full queue closes that connection. `Close` flushes the queue before the close frame, so a
`subscribeFailed` followed by a close arrives. A ping loop drops dead peers.

**`pkg/auth`** defines `Authenticator` and `Identity` (see [AUTH.md](AUTH.md)). `Chain` composes
authenticators with a per-role anonymous policy. Each method is a small file with no dependency on
the rest of the project.

**`pkg/signalling`** is the server. A `peer` is one connection; a streamer has the streamer half
(`streamerID`, `subscribers`), a player the player half (`playerID`, `upstream`), and an SFU both. The
`Server` keeps two maps (streamers by id, players by id) behind one mutex.

- `server.go`: the connection lifecycle. Authenticate, `OnConnect`, admission (caps), upgrade,
  register (assign ids, send `identify`/`config`), `Run`, unregister (undo subscriptions, notify).
- `routing.go`: what each role may send, and what happens to it. This is the file to read to
  understand the protocol, and the one to audit for isolation.
- `hooks.go`: the extension points.

**`pkg/web`** has `Static` (the player page), `API` (read-only JSON for operators) and middleware
(headers, rate limit, trusted-proxy client IP, HTTPS redirect).

**`pkg/sfu`** is the SFU. It connects to the signalling server as a client, as a player of its upstream
streamer and as a streamer to its own players.

- `sfu.go`: the public API (`New`, `Run`, `Hooks`), the signalling session with reconnect and
  backoff, message dispatch, the streamer search, the player registry.
- `streamer.go`: the upstream peer connection. It receives simulcast layers (sorted largest first by
  rid number, which suits both PS1's `simulcast1..N` and PS2's `simulcast0..N-1`), measures each
  layer's bitrate, rate-limits keyframe requests, and owns the streamer side of the data channels.
- `player.go`: one player's peer connection, its offer, its layer-selection loop and its data
  channel relay.
- `forwarder.go`: per player, rewrites SSRC, sequence numbers and timestamps so a layer switch looks
  like one continuous stream, and switches only on a keyframe of the new layer.
- `bwe.go`: chooses a layer from Pion's send-side bandwidth estimate, the measured layer bitrates and
  the player's preference (a ceiling).
- `keyframe.go`: keyframe detection for VP8, VP9, H264 and AV1.
- `datarelay.go`: PS2's multiplex framing (`199` + UTF-16 player id + payload; `198` relay status)
  and bounded queues.

The SFU answers the streamer and offers to players with candidates already in the SDP, because the
reference Wilbur does not relay ICE candidates from an SFU.

## Message flow

```
Streamer                     wilbur                        Player
   │◀──── identify, config ────│                              │
   │──── endpointId{id} ──────▶│ (scope check: PublishAs)      │
   │◀── endpointIdConfirm ─────│                              │
   │                           │◀──────── config ─────────────│ (sent on connect)
   │                           │◀──── listStreamers ──────────│
   │                           │───── streamerList ──────────▶│ (filtered by scope)
   │                           │◀──── subscribe{id} ──────────│ (scope + AuthorizeSubscribe)
   │◀─ playerConnected{pid} ───│                              │
   │──── offer{pid,sdp} ──────▶│── offer{sdp} ───────────────▶│ (pid must be a subscriber; stripped)
   │◀─── answer{pid,sdp} ──────│◀──────── answer{sdp} ────────│ (pid stamped by the server)
   │◀─▶ iceCandidate ... ─────▶│◀─▶ iceCandidate ... ────────▶│
```

Through an SFU, the SFU is the player of the streamer (`playerConnected{sfu:true}`) and the streamer of
the players. After its answer, a player sends `dataChannelRequest`; the SFU replies
`peerDataChannels{send,recv}`, the player opens those negotiated channels and sends
`peerDataChannelsReady`. For a PS1 streamer the SFU then sends `streamerDataChannels` (the server
fills in `sfuId`) and opens a per-player channel to the streamer. PS2 streamers instead use one pair
of channels for all players, multiplexed.

## Extending and embedding

**As a library.** `signalling.New(cfg, authenticator, hooks)` returns a server whose
`Handler(role)` you mount on any listener or mux, behind any middleware. `examples/embed` is a
complete multi-tenant server in about 80 lines. The SFU is the same: `sfu.New(cfg, logger)`, set
`Hooks`, then `Run(ctx)`.

**Hooks** (`signalling.Hooks`), all optional, called without the server lock from the connection's
own goroutine:

| Hook | When | Use |
|---|---|---|
| `OnConnect(r, info) error` | after authentication, before upgrade | quotas, bans, extra checks; error → 403 |
| `OnRegistered(info)` | ids assigned, config sent | metrics, audit |
| `OnDisconnect(info)` | connection gone, subscriptions undone | metrics, audit, billing |
| `OnMessage(info, msg) bool` | each validated message | inspect, rewrite or drop |
| `AuthorizeSubscribe(info, id) error` | player/SFU subscribe | per-request policy |
| `AuthorizeStreamerID(info, req) (string, error)` | streamer/SFU identifies | allow, rewrite (tenant prefix), refuse |
| `PeerOptions(info) json.RawMessage` | building a peer's `config` | per-peer ICE servers or credentials (runs under the lock: do not call back into the Server) |

**Authenticators**: implement one method; see [AUTH.md](AUTH.md).

**State**: `Server.Snapshot()` returns streamers and players with their identities, for your own APIs
or metrics.

**Protocol extensions**: add a struct and a registry entry in `pkg/protocol`, then a case in the
relevant `from*` function in `routing.go`. Nothing is relayed unless a case says so.

## Design choices

- **Go and Pion.** Single static binaries for Linux, Windows and macOS. A small, readable standard
  library HTTP and TLS stack. Pion means no native mediasoup worker (the source of the reference
  SFU's prebuilt and kernel problems).
- **One lock.** Signalling is a few messages per second per connection. One mutex around the routing
  state, with non-blocking sends, is simpler to verify than fine-grained locking and has no
  measurable cost at this scale.
- **Typed messages over pass-through JSON.** A few more lines than forwarding raw objects, but
  validation is in one place and nothing unexpected reaches a peer.
- **Separate listeners per role**, rather than one port with a role parameter: a role is decided by
  where you connected, which a firewall can enforce, not by something the client says.
