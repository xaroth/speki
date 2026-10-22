# schema

Describes the views [`query`](query.md) can use: their columns and what they mean, example queries, and the
limits. Agents call it before their first query.

## Input

None.

## Output

```json
{ "schema": "# Querying speki\n\n`query` runs one read-only SQLite `SELECT` ..." }
```

The text rendering is the same text. It's embedded in the binary from `mcpserver/schema.md`, the source of
truth for what agents are told. [query.md](../query.md) describes the same views for people.

The text says the views are the interface, and that the tables behind them can be read but change without
notice. That way the index schema stays free to change: an index with another schema version is rebuilt
from git, with no migrations.

## Errors

None of its own.
