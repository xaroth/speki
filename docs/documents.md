# Documents

A document is one markdown file in a vault, with a YAML frontmatter block at the top. People clone vaults,
open them in Obsidian and browse them on github.com, so a document has to look normal to all three. Speki
reads documents leniently and writes them strictly: whatever a person wrote is indexed, and a change made
through speki may not make a document worse.

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

## Identity

A document's path in the vault is its identity: `ops/fleet-doctrine.md` in `acme/kb` is the same document
until it's moved. That matches how git and Obsidian see files.

Its version is the git blob SHA of its content. Any change to the bytes changes the version, and the same
bytes always have the same version, in speki and in git ([changes.md](changes.md#versions-and-conflicts)).

## Frontmatter

The frontmatter block starts when the file's first line is exactly `---` (after an optional byte order mark)
and ends at the next line that is exactly `---` or `...`. Anything else means there is no frontmatter, and
the whole file is the body.

Speki owns a small set of flat, lowercase fields. Obsidian shows them as properties, and GitHub renders them
as a table above the document.

| Field | Type | Set by | Meaning |
|---|---|---|---|
| `title` | text | writer | Display title. If missing, speki uses the first `#` heading, then the document's path |
| `description` | text | writer | One or two sentences about what the document covers. Shown in search results and listings |
| `tags` | list of text | writer | Topics. A leading `#` is dropped and tags are lowercased. At most 20 |
| `aliases` | list of text | writer | Other names for the subject. Searched like the title. At most 10 |
| `status` | `draft`, `active`, `deprecated` | writer | Missing means `active`. Search ranks `deprecated` documents lower |
| `related` | list of paths | writer | Other documents in the same vault worth reading alongside this one |
| `created` | UTC datetime | speki | Set when speki creates the document |
| `updated` | UTC datetime | speki | Set every time speki changes the document |

`created` and `updated` duplicate what git knows. They're kept because they make queries cheap
(`WHERE updated < ...`) and Obsidian shows them. Git history stays the record of who changed what. Dates are
read as RFC 3339, or Obsidian's `2026-10-09`, `2026-10-09T10:30:00` and `2026-10-09 10:30:00`, taken as UTC.

A `related` entry is a path relative to the document, a path from the vault root starting with `/`, or a
wikilink (`[[fleet-roles]]`), which is how Obsidian stores links in properties.

Every other key is kept as written. `query` shows such keys as JSON in the `extra` column. Agents may only
change them when the vault lists them under `frontmatter.allow` ([vaults.md](vaults.md#spekiyaml)).

YAML anchors and aliases (`&x`, `*x`) aren't allowed, which rules out "billion laughs" input that expands
into gigabytes.

## Body

Plain markdown: CommonMark plus GitHub tables, strikethrough and bare links. Speki doesn't require any
structure, but headings matter: they get extra weight in search, and `read` returns the outline of a long
document so an agent can read the part it needs.

Speki never renders markdown to HTML. It parses the body only to find headings and links.

## Links

Write links to other documents as relative markdown links:

```markdown
See [fleet roles](../ops/fleet-roles.md) for who does what.
```

They render on GitHub, and Obsidian follows them when it's set to relative path links (Settings, Files and
links: "Use [[Wikilinks]]" off, "New link format" relative path).

Speki also understands wikilinks written by people (`[[fleet-roles]]`, `[[fleet-roles#Logistics|logi]]`,
`![[diagram]]`), resolved by file name like Obsidian does. Markdown links, wikilinks and `related` entries
all end up in the `links` view of `query`. Links to other vaults or to the web are recorded but not
resolved.

Links are resolved when they're read, not when they're indexed. So adding a document fixes the links that
already pointed at it, without touching the documents that contain them. When a wikilink matches several
files, a document that parses wins over a broken one, and among those the shortest path wins.

## Paths

A path speki will write:

- ends in `.md`
- is relative, with `/` as separator, and has no `.` or `..` segments
- has no segment starting with `.`
- has no characters from `[]#^|:?*<>"\`, no control characters, and no invisible ones (zero-width spaces,
  direction marks)
- has no part named like a Windows device (`con`, `nul`, `aux`, `prn`, `com1`, `lpt1` and so on) and no
  part ending in a space or a dot
- has segments of at most 255 bytes and is at most 10 folders deep
- doesn't differ only in letter case from an existing path

These rules exist for the people who open the vault elsewhere. The banned characters break Obsidian or
Windows. Git is case-sensitive but macOS and Windows checkouts aren't, so `Note.md` next to `note.md` breaks
them.

New paths are stored in Unicode NFC. macOS often stores folder names decomposed (NFD). When the vault already
has a folder with the same name in another normalization, speki uses the vault's spelling, so the document
lands in that folder rather than in a look-alike next to it. A file can't be created next to one with the
same name in another normalization. Existing documents keep the exact bytes git has for their path.

Lowercase names with `-` between words are the convention. Spaces force URL encoding in markdown links.
Speki doesn't enforce the convention.

## Size

A document can be up to 1 MiB, its frontmatter up to 64 KiB. Files over 1 MiB aren't indexed at all. A file
whose frontmatter is over 64 KiB is indexed as broken (below).

Titles over 200 bytes and descriptions over 500 are kept, with a warning, but answers show only their start.
That way one careless document can't fill an agent's search results.

## Documents changed outside speki

People edit vaults in Obsidian, on github.com and in their editor, and speki can't stop them from writing
frontmatter it wouldn't accept. So reading is lenient:

- A field with the wrong type is ignored on its own: a `status` of `wip`, a `title` that's a list.
- `tags` written as one string (`tags: a, b`), which Obsidian allows, is split on commas, with a warning.
- Frontmatter that isn't valid YAML, isn't a mapping, repeats a key or uses anchors is ignored as a whole.
  The title then comes from the first heading or the path, and the body is still searchable.
- More than 20 tags or 10 aliases are kept, with a warning.

Each problem becomes a warning that `read` returns. The document is still indexed and searchable.

Only two problems make a file unusable: it isn't UTF-8, or its frontmatter block is over 64 KiB. Such a
file is indexed as broken. It's listed with status `broken`, isn't searchable, and can't be read or changed
through speki, only deleted ([tools/delete.md](tools/delete.md)).

## What a change may do

A change through `write` or `edit` is checked against the document as it was:

- The result must parse, and its frontmatter must be valid YAML, because speki rewrites the frontmatter to
  set `updated`.
- It may not add problems. Problems the document already had are left alone: an agent fixing a typo in a
  document whose tags a person wrote as `tags: a, b` isn't blocked by that. A new document may have no
  problems at all.
- More than 20 tags or 10 aliases can't grow.
- A new document needs a title, in the frontmatter or as a `#` heading.
- Each `related` entry the change adds must point to a document in the vault. Entries already there are left
  alone, so deleting a document doesn't block changes to the documents that list it.
- Agents may only set `title`, `description`, `tags`, `aliases`, `status`, `related` and the vault's
  `frontmatter.allow` keys. Any other key must keep its value: a change can't add, change or remove it. Values
  are compared as YAML, so quoting style and comments don't count. People can still set such keys outside
  speki.

When a document's frontmatter is broken as a whole, the change has to fix it. An agent can only do that when
the broken block holds no keys agents may not set; otherwise a person has to.

Errors name the field and the problem, so the agent can fix its change.

## How speki rewrites a file

After the checks, speki sets `updated` (and `created` for a new document) and commits. It changes as little
as it can:

- Only the frontmatter block is rewritten. The body is kept byte for byte.
- The frontmatter is edited as a YAML tree, not as text. Key order and most comments survive; quoting style or
  indentation of other keys may change. Editing YAML as text breaks on the first unusual document.
- `updated` is always set. `created` is set on a new document, and otherwise kept as it was: an existing
  document without one doesn't get one. Existing values are replaced in place; a missing key is added at the
  end.
- Line endings are kept. Flipping CRLF to LF would show the whole file as changed.
- A file without frontmatter gets a block with just these dates.

The YAML encoder sometimes places a comment where it reads back differently, which would make every change
move it again. Speki re-encodes its own output until it stops changing, and refuses the change with a
message to move or remove the comment if it doesn't settle.

`move` copies the file unchanged and doesn't set `updated`: the content didn't change, and neither does its
version.
