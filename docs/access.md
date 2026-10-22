# Access

Users sign in with their GitHub account, and GitHub decides what they can see and change. Speki keeps no
users, sessions or approvals of its own: everything it needs to know about a signed-in user is inside the
token their client holds, sealed so only speki can open it.

## Signing in

When an MCP client connects for the first time, it gets a 401, finds speki's sign-in metadata, and opens a
browser:

1. Speki's consent page says who is asking for access and where the token will go. For web apps like
   claude.ai that's the site's address. For a program on the user's computer it says so, shows the name the
   program gave itself as unverified, and warns that only the user's own tools should get access. The user
   picks read and write (the default) or read only.
2. The user continues to GitHub, which always shows its account picker, so someone with a personal and a
   work account picks the right one. The first time, GitHub also asks them to authorize the speki app.
3. Speki checks the GitHub account, including organization membership when required. Someone who isn't
   allowed in gets a page saying so, naming the GitHub login they used, instead of a client that fails on
   every call.
4. The browser returns to the client and the client is connected.

The consent page shows on every sign-in, even for clients the user approved before. That's deliberate: GitHub
remembers the user's approval of the speki app, so without speki's own page another program could quietly
get a token for the user by sending them a link.

Clients refresh their access on their own. A user signs in again 90 days after signing in, or sooner if they
revoke the speki app in their GitHub settings.

### Why speki runs its own sign-in

MCP clients sign in with OAuth 2.1 against an authorization server they can register with, either through a
client metadata document or dynamic registration, and they expect tokens bound to the MCP server. GitHub's
OAuth supports neither kind of registration, and its code exchange needs the GitHub App's client secret. So
speki runs a small authorization server in front of GitHub, which the MCP spec covers as a proxy server. The
client never holds a GitHub token; it holds speki's token, with the GitHub token sealed inside, so this isn't
token passthrough.

### Which clients may sign in

Who a client is decides who gets the code at the end, so speki is strict about it.

Web clients identify with a client metadata document: their client ID is an `https://` URL serving a JSON
description of the client. Speki accepts these only from hosts listed in `auth.client_metadata_hosts`
(`claude.ai` by default), and the consent page names the client by that host, which the client can't fake.
The document's redirect URIs must be on the same host or on the user's own machine. Claude Code and the
Claude apps both use claude.ai's documents.

Other local programs register themselves and may only use loopback redirect URIs (`127.0.0.1`, `[::1]`,
`localhost`, any port), so the code can only go back to the user's own machine. The registration isn't stored:
the client ID speki hands out is the sealed registration itself.

Without this split, someone could register a client called "Claude Code" with their own redirect URI, mail a
user the sign-in link, and receive a read-write code when the user clicks continue.

Fetching a metadata document only goes to allow-listed hosts on port 443, refuses private and internal
addresses after DNS resolution, follows no redirects, and reads at most 64 KiB in 5 seconds. Documents are
cached for 10 minutes and refreshed in the background.

### The details that make it safe

- Redirect URIs must match exactly (loopback: any port). A bad one gets an error page, never a redirect, so
  speki can't be used as an open redirector.
- PKCE with S256 is required from clients, and speki uses PKCE with GitHub too.
- The state sent to GitHub is created after consent and bound to a `__Host-` cookie. Otherwise someone could
  make a user finish a sign-in as the attacker's GitHub account, and the user's agent would write into the
  attacker's vaults.
- The consent form needs a sealed value matching a cookie, which stops cross-site form posts.
- Authorization codes live 60 seconds and can be used once.
- Speki refuses GitHub tokens that don't expire. The GitHub App must have "Expire user authorization tokens"
  on.

## Who gets in

A request to `/mcp` is accepted when all of these hold:

1. The token was issued by this speki for its `public_url`, and hasn't expired.
2. If the operator configured organizations, the user is an active member of at least one of them. Pending
   invitations don't count.
3. The user can read at least one vault.

If 2 or 3 fails, the client gets a 403 saying which. It never names organizations or vaults the user can't
see.

