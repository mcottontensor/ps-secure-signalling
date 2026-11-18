# Authentication and authorization

Every websocket upgrade and every REST API request is authenticated before anything else happens. A
connection that fails gets `401` at the HTTP upgrade: it never becomes a websocket and is never sent
the `config` message.

## Roles and listeners

Each listener asks for exactly one role:

| Listener | Role | Who |
|---|---|---|
| player (`:8080`) websocket | `player` | browsers and other viewers |
| player (`:8080`) `/api/` | `admin` | operators and tooling |
| streamer (`127.0.0.1:8888`) | `streamer` | Unreal Engine instances |
| sfu (`127.0.0.1:8889`) | `sfu` | forwarding units |

A credential grants a set of roles. A player token presented on the streamer port is refused, even
though it is a valid token.

## Identity

Authentication produces an `auth.Identity`, which stays with the connection:

| Field | Meaning |
|---|---|
| `Subject` | who: token subject, JWT `sub`, certificate CN, webhook `subject` |
| `Roles` | what it may connect as |
| `Streamers` | streamer ids it may list and subscribe to. `path.Match` patterns allowed (`tenant-a/*`). Empty = all. |
| `PublishAs` | exact streamer ids it may register as. Empty = any, with the legacy suffixing (`Game`, `Game1`, ...) |
| `ExpiresAt` | when the credential lapses (closes the connection if `close_on_expiry`) |
| `Claims` | anything else, for your hooks |

**Subscribe scope.** `Streamers` filters what `listStreamers` returns and what `subscribe` accepts.
A refused subscribe gets the same `subscribeFailed` text as a streamer that does not exist, so a
player cannot probe for other tenants' streamers.

**Reserved streamer ids.** With `PublishAs` set, a streamer may only register as those ids. It always
gets exactly the id it asked for (never a suffixed one), and if another connection holds that id,
that connection is closed (code 4002) and its players dropped. If the streamer sends an empty id and
has exactly one reserved, it gets that one.

Reserved ids are also off limits to everyone else, even while their owner is offline: identities
without `PublishAs` are refused (code 4001) if they ask for one. The reserved set is every
`publish_as` of a static token, plus every id a `PublishAs` identity (JWT, mTLS, webhook) has
registered since the server started. To protect ids that only JWT or webhook identities hold before
they first connect, use `signalling.Config.ReservedStreamerIDs` when embedding. If a streamer is
renamed while players are subscribed, players whose scope does not cover the new id are disconnected.

## Methods

Configure any combination under `auth:` in the wilbur config (see `deploy/wilbur.example.yaml`). The
chain tries them in order; the first identity that grants the requested role wins. If any credential
was presented and rejected, and none accepted, the request is refused even where anonymous access is
allowed: a wrong token is a mistake to surface, not a reason to fall back to anonymous.

### Where tokens are read from

Bearer tokens (static or JWT) are read from, in order:

1. `Authorization: Bearer <token>`: tooling, native clients, proxies
2. the `?token=` query parameter (`auth.token_query`): browsers, which cannot set websocket headers,
   and Unreal (`-PixelStreamingURL=wss://host:8888/?token=...`)
3. a cookie (`auth.token_cookie`, off by default): a login page on the same origin

The REST API reads only the `Authorization` header, so admin credentials never appear in URLs.

The stock frontend connects wherever `?ss=` points, so a player link with a token looks like
`https://host/?ss=wss%3A%2F%2Fhost%2F%3Ftoken%3D<token>`. Tokens in URLs end up in browser history
and proxy logs. Prefer short-lived JWTs minted per session, or a cookie.

### Static tokens

```yaml
tokens:
  - { token_file: /run/secrets/ue1.token, subject: ue1, roles: [streamer], publish_as: [DefaultStreamer] }
  - { token_file: /run/secrets/viewers.token, subject: viewers, roles: [player], streamers: [DefaultStreamer] }
```

Tokens must be at least 16 characters. They are compared in constant time, and every configured token
is checked on each attempt.

### JWT

