---
name: postgres-jsonb
description: Models, queries and indexes JSONB in PostgreSQL - deciding what belongs in columns versus jsonb, operators (->, ->>, @>, ?, jsonb_path_query, SQL/JSON JSON_TABLE and JSON_VALUE), partial updates with jsonb_set and subscripting, GIN jsonb_ops vs jsonb_path_ops and expression indexes, CHECK constraints for shape, and the statistics and TOAST traps. Use when storing API payloads, settings or flexible attributes in Postgres, when a jsonb query is slow or ignores its index, or when deciding whether a field should be promoted to a real column.
license: MIT
---

# Postgres JSONB

JSONB is right for data that is read and written as a whole, varies per row, or comes
from outside (webhook payloads, provider responses, per-user preferences). Anything you
filter, join, sort, aggregate or constrain on regularly belongs in a typed column. Done
well means every jsonb query has a matching index, the shape is guarded by constraints
where it matters, and nobody parses the same key out of the blob in five places.

## Decide: column or jsonb

| Signal | Column | jsonb |
|---|---|---|
| Used in WHERE/JOIN/ORDER BY on hot paths | Yes | Only with a dedicated index |
| Needs a foreign key or uniqueness | Yes | No (can't reference into jsonb) |
| Same keys on every row, known types | Yes | No |
| Shape differs per row or per provider | No | Yes |
| Stored for audit/replay, rarely queried | No | Yes |
| Updated frequently, one key at a time on large documents | Yes | Avoid; each update rewrites the whole value |

A common healthy pattern: core fields as columns, plus `attributes jsonb` for the long
tail, plus promotion of a key into a column once it becomes hot.

## Operators that matter

```sql
data -> 'customer'            -- jsonb
data ->> 'status'             -- text
data #>> '{customer,email}'   -- text at path
data @> '{"status":"paid"}'   -- containment, GIN-indexable
data ? 'refund_id'            -- top-level key exists, GIN (jsonb_ops) indexable
data ?| array['a','b']        -- any key exists
data @? '$.items[*] ? (@.qty > 10)'   -- jsonpath exists, GIN-indexable
jsonb_path_query(data, '$.items[*].sku')   -- set-returning
(data ->> 'amount')::numeric > 100    -- cast before comparing numbers
```

- `->>` returns text: `data->>'amount' > '100'` compares strings ('9' > '100'). Cast.
- JSON `null` and SQL NULL differ: `data->'x'` is `'null'::jsonb` for `{"x":null}` and
  SQL NULL for a missing key. `data->>'x'` is SQL NULL in both cases.
- Keys are case-sensitive and exact; `{"Status":...}` is not `status`.

SQL/JSON (standard syntax) is available in recent versions: `IS JSON` (16+),
`JSON_EXISTS`, `JSON_QUERY`, `JSON_VALUE` and `JSON_TABLE` (17+). `JSON_TABLE` turns
arrays into rows cleanly:

```sql
SELECT o.id, li.*
FROM orders o,
     JSON_TABLE(o.data, '$.line_items[*]' COLUMNS (
        sku text PATH '$.sku',
        qty int  PATH '$.quantity',
        price numeric PATH '$.unit_price')) AS li;
```

On older versions use `jsonb_to_recordset(data->'line_items') AS li(sku text, quantity int)`
or `jsonb_array_elements`. Check `SELECT version()` before choosing.

## Writing and updating

```sql
-- set one key (creates it if missing when create_if_missing is true, the default)
UPDATE accounts SET settings = jsonb_set(settings, '{notifications,email}', 'false')
WHERE id = $1;

-- PG 14+ subscripting
UPDATE accounts SET settings['theme'] = '"dark"' WHERE id = $1;

-- shallow merge (right side wins, top level only)
UPDATE accounts SET settings = settings || '{"beta": true}' WHERE id = $1;

-- remove key / remove path
UPDATE accounts SET settings = settings - 'legacy_flag' WHERE id = $1;
UPDATE accounts SET settings = settings #- '{notifications,sms}' WHERE id = $1;
```

- `jsonb_set` on a NULL column returns NULL: use `coalesce(settings, '{}')`.
- `jsonb_set` does not create missing intermediate objects; set the parent first or
  merge with `||`.
- `||` is a shallow merge; nested objects are replaced, not merged.
- Concurrent read-modify-write in application code loses updates. Do the change in one
  `UPDATE` with jsonb functions, or lock the row (`$database-transactions`).
- Parameters: bind JSON as a string parameter cast to jsonb (`$1::jsonb`); never build
  JSON with string concatenation in SQL.

## Indexing

| Query | Index |
|---|---|
| `data @> '{"status":"paid"}'`, `@?`, `@@` | `GIN (data jsonb_path_ops)`: smaller and faster for containment |
| Also need `?`, `?|`, `?&` key-exists | `GIN (data)` (default `jsonb_ops`) |
| `data->>'status' = 'paid'`, range or sort on one key | B-tree expression index `((data->>'status'))` |
| Numeric range on a key | `(((data->>'amount')::numeric))` and query with the identical cast |
| One key hot enough to deserve stats and constraints | Generated column + B-tree |

```sql
CREATE INDEX CONCURRENTLY orders_data_gin ON orders USING gin (data jsonb_path_ops);
CREATE INDEX CONCURRENTLY orders_status_idx ON orders ((data->>'status'));

ALTER TABLE orders ADD COLUMN status text
    GENERATED ALWAYS AS (data->>'status') STORED;   -- rewrites the table: plan it
```

- GIN indexes do not help `->>` equality; B-tree expression indexes do not help `@>`.
  Write the query to match the index you have, or vice versa.
- A GIN index on the whole document of a write-heavy table is expensive; index a
  sub-path (`USING gin ((data->'tags'))`) when queries only touch that part.
- Adding a STORED generated column rewrites the table under an exclusive lock; on large
  tables add a regular column and backfill instead (`$zero-downtime-migrations`).

## Statistics and planning trap

Postgres keeps no per-key statistics inside jsonb. `WHERE data @> '{"status":"failed"}'`
is estimated with a fixed default selectivity, so plans can be badly wrong in either
direction. Remedies: an expression index (which gets its own statistics after
`ANALYZE`), `CREATE STATISTICS` on the expression (14+), or promoting the key to a column.

## Constraining the shape

```sql
ALTER TABLE webhooks ADD CONSTRAINT webhooks_payload_is_object
    CHECK (jsonb_typeof(payload) = 'object') NOT VALID;
ALTER TABLE webhooks ADD CONSTRAINT webhooks_payload_has_id
    CHECK (payload ? 'id' AND jsonb_typeof(payload->'id') = 'string') NOT VALID;
-- then VALIDATE CONSTRAINT each one
```

Full JSON Schema validation needs an extension (`pg_jsonschema`, available on some
hosts) or app-side validation; keep database checks to the invariants that break code.

## Gotchas

- Values over about 2 kB are TOASTed (compressed and stored out of line). Reading one
  key still detoasts the whole value; very large documents make every read expensive.
- jsonb normalizes: key order is not preserved, duplicate keys keep the last value,
  whitespace is dropped. Store raw `json` or `text` alongside only if byte-exact
  replay (signature verification) is required.
- Numbers are stored as `numeric`; huge integers survive, but your driver may parse them
  into floats. Check the language side.
- ORMs: Prisma `Json` filters, Django `JSONField` lookups (`data__status`), SQLAlchemy
  `JSONB` comparators all emit `->`/`->>`/`@>`; read the generated SQL to see which index
  it can use.
- `SELECT DISTINCT` and `GROUP BY` on jsonb work (equality is defined), but are slow on big documents.

## Verification

- [ ] Each jsonb query on a hot path has an index that matches its operator
- [ ] `EXPLAIN ANALYZE` shows Bitmap Index Scan on the GIN or Index Scan on the expression index
- [ ] Numeric and date comparisons cast explicitly
- [ ] Updates done in single statements; no lost-update read-modify-write loops
- [ ] Shape invariants guarded by CHECK where code depends on them
- [ ] Keys queried in more than a few places reviewed for promotion to columns

## Output template

```markdown
## JSONB review: <table.column>
**Contents:** <what is stored, typical size, write rate>
**Queries:** <query -> operator -> index used>
**Keep in jsonb:** <keys and why>
**Promote to columns:** <keys, migration plan>
**Constraints:** <checks added>
**Indexes:** <DDL>
```
