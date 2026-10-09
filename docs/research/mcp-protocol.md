# MCP protocol and Go SDK

Researched 2026-10-09. Links point at the revision we target.

## Which spec revision

The current MCP revision is `2026-07-28`. It is the first stateless one, and it changes enough that we
build for it directly instead of treating it as an upgrade later.

What changed compared to `2025-11-25`:

- Sessions are gone (SEP-2567). There is no `Mcp-Session-Id`. A server that needs state across calls hands
  out its own handles as ordinary tool arguments.
- The `initialize`/`initialized` handshake is gone (SEP-2575). Every request carries the protocol version,
  client capabilities and client info in `_meta`. A new `server/discover` call returns the supported
  versions, capabilities and the server `instructions`.
- Streamable HTTP is POST only. Each POST gets a JSON body or an SSE stream for that one request. GET and
  DELETE return 405.
- Requests carry `MCP-Protocol-Version`, `Mcp-Method` and `Mcp-Name` headers. When they disagree with the
  body the server answers 400 with error -32020.
- List results can be cached by the client (`ttlMs`, `cacheScope`).
- Server-to-client requests (elicitation, sampling) became multi round-trip requests: the server returns
  `resultType: "input_required"` plus an opaque `requestState`, and the client retries. Speki doesn't use
  it.

Sources: [changelog](https://modelcontextprotocol.io/specification/2026-07-28/changelog),
[Streamable HTTP](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http),
[server/discover](https://modelcontextprotocol.io/specification/2026-07-28/server/discover).

### Older clients

Not every Claude surface speaks `2026-07-28` yet. Claude Code does. Anthropic's connector docs still list
the `2025-03-26`, `2025-06-18` and `2025-11-25` authorization specs for claude.ai and Desktop
([connector docs](https://claude.com/docs/connectors/building/index.md)). Clients probe with a modern request
and fall back to `initialize`, so speki has to answer both on the same endpoint. The official Go SDK
does this for us (see below).

Speki never relies on a session, so the legacy path costs us nothing beyond what the SDK does.

## Announcing what speki is for

The `instructions` string is returned by `server/discover` and, for legacy clients, in the
`initialize` result. We send the same text in both.

Things worth knowing about how Claude uses it:

- Claude Code loads only tool names and server instructions at session start, then searches for tools
  when it needs them. The instructions are what make it think of speki in the first place.
- Claude Code cuts each server's instructions and each tool description at 2,048 characters. The
  important part goes first.
- `_meta["anthropic/alwaysLoad"]: true` on a tool keeps it loaded up front. That fits `search`.
- Tool output is capped at about 25k tokens unless `_meta["anthropic/maxResultSizeChars"]` raises it.
  `read` has to page large documents either way.

Source: [Claude Code MCP docs](https://code.claude.com/docs/en/mcp).

There is a limit on how hard we can push. Anthropic's connector review rejects descriptions that
interfere with Claude calling other tools or try to override system instructions
([review criteria](https://claude.com/docs/connectors/building/review-criteria.md)). So the instructions
say plainly what speki holds and when to look there first ("before answering questions about our
systems, processes or decisions, search speki"), and stop short of telling the model to ignore
other sources.

The [server instructions guidance](https://blog.modelcontextprotocol.io/posts/2025-11-03-using-server-instructions/)
says to use instructions for how tools fit together and leave per-tool detail to tool descriptions. We
follow that, and put the longer explanation in the `guide` tool.

Tool annotations matter for how clients prompt the user. Read tools get `readOnlyHint: true`, which lets
clients run them without asking. `delete` and `move` get `destructiveHint: true`. Every tool gets a
`title`.

Server Cards (SEP-2127) are final but on the extensions track, and don't carry tools. Speki doesn't
publish one.

## Authorization in the spec

The parts that shape our design:

- OAuth 2.1 with PKCE (S256).
- The MCP server publishes Protected Resource Metadata (RFC 9728) and points to it from the
  `WWW-Authenticate` header on a 401.
- Clients send the RFC 8707 `resource` parameter. The server must check that tokens were issued for it
  and must not accept or forward tokens meant for someone else ("token passthrough").
- Client registration, in the client's order of preference: pre-registered client, Client ID Metadata
  Documents (CIMD), then Dynamic Client Registration (DCR). DCR is now marked deprecated but still
  common.
- Scope step-up works through a 403 with `insufficient_scope`.

Source: [authorization](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization).

What Claude supports ([connector auth docs](https://claude.com/docs/connectors/building/authentication.md)):

- CIMD, when the authorization server advertises `client_id_metadata_document_supported: true` and
  `none` in `token_endpoint_auth_methods_supported`. Otherwise DCR.
- PKCE S256 always.
- Callback `https://claude.ai/api/mcp/auth_callback` for the hosted apps. Claude Code uses a loopback
  redirect on any port.
- Refresh on a 401 or shortly before expiry, and expects `invalid_grant` for a dead refresh token.

GitHub can't be the authorization server for MCP clients directly. Its metadata at
`https://github.com/.well-known/oauth-authorization-server/login/oauth` has no registration endpoint
and no CIMD support, and the code exchange needs a client secret. GitHub's own remote MCP server has the
same problem, and its Claude Desktop guide says remote use "is not currently supported"
([install guide](https://github.com/github/github-mcp-server/blob/main/docs/installation-guides/install-claude.md)).

So speki has to be its own authorization server that sends people through GitHub to sign in. The spec
covers this case under "MCP proxy servers" in its
[security best practices](https://modelcontextprotocol.io/docs/tutorials/security/security_best_practices).
[authentication.md](authentication.md) works it out.

## Go SDK

Two options:

| | `github.com/modelcontextprotocol/go-sdk` | `github.com/mark3labs/mcp-go` |
|---|---|---|
| Latest | v1.8.0, 2026-09-14 | v1.2.1, 2026-10-09 |
| `2026-07-28` | Since v1.7.0, with `StreamableHTTPOptions{Stateless: true}` | Since v1.0.0 |
| Legacy clients | Same endpoint negotiates `2025-11-25` | Yes |
| Auth helpers | `auth.RequireBearerToken`, `auth.ProtectedResourceMetadataHandler`, `TokenInfoFromContext` | Fewer |
| Maintained by | The MCP project, with Google | Community |

We use the official SDK. It tracks the spec, ships the resource-server half of auth, and GitHub's MCP
server runs on it. It has no authorization-server helpers, so that part is ours either way.

One thing to remember: v1.8 removed built-in `Origin` checking. We wrap the handler with
`http.NewCrossOriginProtection` from the standard library (Go 1.25+) ourselves. That rejects cross-origin
browser requests. It doesn't add CORS headers, which speki doesn't need: Claude's apps call MCP servers from
their own backend.

Sources: [v1.7.0](https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.7.0),
[v1.8.0](https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.8.0).
