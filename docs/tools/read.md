# read

Reads a document's raw markdown, frontmatter included, with its version. It's always loaded, because
almost every task that starts with a search continues with a read.

`content` is the raw file, not a rendering of it, so an agent can copy text out of it into
[`edit`](edit.md) exactly, whitespace and line endings included.

## Input

| Input | Type | Default |
|---|---|---|
| `vault` | vault name | required |
| `path` | document path | required |
| `from_line` | first line, counting from 1 at the top of the file | 1 |
| `line_count` | number of lines | as many as fit in 1,000 lines and 64 KiB |

A `line_count` over 1,000 is treated as 1,000.

## Output

```json
{ "vault": "acme/kb", "path": "services/esi-proxy.md",
  "version": "3f2a91c4e0b8d2a7c6f1e9b3d5a8c0f2e4b6d8a1",
  "metadata": { "title": "ESI proxy", "tags": ["esi", "proxy"], "status": "active",
                "updated": "2026-10-09T14:02:11Z" },
  "content": "---\ntitle: ESI proxy\n...",
  "from_line": 1, "line_count": 212, "total_lines": 212 }
```

`metadata` holds the parsed frontmatter fields (`title`, `description`, `tags`, `aliases`, `status`,
`related`, `created`, `updated`) and, under `extra`, keys speki doesn't own. Line numbers count from the top
of the file, frontmatter included, so they match what an editor shows.

## Long documents

One call returns at most 1,000 lines and 64 KiB. The limit keeps a result well under what clients accept,
even with the content in both the structured part and the text.

When the call doesn't return the whole document, `outline` lists its headings with their level, text and
line number, so the agent can read just the part it needs with `from_line` and `line_count`. The outline
shows at most 200 headings, each cut to 200 bytes, about 16 KiB in all, and counts against the 64 KiB. When
it's cut, a warning says so. When the whole document fits, there's no outline.

A single line longer than fits is cut, with a warning naming the line.

## Warnings

`warnings` lists problems with the document's frontmatter, for documents changed outside speki: a field of
the wrong type, frontmatter that isn't valid YAML, too many tags. They're the same problems a change must
not add ([documents.md](../documents.md#documents-changed-outside-speki)). The key is left out when there
are none.

## Text rendering

```text
acme/kb services/esi-proxy.md
version 3f2a91c4e0b8d2a7c6f1e9b3d5a8c0f2e4b6d8a1, lines 1-212 of 212
<document boundary="9f3c1a7e5b2d8c40">
---
title: ESI proxy
...
</document boundary="9f3c1a7e5b2d8c40">
```

Warnings come after the header, and the outline, when there is one, sits in an `outline` fence before the
content.

## How it reads

The index holds each document's metadata and version, but not its text twice: the content comes from the
vault's git copy, by version. So `read` always returns the bytes that version names, and the version it
returns is the one to pass to a change.

## Errors

| Code | When |
|---|---|
| `invalid` | Negative `from_line` or `line_count`; `from_line` past the end of the document |
| `not_found` | No such vault or document. Use `search` or `list` |
| `broken` | The file can't be parsed. Includes the reason and version; only `delete` works on it |
| `unavailable` | The vault is building, failed or gone |
