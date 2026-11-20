# OKF vaults

A vault whose `speki.yaml` sets `format: okf` holds documents in the
[Open Knowledge Format](https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md)
(OKF), v0.2 or v0.1. Speki reads it like any vault. Its changes don't make a document less conformant: a
new concept needs a `type`, `index.md` files don't gain frontmatter, and speki dates documents where OKF
does. What a document already breaks is left alone.

```yaml
title: Data catalog
description: What our tables and metrics mean, in OKF.
format: okf
```

Everything in [documents.md](documents.md) applies, except where this page says otherwise.

## Reading

Reading doesn't depend on the format, so the index is the same for every vault:

- `status: stable` reads as `active`. `draft` and `deprecated` mean the same in both.
- A document's last change is the later of `updated` and its OKF date: `generated.at`, or `timestamp` when
  that's missing or not a date. Listings, search results and `query` show it.
- `type`, `resource`, `sources`, `generated`, `verified` and the other OKF keys are kept as written, and
  `query` shows them in the `extra` column, so `json_extract(extra, '$.type')` filters by type.
- Links from the vault root, like `/tables/orders.md`, resolve in every vault.

## What a change may do

On top of the rules in [documents.md](documents.md#what-a-change-may-do):

- Agents may also set `type`, `resource`, `sources`, `usage_window`, `verified` and `stale_after`, and
  `generated` and `timestamp`, which speki overwrites (only `by` and `at` inside `generated`). Other keys stay
  locked unless the vault lists them under `frontmatter.allow`. OKF's Attested Computation keys (`runtime`,
  `parameters`, `computation`, `executor`, `attester`) need that.
- `status` accepts `active` as well as OKF's `stable`, and keeps what was written.
- A concept needs a non-empty text `type`. A document whose current version has none may keep going without
  one.
- `created` is locked like other keys, since speki doesn't set it in an OKF vault.
- `index.md` and `log.md` aren't concepts: they need no `type` or title. A nested `index.md` can't gain
  frontmatter, and the root `index.md` may only add `okf_version`. Frontmatter they already have is left
  alone. Neither can be moved, nor can a document be moved onto one, because a move copies the file
  without these checks.
- A change may only add a `human:` entry to `verified` for the signed-in user, as `human:<login>`, so an
  agent can't claim someone else reviewed a document. Entries already there, and entries by agents
  (`<producer>/<version>`) or processes (`process:<id>`), aren't checked.

## Dates

Speki doesn't add `created` or `updated` in an OKF vault. It records the last change where OKF does:

- A document with `generated` gets `generated.by` and `generated.at`. Other keys in `generated` are kept; a
  `generated` that isn't a mapping is replaced.
- A document with `timestamp` gets `timestamp`. One with both gets both.
- A new document gets `generated`, or `timestamp` when the root `index.md` declares `okf_version: "0.1"`. An
  existing document with neither gets `generated`, whatever the vault declares.
- A document that already has `updated` gets it too, so the date speki reads stays current.
- `index.md` and `log.md` are committed as written.

`generated.by` names the MCP client the user signed in with, in lower case with dashes, such as
`claude-code`; `speki` when the client gave no name. OKF asks for `<producer>/<version>`, but speki doesn't
know the client's version.

## Index files

Speki doesn't maintain `index.md`. `guide` asks agents to add a line when they create a document and to fix
it when they move or delete one, in OKF's `* [Title](./path.md) - description` form.

## Agents

`guide` marks an OKF vault with a `Format` line, lists the OKF keys among the keys agents may set, and adds a
section on OKF's rules ([tools/guide.md](tools/guide.md)).
