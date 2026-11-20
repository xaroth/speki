## OKF vaults

Documents in a vault with format OKF follow the Open Knowledge Format
(https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md). On top of the rules
above:

- Every document needs `type`: a short name for what it describes, such as `Reference`, `Guide` or
  `Playbook`. Reuse the types the vault already has.
- You may also set `resource` (the URI of the thing described), `sources` (a list of `{ id, resource,
  title }` it derives from, cited in the body with footnotes like `[^id]`), `usage_window`, `verified`
  (a list of `{ by, at }`) and `stale_after`.
- `status` is `draft`, `stable` (the default) or `deprecated`.
- Speki sets `generated` (`by` and `at`), or `timestamp` where the document or vault uses it, instead
  of `created` and `updated`.
- `index.md` lists its folder's documents, each as `* [Title](./path.md) - description`, under `#`
  headings. Add a line when you create a document and fix it when you move or delete one. It has no
  frontmatter, except `okf_version` in the vault's root `index.md`. `log.md` holds `## YYYY-MM-DD`
  headings, newest first.
- Prefer links from the vault root, like `[orders](/tables/orders.md)`.
