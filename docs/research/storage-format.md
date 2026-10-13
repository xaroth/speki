# Storage format

Researched 2026-10-09. A vault is a git repository of markdown files. Speki is the main way in, but people
will clone vaults and open them in Obsidian or browse them on github.com, so the files have to look normal
to both.

## Documents

One document is one `.md` file with YAML frontmatter:

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

...
```

The path is the document's identity: `ops/fleet-doctrine.md` in vault `acme/kb` is the same
document until it's moved. That matches git and Obsidian, and keeps renames simple.

## Frontmatter

Obsidian calls frontmatter "properties". It supports text, list, number, checkbox, date, datetime and
tags, with one type per property name across a vault. Nested objects only work in source mode. Default
properties are `tags`, `aliases` and `cssclasses`. The singular `tag`, `alias` and `cssclass` forms
are deprecated ([Obsidian properties](https://obsidian.md/help/properties)).

GitHub renders frontmatter as a table above the document
([GitHub blog](https://github.blog/news-insights/product-news/viewing-yaml-metadata-in-your-documents/)).

Speki owns a small set of flat, lowercase fields:

| Field | Type | Notes |
|---|---|---|
| `title` | text | Falls back to the first `#` heading, then the filename |
| `description` | text | One or two sentences. Shown in search results |
| `tags` | list of text | No leading `#` |
| `aliases` | list of text | Other names people search for |
| `status` | `draft`, `active` or `deprecated` | Missing means `active`. Search ranks `deprecated` lower |
| `related` | list of relative paths | Explicit links to other documents |
| `created` | datetime, UTC | Set by speki when it creates the file |
| `updated` | datetime, UTC | Set by speki on every write |

Every other key is kept exactly as it is and exposed to queries as JSON. When Obsidian or a person adds
`cssclasses` or `author`, speki leaves it alone.

Speki only rewrites the frontmatter block when it has to change one of its own fields. When it does, it
edits the YAML node tree, so unknown keys, key order and most comments survive. If the frontmatter
doesn't parse, speki indexes the body, reports the error on `read`, and only accepts changes that fix the
frontmatter.

`created` and `updated` duplicate what git knows. We keep them anyway because they make queries
cheap (`WHERE updated > ...`) and Obsidian shows them. Git history stays the source of truth for who
changed what.

## Links

Speki writes relative markdown links: `[Fleet roles](../ops/fleet-roles.md)`. These render on GitHub and
work in Obsidian with "Use [[Wikilinks]]" turned off and the link format set to relative path. We document
that setting for Obsidian users.

When reading, speki also understands wikilinks (`[[fleet-roles]]`, `[[fleet-roles#Heading|text]]`) and
resolves them the way Obsidian does, by filename. Both kinds end up in the `links` table, so agents can
find what links to what.

Filenames: lowercase, words separated by `-`, no spaces. Spaces force URL encoding in markdown links and
make paths awkward for agents. Speki rejects characters that break Obsidian or Windows
(`[]#^|:?*<>"\`).

## Images and other files

Speki indexes `.md` files only and ignores everything else. Binary files would need upload limits,
content type checks, a way to return images to models, and decisions about git LFS, for content that is
mostly written by agents as text.

People can still put images in a vault by hand. Speki leaves them alone, and links to them show up in
`links` like any other link.

## What speki ignores

- Anything under a path segment starting with `.` (`.obsidian/`, `.github/`, `.trash/`).
- Symlinks and submodules. A symlink could point outside the vault, and speki never follows them.
- Files tracked by git LFS. A clone only has the pointer file.
- Files larger than 1 MiB.

## Filesystem gotchas

- Git paths are case-sensitive, but macOS and Windows checkouts are not. `Note.md` and `note.md` in one
  vault break Obsidian users. Speki refuses a write that would create a case-only collision.
- macOS produces NFD Unicode in filenames. Speki normalises paths to NFC.
- Speki keeps a file's existing line endings when rewriting it. API writes are raw bytes, and flipping CRLF
  to LF produces a diff of the whole file.

## Go libraries

| Purpose | Library | Status |
|---|---|---|
| Markdown parsing (headings, links) | `github.com/yuin/goldmark` | Active, v1.8.6. Use 1.7.17 or later for GO-2026-5320 |
| YAML | `go.yaml.in/yaml/v3` | Maintained by the YAML org. Same API as the archived `gopkg.in/yaml.v3` |

Speki splits frontmatter off itself (from the first `---` line to the next) and caps it at 64 KiB before
parsing. Anchors and aliases are rejected outright, which rules out "billion laughs" style input. The
goldmark frontmatter extensions depend on the archived YAML package, so we don't use them.
