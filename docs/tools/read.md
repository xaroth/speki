# read

Reads a document's raw markdown, frontmatter included, with its version. It's always loaded, because
almost every task that starts with a search continues with a read.

The content is the raw file, not a rendering of it. It's in the text result, in a `document` fence, so an
agent can copy text out of it into [`edit`](edit.md) exactly, whitespace and line endings included. A `<`
that starts something that looks like a fence tag is shown as `&lt;` ([overview.md](overview.md#results));
such text has to be copied as it is in the file.

## Input

| Input | Type | Default |
|---|---|---|
| `vault` | vault name | required |
| `path` | document path | required |
| `from_line` | first line, counting from 1 at the top of the file | 1 |
| `line_count` | number of lines | as many as fit in 1,000 lines and one answer (80 KiB) |

A `line_count` over 1,000 is treated as 1,000.

## Output

```text
acme/kb services/edge-proxy.md
version 3f2a91c4e0b8d2a7c6f1e9b3d5a8c0f2e4b6d8a1, lines 1-212 of 212
<document boundary="9f3c1a7e5b2d8c40">
---
title: Edge proxy
...
</document boundary="9f3c1a7e5b2d8c40">
```

The first line is the vault and path, the second the version and which lines this is. Warnings follow as
`warning: ...` lines, then the outline in an `outline` fence when there is one, then the content. An
empty document reads "lines 1-0 of 0". The frontmatter is part of the content, as written in the file.
Line numbers count from the top of the file, frontmatter included, so they match what an editor shows.

## Long documents

One call returns at most 1,000 lines, and as many as fit in one answer of 80 KiB, JSON-encoded, beside
the vault, path, warnings and outline. The budget also sets aside the size of the document's parsed
frontmatter fields, JSON-encoded, called the metadata below. The tool description puts it at about 75 KiB
of content. Each `<` or look-alike counts as the `&lt;` a fence may turn it into, so markup-heavy text
gets fewer lines. Content always gets at least 4 KiB, so a document whose metadata fills the budget still
pages to the end; the answer may then go over 80 KiB.

When the call doesn't start at line 1 or doesn't return the whole document, the text has an `outline` fence
before the content, listing its headings as `line N: ## text`, so the agent can read just the part it needs
with `from_line` and `line_count`. The outline shows at most 200 headings, each cut to 200 bytes, about 16
KiB in all, and counts against the answer. When it's cut, a warning says "the outline shows the first N of M
headings only". When the metadata leaves less than 4 KiB, the outline is left out with the warning "the
outline is left out: the metadata leaves too little room".

A single line longer than fits is cut, with the warning "line N is longer than fits in one read; only its
start is shown".

## Warnings

The `warning:` lines list problems with the document's frontmatter, for documents changed outside speki:
a field of the wrong type, frontmatter that isn't valid YAML, too many tags. They're the same problems a
change must not add ([documents.md](../documents.md#documents-changed-outside-speki)). They also carry
this read's own notes: a cut outline, a left-out outline, or a cut line.

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
