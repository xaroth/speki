# Signing in and access

## Signing in

Speki uses GitHub accounts. When an MCP client connects for the first time, it opens a browser:

1. Speki's sign-in page says who is asking for access and where the token will go. For web apps like
   claude.ai that's the site's address. For programs on the user's computer, like Claude Code, it says
   so, and warns that only the user's own tools should get access. The user picks one of:
   - read and write, the default
   - read only: the agent can search and read but never change anything
2. The user continues to GitHub. The first time, GitHub asks them to authorise the speki app. After that,
   GitHub sends them straight back.
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

Speki checks 2 and 3 again at most every 60 seconds per user. Someone removed from an organisation or a
repository loses access within a minute.

## What a user can see and change

Access follows GitHub, per vault:

| On GitHub, the user can | In speki, they can |
|---|---|
| read the repository | `list`, `search`, `query`, `read` and `history` in that vault |
| push to the repository | also `write`, `edit`, `move` and `delete`, if they signed in with read and write |
| nothing | nothing. The vault doesn't exist for them |

A vault the user can't read behaves like a vault that doesn't exist. Errors don't tell the two apart.

Being able to push isn't always enough. When the default branch requires pull requests, GitHub rejects
the write and speki says the vault doesn't accept direct writes.

## Read-only sign-in

A read-only sign-in can't change anything, regardless of the user's GitHub permissions. When the agent
tries, it gets a `read_only_session` error telling it to ask the user to reconnect with write access.

## What speki keeps

Nothing about users. The tokens the client holds contain everything speki needs, sealed so only speki can
open them. Speki logs who did what (GitHub login, tool, vault, path) but not document content, queries or
tokens.
