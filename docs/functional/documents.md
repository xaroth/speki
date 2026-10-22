# Documents

A document is a markdown file with a frontmatter block. [The storage research](../research/storage-format.md)
explains the choices made here.

```markdown
---
title: Fleet doctrine
description: Which ship fittings we fly in which fleet type, and why.
tags: [fleet, doctrine]
aliases: [doctrines]
status: active
related:
  - ../ops/fleet-roles.md
created: 2026-10-09T10:30:00Z
updated: 2026-10-09T14:02:11Z
---

# Fleet doctrine

Our standard fleets fly ...
```

## Paths

The path within the vault identifies a document: `ops/fleet-doctrine.md`.

A path speki will write:

- ends in `.md`
- is relative, with `/` as separator, and has no `.` or `..` segments
- has no segment starting with `.`
- has no characters from `[]#^|:?*<>"\`, no control characters, and no invisible ones (zero-width
  spaces, direction marks)
- has no part named like a Windows device (`con`, `nul`, `aux`, `prn`, `com1`, `lpt1` and so on) and
  no part ending in a space or a dot, so Obsidian users on Windows can open it
- has segments of at most 255 bytes and at most 10 folders deep
- doesn't differ only in letter case from an existing path

New paths are stored in Unicode NFC, and a new file can't have the same name as an existing one in another
normalisation. Where the vault already has a folder with the same name in another normalisation (folders
made on macOS are often stored decomposed), speki uses the vault's spelling, so the document lands in that
folder rather than in a look-alike next to it.

Lowercase names with `-` between words are the convention, but speki doesn't enforce it.

## Frontmatter fields

| Field | Type | Who sets it | Meaning |
|---|---|---|---|
| `title` | text | writer | Display title. If missing, speki uses the first `#` heading, then the path in the vault |
| `description` | text | writer | One or two sentences about what the document covers. Shown in search results and listings |
| `tags` | list of text | writer | Topics. No leading `#`. Matched case-insensitively. At most 20 |
| `aliases` | list of text | writer | Other names for the subject. Searched like the title. At most 10 |
| `status` | `draft`, `active`, `deprecated` | writer | Missing means `active`. Search ranks `deprecated` documents lower |
| `related` | list of paths | writer | Other documents in the same vault worth reading alongside this one. Each entry a change adds must point to an existing document |
| `created` | UTC datetime | speki | Set when speki creates the document. A change that leaves it out keeps it |
| `updated` | UTC datetime | speki | Set every time speki changes the document |

Values that don't fit the table are rejected on write: a `status` of `wip`, `tags` given as a single string
with commas. The error says which field and why.

A `title` longer than 200 bytes or a `description` longer than 500 is kept, with a warning; answers show
only its start. More than 20 tags or 10 aliases are kept too, with a warning, so a change can't add them.

A `related` entry is a path relative to the document, a path from the vault root starting with `/`, or a
wikilink (`[[fleet-roles]]`), resolved like the [links](#links) in the body. A change can't add an entry
that doesn't point to a document in the vault. Entries already there are left alone, so deleting a
document doesn't block changes to the documents that list it.

Agents may only set the writer fields above, plus the keys the vault lists under `frontmatter.allow` in
its [speki.yaml](vaults.md#spekiyaml). Any other key must keep the value it has: a change can't add,
change or remove it. People can still set such keys outside speki. `query` exposes them as JSON in the
`extra` column. Speki never removes or reorders keys it doesn't own.

Documents changed outside speki may break these rules. Speki still indexes them: a field with the wrong
type is ignored, and frontmatter that isn't valid YAML is ignored as a whole (the body is still searchable).
`read` lists the problems under `warnings`. An agent can change such a document as long as it doesn't add
new problems. If the frontmatter as a whole is broken, the change has to fix it, because speki rewrites
the frontmatter to set `updated`. An agent can only make that fix when the broken block holds no keys
agents may not set; otherwise a person has to.

## Body

Plain markdown (CommonMark plus GitHub tables). Speki doesn't require any structure, but headings matter:
they get extra weight in search, and `read` can return the outline of a long document.

## Links

Write links to other documents as relative markdown links:

```markdown
See [fleet roles](../ops/fleet-roles.md) for who does what.
```

They render on GitHub, and Obsidian follows them when it's set to relative path links (Settings → Files
and links: "Use [[Wikilinks]]" off, "New link format" relative path).

Speki also understands wikilinks (`[[fleet-roles]]`, `[[fleet-roles#Logistics|logi]]`) written by people.
Both kinds, plus the `related` field, end up in the `links` table for `query`. A `related` entry written
as a wikilink (`[[fleet-roles]]`) counts as a wikilink there. Links to other vaults or to the web are
recorded but not resolved.

## Size

A document can be up to 1 MiB, its frontmatter up to 64 KiB. YAML anchors and aliases (`&x`, `*x`)
aren't allowed in frontmatter.