When every vault the user can read is still being cloned or indexed, the request waits up to 30 seconds for
one to be ready, then gets a 503 with `Retry-After`. When their only vaults failed to open, they get a 503
straight away. A user who can read a served vault never waits; their other vaults show up with their state
([tools/overview.md](tools/overview.md#vaults-speki-cant-serve)).

## What a user can see and change

Access follows GitHub, per vault:

| On GitHub, the user can | In speki, they can |
|---|---|
| read the repository with at least the vault's `read_role`, and is in its `readers` | `list`, `search`, `query`, `read` and `history` in that vault |
| also push to the repository, and is in the vault's `writers` | also `write`, `edit`, `move` and `delete`, if they signed in with read and write |
| nothing | nothing. The vault doesn't exist for them |

A vault's `speki.yaml` can narrow GitHub's permissions but never widen them
([vaults.md](vaults.md#limiting-who-reads-and-writes-a-vault)). A vault the user can't read behaves like one
that doesn't exist; errors don't tell the two apart.

Speki works this out with the user's own token: the app installations the user can access, the repositories
in each with the user's permissions, and organization and team memberships. That works because a GitHub App's
user token can only do what the user can do, limited to what the app may do, limited to where the app is
installed.

Being able to push isn't always enough. When the default branch requires pull requests or checks, GitHub
rejects the commit and the agent gets `vault_read_only`.

## Read-only sign-in

A read-only sign-in can't change anything, whatever the user's GitHub permissions. Write tools answer
`read_only_session`, telling the agent to ask the user to reconnect with write access. A user can connect an
agent read-only to keep it from writing, for example when it also reads untrusted content
([security.md](security.md#prompt-injection-stored-in-documents)).

## Caching and how fast access changes

What GitHub says a user may see is cached for 5 minutes, keyed by a hash of the GitHub token, not by the
user. With Redis, every instance shares the cache. Someone removed from an organization, repository or team,
or who revokes the app, keeps access for up to 5 minutes.

The token's scope is applied after the cache, so a read-only sign-in never inherits a cached read-write
answer. Failures aren't cached, so someone turned away is checked again on the next request. The rate
limit runs before this check so that can't be used to make speki call GitHub without limit.

Writes are checked by GitHub when the commit arrives, whatever the cache says.

## Tokens

Speki's tokens aren't JWTs, and nobody but speki needs to read them. Each is a value sealed with AES-256-GCM:
`v1.<key id>.<nonce and ciphertext>`. Every kind of value has its own key, derived from the configured key,
so a refresh token can't be presented as an access token, and the format has no algorithm field to tamper
with.

| Token | Holds | Lives |
|---|---|---|
| access | GitHub user ID and login, GitHub access token, scopes, client name, audience | 1 hour, or until the GitHub token expires if sooner |
| refresh | GitHub user ID and login, GitHub refresh token, scopes, client, audience, sign-in time | until 90 days after sign-in, or until the GitHub refresh token expires if sooner |

Checking an access token needs no GitHub call: speki opens it and checks its audience and expiry. GitHub is
asked when the user's access is resolved, at most every 5 minutes. A GitHub token GitHub no longer accepts
makes speki answer 401, the client refreshes, the refresh fails, and the user signs in again.

### Refreshing

Every speki refresh is a GitHub refresh. GitHub's refresh tokens are single use: refreshing returns a new
pair and kills the old one. That gives speki rotation and replay detection for free. Once the real client has
refreshed, a stolen copy of its old refresh token is dead, because the GitHub token inside is dead.

The sign-in time inside a refresh token doesn't change on refresh, so every chain of refresh tokens ends 90
days after the user signed in. A stolen refresh token can't be kept alive forever. Scopes on refresh can only
shrink.

A client sometimes refreshes the same token twice at once, or retries after a lost answer. GitHub accepts the
first and refuses the second, which would sign the user out. So speki takes a lock per token, refreshes once,
and hands the result to the second request. The result is kept for a minute, encrypted with a key derived from
the old refresh token itself, so the store never holds a usable token. With several instances, the lock and
results live in Redis; that's the main reason several instances need it.

When GitHub doesn't answer a refresh in time, it may have spent the token anyway, and has no grace period for
the old one. Speki can't prevent the sign-out that follows, only make it visible: it's logged and counted as
`lost_to_timeout`.

### Revocation

Speki keeps no list of its tokens, so it can't revoke one before it expires. What works:

- A user revokes the speki app in their GitHub settings. The GitHub token inside every speki token they hold
  dies, and speki notices within 5 minutes.
- An operator removes a key from the key file. Every token sealed with it stops working, and so do the client
  IDs of programs that registered themselves.
- When a key may have leaked, the operator also revokes all user tokens in the GitHub App's settings: anyone
  with the key could open captured tokens and use the GitHub token inside directly.

Key rotation is in [running.md](running.md#keys).

## Using more than one speki

People often use more than one speki, say one their employer runs and a personal one. Each instance signs
users in on its own, and tokens from one don't work on the other. Operators should set the `instance` block
([running.md](running.md#configuration)), so agents can tell the instances apart: it names the server
`speki-<name>` and opens the agent's instructions with what belongs there.

Name each server in the client after its advertised name. Claude Code builds tool names from that key, so
permission rules stay readable:

```sh
claude mcp add --transport http speki-work https://speki.example.com/mcp
claude mcp add --transport http speki-home https://speki.home.example/mcp
```

A rule like `"deny": ["mcp__speki-home"]` in work repositories then keeps the personal instance out of them.

## What speki keeps

Nothing about users. Speki logs who did what (GitHub login, client, tool, vault, path), but never document
content, queries or tokens.
