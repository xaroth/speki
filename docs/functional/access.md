# Signing in and access

## Signing in

Speki uses GitHub accounts. When an MCP client connects for the first time, it opens a browser:

1. Speki's sign-in page says who is asking for access and where the token will go. For web apps like
   claude.ai that's the site's address. For programs on the user's computer, like Claude Code, it says
   so, and warns that only the user's own tools should get access. The user picks one of:
   - read and write, the default
   - read only: the agent can search and read but never change anything
2. The user continues to GitHub, which asks which of their GitHub accounts to use. The first time, GitHub
   also asks them to authorise the speki app.
3. The browser returns to the client and the client is connected.

The page shows up on every sign-in, even for clients the user approved before. This is deliberate: it stops
another program from quietly getting a token by reusing GitHub's remembered approval.

Clients refresh their access on their own. A user signs in again 90 days after signing in, or sooner if
they revoke the speki app in their GitHub settings.

## Who gets in

A request is accepted when all of these hold:

1. The token was issued by this speki instance and hasn't expired.
2. If the operator configured organisations, the user is an active member of at least one of them.
   Pending invitations don't count.
3. The user can read at least one vault.

If 2 or 3 fails, the client gets an HTTP 403 saying which of the two it was. It never names organisations
or vaults the user can't see.

Vaults speki is still cloning and indexing, after a fresh start or a newly added vault, aren't served yet.
When those are all a user can read, the request waits up to 30 seconds for one to be ready, then gets an
HTTP 503 with `Retry-After` saying speki is still loading their vaults. The same 503 comes straight away
when their only vaults failed to open; speki retries those by itself. A user who can read a served vault
never waits.

Speki checks 2 and 3 again at most every 5 minutes per user. Someone removed from an organisation, a
repository or a team loses access within 5 minutes.

## What a user can see and change

Access follows GitHub, per vault:

| On GitHub, the user can | In speki, they can |
|---|---|
| read the repository with at least the vault's `read_role`, and is in its `readers` | `list`, `search`, `query`, `read` and `history` in that vault |
| also push to the repository, and is in the vault's `writers` | also `write`, `edit`, `move` and `delete`, if they signed in with read and write |
| nothing | nothing. The vault doesn't exist for them |

A vault's `speki.yaml` can narrow GitHub's permissions with `read_role`, `readers` and `writers`. Left out,
they don't narrow anything. See [vaults.md](vaults.md#limiting-who-reads-and-writes-a-vault).

A vault the user can't read behaves like a vault that doesn't exist. Errors don't tell the two apart.

Being able to push isn't always enough. When the default branch requires pull requests, GitHub rejects
the write and speki says the vault doesn't accept direct writes.

If GitHub turns someone away, or they can't read any vault, the page says which GitHub account they signed
in with, so a wrong account is obvious.

## Read-only sign-in

A read-only sign-in can't change anything, regardless of the user's GitHub permissions. When the agent
tries, it gets a `read_only_session` error telling it to ask the user to reconnect with write access.

## Using more than one speki

People often use more than one speki, say the one their employer runs for work documentation and a
personal one at home. Each instance signs users in on its own, and tokens from one don't work on the
other. Operators should set the `instance` block (see [running.md](running.md)), so agents can tell the
instances apart: it names the server `speki-<name>` and opens the agent's instructions with what belongs
there.

Name each server in the client after its advertised name. Claude Code builds tool names from that key,
so permission rules stay readable:

```sh
claude mcp add --transport http speki-work https://speki.example.com/mcp
claude mcp add --transport http speki-home https://speki.home.example/mcp
```

A rule like `"deny": ["mcp__speki-home"]` in work repositories then keeps the personal instance out of
them. To only read from an instance, pick "read only" when signing in.

## What speki keeps

Nothing about users. The tokens the client holds contain everything speki needs, sealed so only speki can
open them. Speki logs who did what (GitHub login, tool, vault, path) but not document content, queries or
tokens.
