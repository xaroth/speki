# Documents in code

Package: `document`. It turns bytes into a `Document` and back, and decides what's a valid path. It knows
nothing about vaults, git or the index, so it's easy to test, and it gets the most tests.

## Types

```go
type Status string // "draft", "active", "deprecated"

type Meta struct {
	Title       string
	Description string
	Tags        []string
	Aliases     []string
	Status      Status
	Related     []string
	Created     time.Time // zero when missing
	Updated     time.Time
	Extra       map[string]any // keys speki doesn't own, as decoded YAML; NaN and infinity become text
}

type Document struct {
	Meta     Meta
	Body     string    // after the frontmatter
	Headings []Heading // level, text, line (1-based, whole file)
	Links    []Link    // target as written, candidate, kind, text
	Warnings []string  // problems with frontmatter fields that were ignored

	raw     []byte
	front   *yaml.Node // nil when there is no frontmatter
	bodyOff int        // byte offset of Body in raw
}

func Parse(path string, raw []byte) (*Document, error)
```

`Parse` errors only when the file can't be used at all: it isn't UTF-8, or its frontmatter block is over
64 KiB. The index keeps such a file as broken (see [index.md](index.md)). Everything else becomes a
warning. Frontmatter that doesn't parse as YAML, uses anchors or
aliases, or isn't a mapping is ignored as a whole: the document gets its title from the first heading or
the file name, and `Warnings` says what's wrong. A core field with the wrong type is ignored on its own.

## Frontmatter

The block starts when the file's first line is exactly `---` and ends at the next line that is `---` or
`...`. A UTF-8 byte order mark before the `---` is allowed, and `Touch` keeps it. Line endings may be `\n`
or `\r\n`. Anything else means there's no frontmatter, and the whole file is the body.

The block is decoded into a `yaml.Node` with `go.yaml.in/yaml/v3`, which doesn't expand aliases. Speki
walks the node tree and treats the frontmatter as broken if it contains an anchor or alias. The top level
must be a mapping whose keys are plain scalars, each appearing once, and the block must hold exactly one
YAML document. That last rule catches a closing line with a trailing space: the block then runs on to the
next `---` in the body, and the text in between would otherwise disappear without a warning.

Core fields are read from the node tree with type checks. `tags` accepts a list of strings. A single string
is accepted when read (Obsidian allows it) but rejected on write. Tags lose a leading `#` and are
lowercased. Dates accept RFC 3339 and Obsidian's `2026-10-09`, `2026-10-09T10:30:00` and
`2026-10-09 10:30:00` (taken as UTC).

## Changing a document

```go
// Check compares a document an agent wants to write with the current one (nil for a new document) and
// returns the problems the change introduces.
func Check(current, next *Document) error

// Touch sets updated to now, and created to created unless that's zero, and returns the new bytes.
func (d *Document) Touch(now, created time.Time) ([]byte, error)
```

`Check` rejects problems the change brings in, and leaves old ones alone. An agent fixing a typo in a
document whose `tags` a person wrote as `tags: a, b` shouldn't be blocked by that. A new document must have
no problems at all and must have a title (frontmatter or `#` heading). Changing a document with broken
frontmatter only works if the change fixes the frontmatter, because speki has to rewrite it to set
`updated`. The error says so and quotes the problem.

`Touch` edits the `yaml.Node` tree in place: it updates the value node for `updated` (and `created`), or
appends key and value nodes at the end of the mapping. Then it re-encodes only the frontmatter block, using
2-space indentation, and splices it back before the untouched body bytes. The line ending style of the
original is kept: the block is decoded with `\n` endings and `\r\n` is put back on output. If the file had
no frontmatter, `Touch` adds a block with just those fields.

The encoder sometimes places a comment or a blank line where the decoder reads it back differently, which
would make every `Touch` change the block again. So `Touch` decodes and re-encodes its own output until
it stops changing (a few rounds at most) and refuses the change if it doesn't settle. It also refuses when
the dates push the document over 1 MiB or the block over 64 KiB, since `Check` measured the document
before they were added.

Re-encoding a node tree keeps key order and most comments. It may change quoting style or indentation of
unknown keys. That's accepted: the alternative (editing YAML as text) breaks on the first unusual
document.

`edit` works on raw bytes: `knowledge` replaces `old_text` in the raw file, then parses, checks and touches
the result. So agents can edit frontmatter with `edit` too, and speki still checks the outcome.

## Markdown

goldmark parses the body with the GFM table, strikethrough and linkify extensions and a small wikilink
parser of our own (goldmark has none built in that we want to depend on). Speki walks the AST once to
collect:

- headings, with their line numbers counted from the top of the file (body offset plus the line within the
  body)
- links: markdown links and wikilinks, with their text. Linkify makes bare URLs, `www.example.com` and
  email addresses links too, so they're recorded like any other link to the web.

Speki never renders HTML, so goldmark's renderer and its past XSS problems don't matter here. The version
is kept current anyway.

Link candidates:

- Markdown link `../ops/fleet-roles.md#crew`: drop the fragment, URL-decode, join with the document's folder,
  `path.Clean`. If the result escapes the vault root, or the target has a scheme (`https:`), the candidate is
  empty.
- Wikilink `[[fleet-roles#Crew|crew]]`: the candidate is the name part, lowercased, without `.md`.
- `related` entries: resolved like markdown links, with kind `related`. An entry written as a wikilink
  (`[[fleet-roles]]`, which is how Obsidian stores links in properties) is treated as one and gets kind
  `wikilink`.

## Paths

```go
// CheckPath reports whether p is a path speki may write, and returns the path to use. existing is used
// to refuse case-only collisions and to reuse git's spelling of a name.
func CheckPath(p string, existing func(folded string) (string, bool)) (string, error)
```

The rules are in [documents.md](../functional/documents.md). `CheckPath` is for new paths. It normalises
to NFC and returns an error naming the rule that failed, which goes straight to the agent. A folder name
that git has in another normalisation is returned with git's bytes, so `café/new.md` lands in an existing
NFD `café/`. A file that exists in another normalisation, or a name that differs in letter case, is
refused. Existing documents keep the exact path git has, even when it isn't NFC.
