# Sign-in and tokens

Packages: `oauth`, `seal`, `gh` (GitHub's side of OAuth), `store`.

[The authentication research](../research/authentication.md) explains why speki runs its own
authorization server in front of GitHub. This document describes how.

## Endpoints

All URLs are built from `public_url` in the configuration, never from request headers.

| Method, path | Purpose |
|---|---|
| `GET /.well-known/oauth-protected-resource` and `.../oauth-protected-resource/mcp` | Protected Resource Metadata (RFC 9728) |
| `GET /.well-known/oauth-authorization-server` | Authorization server metadata (RFC 8414) |
| `POST /oauth/register` | Dynamic client registration (RFC 7591), stateless, local apps only |
| `GET /oauth/authorize` | Validates the request and shows the consent page |
| `POST /oauth/authorize` | Consent given. Redirects to GitHub |
| `GET /oauth/github/callback` | GitHub returns here. Redirects back to the client with a code |
| `GET /oauth/fonts/{name}` | The fonts the consent and error pages use |
| `POST /oauth/token` | Code and refresh grants |

Resource metadata:

```json
{ "resource": "https://speki.example.com/mcp",
  "authorization_servers": ["https://speki.example.com"],
  "scopes_supported": ["speki:read", "speki:write"],
  "bearer_methods_supported": ["header"] }
```

Authorization server metadata advertises: `S256` as the only PKCE method, `none` as the only token
endpoint auth method (all clients are public), the `authorization_code` and `refresh_token` grants,
`client_id_metadata_document_supported: true`, `authorization_response_iss_parameter_supported: true`,
and the registration endpoint.

## Sealed values

Everything speki would otherwise have to store is handed out sealed instead. `seal` does one thing:

```go
// Package seal
type Purpose string // "client", "consent", "transaction", "code", "access", "refresh"

func (k *Keyring) Seal(p Purpose, v any, expires time.Time) (string, error)
func (k *Keyring) Open(p Purpose, s string, v any) error
```

Format: `v1.<key id>.<base64url(nonce || ciphertext)>`. The payload is JSON with an `exp` field (none for
`client`). The cipher is AES-256-GCM with a key derived per purpose with HKDF-SHA256 from the keyring key,
and the purpose and key ID as associated data. A refresh token can't be presented as an access token, and
the format has no algorithm field to tamper with.

The transaction carries the nonce that names its cookie and is GitHub's `state`, so a transaction cookie
from another sign-in doesn't fit. The code carries the resource it was issued for. The consent value
records whether the client asked for `speki:write`; when it asked only for `speki:read`, the consent page
offers read-only access and nothing else.

`Open` allows 60 seconds of clock skew on `exp`, for instances whose clocks drift a little.

| Purpose | Contents | Lifetime | Held by |
|---|---|---|---|
| `client` | client name, redirect URIs | none | a local MCP client, as its `client_id` |
| `consent` | the validated authorize request, hash of the consent cookie | 10 minutes | consent form |
| `transaction` | authorize request, chosen scopes, GitHub PKCE verifier, nonce | 10 minutes | `__Host-speki_tx_<nonce>` cookie |
| `code` | ID, user, GitHub tokens, client, redirect URI, PKCE challenge, scopes, resource, sign-in time | 60 seconds | MCP client, briefly |
| `access` | user ID and login, GitHub access token, scopes, client name, audience | 1 hour, or until the GitHub token expires if sooner | MCP client |
| `refresh` | user ID and login, GitHub refresh token, scopes, client ID, client name, audience, sign-in time | until 90 days after sign-in, or until the GitHub refresh token expires if sooner | MCP client |

Access tokens are not JWTs. Nobody but speki needs to read them.

The sign-in time in `refresh` doesn't change on refresh, so every chain of refresh tokens ends 90 days
after the user last signed in. A stolen refresh token can't be kept alive forever.

## Clients

Who a client is decides who gets the code at the end, so speki is strict about it.

Web clients identify with a Client ID Metadata Document: their `client_id` is an `https://` URL serving a
JSON description of the client. Speki accepts these only from hosts listed in `auth.client_metadata_hosts`
(default: `claude.ai`). The consent page names the client by that host, which the client can't fake. Speki
fetches the document with an HTTP client that:

- only connects to the listed hosts, on port 443, without a proxy
- refuses private, loopback, link-local, CGNAT, unspecified and NAT64 addresses after resolving, with
  IPv4-mapped addresses unmapped first
- follows no redirects, times out after 5 seconds, reads at most 64 KiB
- requires the document's `client_id` to equal its URL, and its redirect URIs to be either `https` on the
  same host or loopback (Claude Code's document lists loopback redirects)

Parsed documents (name and redirect URIs only) are kept in a per-instance cache of 100 entries and
refetched after 10 minutes. The refetch runs in the background, and the old document is used meanwhile,
and for up to an hour while the host can't be reached or answers with an error. A 404 or 410 means the
document was removed, so the old one is dropped. Failed fetches are remembered on their own: a missing
or invalid document for a minute, an unreachable host for 10 seconds.

At most 4 fetches of new documents run at once, with 2 more slots for refetching documents that were
fetched before, so a stream of made-up URLs can't keep claude.ai out. A request waits up to 2 seconds for
a slot, and stops waiting when its client leaves.

Other local clients, the ones that don't use metadata documents, register with
`POST /oauth/register`. Registration only accepts loopback redirect URIs (`http://127.0.0.1`,
`http://[::1]`, `http://localhost`, any port) and answers with a `client_id` that is the sealed
registration. Nothing is stored. The consent page labels these "a program on this computer", shows the
name the program gave itself as unverified, and warns that only the user's own tools should get access.

Why the split: a registered client with an `https` redirect could call itself "Claude Code" and send the
code anywhere. Limiting `https` redirects to known hosts, and showing the host instead of a
self-chosen name, turns that phishing route into a dead end.

## The authorization flow

```text
client           speki                         GitHub
  │ GET /authorize  │                              │
  ├────────────────►│ check client, redirect_uri,  │
  │                 │ PKCE S256, resource          │
  │  consent page   │ set __Host-speki_consent     │
  │◄────────────────┤                              │
  │ POST /authorize │                              │
  ├────────────────►│ check consent value + cookie │
  │                 │ set __Host-speki_tx_<nonce>  │
  │  302 to GitHub  │ (state = nonce)              │
  │◄────────────────┤                              │
  ├───────────────────────────────────────────────►│ user approves (first time)
  │  302 /oauth/github/callback?code&state         │
  ├────────────────►│ tx cookie for state exists   │
  │                 │ exchange code + verifier ───►│
  │                 │◄──── ghu_, ghr_, expiries ───┤
  │                 │ GET /user, org gate ────────►│
  │  302 redirect_uri?code&state&iss               │
  │◄────────────────┤                              │
  │ POST /token     │                              │
  ├────────────────►│ code unused, PKCE, client,   │
  │  access+refresh │ redirect_uri, resource match │
  │◄────────────────┤                              │
```

Details per step:

- `GET /authorize` checks everything before showing anything. An unknown client or a redirect URI that
  doesn't match exactly (loopback: any port) gets an error page. It never redirects, so speki can't be used
  as an open redirector. Other errors (missing PKCE, wrong `resource`) redirect to the client with an OAuth
  error, as the spec says.
- Client names lose control characters, line separators, bidi controls and tag characters, which could
  hide or reorder text. Other format characters (zero-width spaces and joiners, the word joiner, soft
  hyphens) are kept: emoji and some scripts need them, and they can't reorder anything.
- The consent page shows who is asking (host, or "a program on this computer"), the redirect target, a
  "read and write" / "read only" choice, and a continue button. It sends
  `Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'; font-src 'self'; frame-ancestors 'none'`
  and loads nothing from elsewhere. There's no `form-action`: Chromium applies it to the redirect that
  follows the form, to GitHub or back to the client, and would block sign-in. The page is an
  `html/template` embedded in the binary.
- The consent and error pages use Alegreya and Alegreya Sans (SIL Open Font License), embedded in the
  binary and served from `GET /oauth/fonts/{name}`. A margin lists the three steps of signing in and
  marks the current one; an error page marks the step that failed.
- The cookies are `__Host-`, `Secure`, `HttpOnly`, `SameSite=Lax`. The consent POST must carry a sealed
  consent value whose cookie hash matches, which stops cross-site form posts and replays from another
  browser. The transaction cookie's name includes its nonce, so two sign-ins in one browser don't
  overwrite each other. The callback clears it.
- The redirect to GitHub carries `prompt=select_account`, so GitHub shows its account picker even after
  the app was approved. Someone with a personal and a work account then picks the right one, instead of
  getting whichever the browser is signed in with.
- The consent and error pages name the instance by `instance.title` when it's set. Error pages after the
  GitHub sign-in say which GitHub login was used.
- At the callback, speki refuses GitHub tokens without an expiry. It calls `GET /user` and, when
  organisations are required, checks membership right away, so someone outside the organisation gets a
  clear page instead of a client that fails on every call.
- When a code is redeemed, its ID goes into the store with `SetNX` and a 5-minute TTL, comfortably longer
  than the code's lifetime plus clock skew. A second redemption fails with `invalid_grant`.

## Refreshing

`POST /token` with `grant_type=refresh_token`:

1. Open the refresh token and check its client ID against the request's.
2. Look in the store for a recent result for this token (keyed by the token's hash). If there is one,
   return it. This covers two windows of one client refreshing at once, and a client retrying after a lost
   response.
3. Take a lock in the store on the token's hash (25 seconds). If another request holds it, wait for its
   result (step 2) for up to 25 seconds.
4. Refresh with GitHub, within 20 seconds of taking the lock. A request that fails looking GitHub up,
   connecting or in the TLS handshake is tried up to three times in all, a moment apart: nothing of it
   was sent, so the token is still good. One that may have been sent is never tried again. A
   `bad_refresh_token` becomes `invalid_grant`, which sends the user to sign in again.
5. Seal the new pair. Store it for a minute under the old token's hash, encrypted with a key derived
   from the old refresh token itself, so the store (Redis, in a cluster) never holds a usable token.
   Return it. Storing should finish before the lock runs out. If it fails twice, speki stores a marker
   instead, and a request waiting with the same token gets `temporarily_unavailable`: the client may
   already hold the new pair from the first request (another window of it, say). Once the marker expires,
   the old token fails with `invalid_grant`, since the GitHub token inside it is spent. If even the marker
   can't be stored, the store is down, and the waiting request most likely fails with `server_error`.

When GitHub doesn't answer in time, or the connection drops after the request may have been sent, the
client gets `temporarily_unavailable`. GitHub may have spent the refresh token anyway, and it has no grace
period for the old one, so the client's retry may get `invalid_grant` and the user has to sign in again.
Speki can't prevent that, only make it rare and visible: it remembers such a refresh for 10 minutes under
the token's hash (no token material), and when GitHub then refuses the same token, it logs a warning,
`refresh lost to GitHub timeout`, and counts it in `speki_refreshes_total{result="lost_to_timeout"}`
instead of as an ordinary sign-out.

The lock, the results and these markers are in the store. With the memory store each instance has its
own, so two instances refreshing one token both ask GitHub, and the second signs the user out. Speki
warns about the memory store at startup and on the status page.

Every speki refresh is a GitHub refresh. GitHub's single-use refresh tokens give us rotation and replay
detection: after the real client has refreshed, an old refresh token is dead, because the GitHub token
inside it is dead.

Scopes on refresh can only shrink, never grow.

## The token check

`oauth.Middleware` plugs a verifier into the go-sdk's `auth.RequireBearerToken`, which writes the right
`WWW-Authenticate` header with the `resource_metadata` URL on failure.

The verifier opens the token as `access`, checks `aud` against the configured resource, and returns the
`oauth.Identity`. It makes no GitHub call. GitHub is asked when the grant is resolved (see
[architecture.md](architecture.md)), at most once every 5 minutes per GitHub token. A revoked GitHub authorization
fails there, and speki answers 401 so the client refreshes, which fails and sends the user to sign in.

Scopes: the `Grant` of a token without `speki:write` has no writable vaults. Write tools then return the
`read_only_session` tool error, which tells the agent to ask the user to reconnect with write access. Speki
doesn't use HTTP-level scope step-up: it would mean reading tool names out of requests before the MCP
handler, and few clients handle it today.

## Keys

`seal.Keyring` loads the key file described in [running.md](../functional/running.md). It refuses to start
without exactly one primary key, with keys shorter than 32 bytes, or with duplicate IDs. Every instance in
a cluster must have the same key file. A token sealed by one instance opens on any other.

`auth.key` is the alternative to a key file: `seal.SingleKey` makes a keyring of that one key, with the
ID `default`. Since every sealed value names its key's ID, a key file that lists the same secret as
`default` opens everything the single key sealed, which is how an operator moves to a key file to rotate.

## Revocation

Speki can't revoke a single one of its own tokens before it expires, because it keeps no list of them. What
it can do:

- A user revokes the speki app in their GitHub settings. The GitHub token inside every speki token they
  hold dies, and speki notices within 5 minutes.
- An operator removes a key from the key file. Every token sealed with it stops working.
- When a key may have leaked, the operator also revokes all user tokens of the GitHub App in GitHub's app
  settings, because whoever holds the key could open captured tokens and use the GitHub token inside
  directly.
