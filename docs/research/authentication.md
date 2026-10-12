# Authentication and access

Researched 2026-10-09. The goal: speki keeps no user database and lets GitHub decide who someone is and what
they may see, while MCP clients like Claude still get a normal browser sign-in.

## GitHub App user tokens

Speki is a GitHub App. When a user signs in through the app's web flow, speki receives a user access
token (`ghu_...`) and a refresh token (`ghr_...`).

- The access token lives 8 hours, the refresh token 6 months.
- Refresh tokens are single use. Refreshing returns a new pair and kills the old refresh token and the
  old access token.
- The app setting "Expire user authorization tokens" controls this. If it is off, GitHub returns tokens
  that never expire. Speki refuses to work with those.
- PKCE with S256 is supported since July 2025.
- The code exchange needs the app's client secret. That is why speki has to run the exchange itself.
- Scopes don't exist for GitHub Apps. A user token can do what the user can do, limited to what the app
  is allowed to do, limited to where the app is installed.

That last point does most of our authorization work. The app only gets `Contents: read & write`,
`Metadata: read` and organization `Members: read`. Without the `Workflows` permission GitHub refuses
writes to `.github/workflows/`, even if speki has a bug.

Sources: [generating user tokens](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app),
[refreshing](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/refreshing-user-access-tokens),
[PKCE changelog](https://github.blog/changelog/2025-07-14-pkce-support-for-oauth-and-github-app-authentication/).

## Finding out what a user may do

Two calls with the user's own token answer almost everything:

1. `GET /user/installations` lists installations of our app the user can access.
2. `GET /user/installations/{id}/repositories` lists the repositories in that installation the user can
   access, each with a `permissions` object (`pull`, `push`, `admin`, ...).

Intersect that with the repos speki knows are vaults (installed and containing `speki.yaml`) and you have
the user's vaults: `pull` means read, `push` means write.

For the org gate, `GET /user/memberships/orgs/{org}` with the user token returns the membership if it is
active or pending. Only `state == "active"` counts. This needs the app's `Members: read` permission and
the app installed in that org. An installed app with zero selected repos is enough for that.

Pushing to a repo doesn't guarantee pushing to its default branch. Rulesets can still block it, and a
blocked API write comes back as a 409 or 422. Speki reports those as "this vault doesn't accept direct
writes".

Rate limits: a user token gets 5,000 requests per hour, shared with everything else that user runs. A
304 on a conditional request doesn't count. We cache these lookups for a short time (see below) and
re-validate with ETags.

Sources: [installations](https://docs.github.com/en/rest/apps/installations),
[org members](https://docs.github.com/en/rest/orgs/members),
[rate limits](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api).

## Speki as authorization server

As [mcp-protocol.md](mcp-protocol.md) explains, MCP clients can't talk to GitHub's OAuth directly. Speki
runs a small authorization server in front of it. This is the pattern the spec calls an MCP proxy server.
[FastMCP's OAuth proxy](https://gofastmcp.com/servers/auth/oauth-proxy) does the same thing in Python.

The sign-in, as the user sees it:

1. Claude calls speki, gets a 401, finds speki's metadata and opens a browser.
2. Speki shows a short consent page: which app is asking, where the token goes, and a choice between
   read-only and read-write.
3. The user continues to GitHub. After the first time GitHub skips its own screen.
4. GitHub redirects to speki, speki redirects to Claude, and Claude exchanges the code for tokens.

The tokens Claude receives are speki's own. Inside, sealed with a key only speki has, sits the GitHub
token. Speki checks the audience on every request and then uses the GitHub token inside for calls to
GitHub. The client never holds a GitHub token, so this is not token passthrough.

The speki refresh token wraps the GitHub refresh token. Refreshing with speki refreshes with GitHub, and the
client stores the new pair. GitHub rotating refresh tokens gives us rotation for free, and speki
stores nothing.

### Client registration without storage

- CIMD: the client's ID is a URL to a JSON document describing it. Speki fetches it (with SSRF
  protection) and needs no storage. Claude Code and the Claude apps use this when speki advertises it.
  Speki only accepts documents from hosts on a configured list (`claude.ai` by default). The host is what
  the consent page shows, so a client can't pose as another.
- DCR: other clients still register. Speki answers with a `client_id` that is itself the sealed
  registration (name and redirect URIs). Nothing is written anywhere. Registered clients may only use
  loopback redirects, because their name is whatever they say it is.

### What statelessness costs

Tradeoffs:

- OAuth 2.1 wants authorization codes to be single use. Speki's codes are sealed blobs with a 60 second
  lifetime, bound to the PKCE challenge, so a stolen code is useless without the verifier. A single
  instance keeps used code IDs in memory and rejects repeats. A cluster needs a shared store for this.
- Two refreshes of the same token at once: GitHub accepts one and the other fails. Speki serialises
  refreshes per token and hands the second caller the result of the first, which it keeps for 30 seconds.
  In memory for one instance, in the shared store for a cluster.
- The spec asks a proxy to get consent for every new client before forwarding to the upstream
  authorization server. Speki shows its consent page on every sign-in instead of remembering approvals.
  That is one extra click.

Decision: one instance runs with in-memory state only. Running more than one requires Redis or Valkey,
which then also carries the "vault X moved to commit Y" messages between instances.

## Org and vault gate

A request passes only when:

1. the token is speki's, unexpired, for this audience;
2. if orgs are configured, the user is an active member of at least one;
3. the user can read at least one vault.

Failing 2 or 3 is a 403 with a message that names the reason, but not other vaults or orgs.

Membership and vault access are cached per GitHub token for 5 minutes, shared by every instance. Removing someone
from GitHub stops their speki access within that window. Writes go to GitHub with the user's own token, so
GitHub re-checks push access at that moment regardless of our cache.

## Who may install the app

The GitHub App is part of each deployment. The organisation that registers it decides whether other
accounts may install it. We recommend keeping it private. Speki also has a config list of accounts
(by numeric ID) whose installations count, defaulting to the app owner. That way a public app still can't be used by a
stranger to push content into speki's indexer.
