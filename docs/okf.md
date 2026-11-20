# OKF vaults

A vault whose `speki.yaml` sets `format: okf` holds documents in the
[Open Knowledge Format](https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md)
(OKF), v0.2 or v0.1. Speki reads it like any vault, and writes it so that it stays conformant: every concept
has a `type`, and `index.md` files keep no frontmatter.

```yaml
title: Data catalog
description: What our tables and metrics mean, in OKF.
format: okf
```

Everything in [documents.md](documents.md) applies, except where this page says otherwise.

## Reading

Reading doesn't depend on the format, so the index is the same for every vault:

- `status: stable` reads as `active`. `draft` and `deprecated` mean the same in both.
- When a document has no `updated`, its last change comes from `generated.at`, then `timestamp`. Listings,
  search results and `query` show that date.
- `type`, `resource`, `sources`, `generated`, `verified` and the other OKF keys are kept as written, and
  `query` shows them in the `extra` column, so `json_extract(extra, '$.type')` filters by type.
- Links from the vault root (`/tables/orders.md`) resolve, as in every vault.

## What a change may do

On top of the rules in [documents.md](documents.md#what-a-change-may-do):

- Agents may also set `type`, `resource`, `sources`, `usage_window`, `verified` and `stale_after`. Other keys
  stay locked unless the vault lists them under `frontmatter.allow`, which covers OKF's Attested Computation
  keys (`runtime`, `parameters`, `computation`, `executor`, `attester`).
- A concept needs a non-empty `type`. A document that had none, written before the vault used speki, may
  keep going without one.
- `index.md` and `log.md` aren't concepts: they need no `type`. A nested `index.md` can't gain frontmatter,
  and the root `index.md` may only add `okf_version`. Frontmatter they already have is left alone.

`verified` isn't checked against who makes the change: an agent can write `by: human:<id>`. OKF treats
trust tiers as advisory, and git history shows who made each change.

## Dates

Speki doesn't set `created` or `updated` in an OKF vault. It records the last change where OKF does:

- A document with `generated` gets `generated.by` and `generated.at`. Other keys in `generated` are kept.
- A document with `timestamp` gets `timestamp`.
- A new document gets `generated`, or `timestamp` when the root `index.md` declares `okf_version: "0.1"`.
- A document that has `updated` gets it too, so the date speki reads stays current.
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