```yaml
jwt:
  hmac_secret_file: /run/secrets/jwt.key   # HS256, at least 32 bytes
  # and/or
  oidc_issuer: https://login.example.com    # JWKS found by discovery, refreshed in the background
  jwks_url: https://login.example.com/jwks  # or directly
  audience: pixelstreaming
  leeway: 30s
```

Tokens must have an `exp`. Set `audience` when using an identity provider shared with other
applications; wilbur warns if you do not. Accepted algorithms default to HS256 with an HMAC secret and
RS256/PS256/ES256/EdDSA with public keys; `none` is never accepted, and an HMAC token is never
checked against a public key. Claims (names configurable):

```json
{ "sub": "alice", "exp": 1767225600, "aud": "pixelstreaming",
  "ps_roles": ["player"], "ps_streamers": ["DefaultStreamer"], "ps_publish": [] }
```

Each of `ps_roles`, `ps_streamers` and `ps_publish` may be a list or a space-separated string.

### Client certificates (mTLS)

```yaml
listeners:
  streamer:
    addr: "0.0.0.0:8888"
    tls: { cert_file: server.pem, key_file: server.key, client_ca_file: ca.pem, require_client_cert: true }
auth:
  mtls: { roles: [streamer, sfu], publish_as_subject: true }
```

Only certificates the listener verified against `client_ca_file` count. `publish_as_subject` reserves
the certificate's CN as its streamer id, so a CA can issue one certificate per streamer. The SFU
supports client certificates natively (`tls.cert_file` in its config). Unreal does not, so for
streamers put a TLS-terminating sidecar (stunnel, Envoy) next to each instance, or use tokens.
`deploy/gen-dev-certs.sh` makes a throwaway CA for trying it out.

### Webhook

```yaml
webhook:
  url: https://auth.internal/pixelstreaming
  secret_file: /run/secrets/webhook.key
  timeout: 5s
```

wilbur POSTs, for every connection attempt:

```json
{ "role": "player", "remoteAddr": "203.0.113.9:51234", "path": "/", "origin": "https://play.example.com",
  "tokens": ["..."], "headers": { "Authorization": "...", "Cookie": "..." },
  "clientCert": "CN=...", "timestamp": 1763452800 }
```

with `X-PS-Timestamp`, and `X-PS-Signature: sha256=<hex HMAC-SHA256 of the body>` when a secret is
set. Verify the signature and reject stale timestamps. Reply `200` with:

```json
{ "allow": true, "subject": "alice", "roles": ["player"], "streamers": ["DefaultStreamer"],
  "publishAs": [], "expiresAt": 1763456400, "claims": { "plan": "pro" } }
```

`roles` is required: an allow that grants no roles is refused, so a webhook written with players in
mind cannot grant `admin` by omission. Check `role` in the request and grant only what you mean to.
Anything other than a `200` with `"allow": true` and roles, including a timeout or an unreachable
service, refuses the connection. Redirects are not followed. At most 32 calls are in flight; further
attempts are refused rather than queued. `remoteAddr` is the client address after `trusted_proxies`
are applied.

### Anonymous access

```yaml
anonymous: [player]
```

Roles listed here may connect without credentials. The default, `[player, streamer, sfu]`, suits a
single machine and is only safe because the streamer and SFU listeners default to loopback. wilbur
refuses to start if a streamer or SFU listener is not on loopback while its role is anonymous, unless
`allow_insecure: true`. `admin` can never be anonymous.

## Your own authenticator

Anything with this method can join the chain:

```go
type Authenticator interface {
    Authenticate(ctx context.Context, r *http.Request, role auth.Role) (*auth.Identity, error)
}
```

Return `auth.ErrNoCredentials` when the request carries nothing you handle, any other error to refuse
it, or an Identity. See `examples/embed` for a multi-tenant example.

## Authorization hooks

For policy beyond scopes, `signalling.Hooks` has:

- `AuthorizeSubscribe(info, streamerID) error`: runs after the identity's scope allowed it
- `AuthorizeStreamerID(info, requested) (string, error)`: allow, rewrite (for example prefix with a
  tenant) or refuse a streamer's id
- `OnConnect(r, info) error`: refuse a connection after authentication (quotas, bans, geo)
- `OnMessage(info, msg) bool`: inspect, modify or drop any message
