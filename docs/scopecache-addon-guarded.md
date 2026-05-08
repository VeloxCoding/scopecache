# scopecache — addon: guarded

> Addon RFC for [`addons/guarded/`](../addons/guarded/). Companion
> to [scopecache-core-rfc.md](scopecache-core-rfc.md); see §11 of
> the core RFC for the generic addon framework.
>
> **Status: pre-v1.0 draft.** Tracks the core RFC's status; wording
> and shape remain open for revision.

---

## 1. Purpose

`addons/guarded/` exposes a single token-gated tail endpoint,
`/guarded-tail`, that confines a token holder to scopes prefixed
with that token's capability ID. The plaintext token never
reaches the cache; only its hash does.

## 2. Token model

A **capability ID** (`capID`) is `base64url(sha256(token))` — the
full SHA-256 of the bearer token, base64url-encoded without
padding, 43 characters long. The operator chooses each plaintext
token (any length, any character set); only the cache and the
client need the plaintext, and the cache never stores it.

Operator workflow:

- **Issue a token.** Compute `capID = base64url(sha256(token))`,
  then `POST /upsert` an item into the `_tokens` scope:
  ```json
  {"scope":"_tokens","id":"<capID>","payload":{}}
  ```
  The payload is opaque to the addon — operators may store audit
  metadata (issued_at, label) for their own bookkeeping; the addon
  never inspects it.
- **Revoke a token.** `POST /delete` on `(_tokens, <capID>)`.
- The addon never writes to `_tokens` — it is a read-only access gate.

`_tokens` is **not** one of the core's two reserved scopes (see
core RFC §2.6). The core makes no special promise about `_tokens`;
its contract is owned by this addon. The leading underscore is
the same naming convention used for other addon-managed
infrastructure scopes.

## 3. Scope namespacing

When a client calls `GET /guarded-tail?scope=foo` with a valid
bearer token, the addon dispatches the underlying tail to
`<capID>:foo` — never to `foo`. A token holder cannot reach scopes
outside their capID prefix; conversely, a capID has no way to
guess another capID's prefix.

The capID prefix is stripped from `item.scope` on the response so
the client sees only its own scope name. The hash never appears on
the wire to the client.

## 4. Endpoint: `GET /guarded-tail`

Token-gated tail. Mirrors core `/tail` (core RFC §6.4) semantics
on a scope confined to the token's capability prefix. Returns 200
in both the hit and miss case.

**Headers**

| header          | required | notes                                                                |
|-----------------|----------|----------------------------------------------------------------------|
| `Authorization` | yes      | `Bearer <token>` scheme; `sha256(token)` is looked up in `_tokens`   |

**Query parameters**

| parameter | type   | default | notes                                                |
|-----------|--------|---------|------------------------------------------------------|
| `scope`   | string | —       | required; the lookup runs against `<capID>:<scope>`  |
| `limit`   | int    | 1000    | clamped to ≤ 10000                                   |
| `offset`  | int    | 0       | skip this many items from the tail before reading    |

**Response (200, hit)**

```json
{
  "ok": true,
  "hit": true,
  "count": 3,
  "offset": 0,
  "truncated": false,
  "items": [
    {"scope": "demo", "seq": 1, "ts": 1700000000000000, "payload": {"msg": "first"}},
    {"scope": "demo", "seq": 2, "ts": 1700000000000001, "payload": {"msg": "second"}},
    {"scope": "demo", "seq": 3, "ts": 1700000000000002, "payload": {"msg": "third"}}
  ]
}
```

`item.scope` is the client's input scope; the cache-internal
`<capID>:<scope>` form is never serialised.

**Response (200, miss — no token-prefixed scope, or scope empty)**

```json
{
  "ok": true,
  "hit": false,
  "count": 0,
  "offset": 0,
  "truncated": false,
  "items": []
}
```

**Endpoint-specific errors**

| status | error                                                  | when                                                          |
|--------|--------------------------------------------------------|---------------------------------------------------------------|
| 401    | `missing or malformed Authorization header`            | header missing, not Bearer scheme, or empty token             |
| 401    | `invalid or unknown token`                             | token hashed to a capID that is not provisioned in `_tokens`  |
| 400    | `the 'scope' query parameter is required`              | `scope` omitted                                               |
| 400    | `the 'limit' parameter must be a positive integer`     | non-numeric or non-positive `limit`                           |
| 400    | `the 'offset' parameter must be a non-negative integer`| non-numeric or negative `offset`                              |

The access check runs before query parsing — a caller without a
valid token does no shape-validation work and learns nothing about
the addon's wire shape beyond what is publicly documented.

**Side effects**

A successful hit bumps the per-scope read-bookkeeping atomics
(core RFC §9) on the cache-internal `<capID>:<scope>` scope,
exactly like core `/tail`.

**Example**

```bash
# operator issues a token (one-time)
# id = base64url(sha256("123456")); compute with:
#   echo -n 123456 | openssl dgst -sha256 -binary | openssl base64 -A | tr +/ -_ | tr -d =
curl -s -X POST http://localhost:8080/upsert \
  -H 'Content-Type: application/json' \
  -d '{"scope":"_tokens","id":"jZae727K08KaOmKSgOaGzww_XVqGr_PKEgIMkjrcbJI","payload":{}}'

# operator pre-populates a token-owned scope
curl -s -X POST http://localhost:8080/append \
  -H 'Content-Type: application/json' \
  -d '{"scope":"jZae727K08KaOmKSgOaGzww_XVqGr_PKEgIMkjrcbJI:demo","payload":{"msg":"first"}}'

# client uses the plaintext token; the addon does the hash + prefix
curl -s 'http://localhost:8080/guarded-tail?scope=demo' \
  -H 'Authorization: Bearer 123456'
# → {"ok":true,"hit":true,"count":1,"offset":0,"truncated":false,"items":[{"scope":"demo","seq":1,"ts":...,"payload":{"msg":"first"}}]}
```
