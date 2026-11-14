# github-core-clone-01 — standalone GitHub-compatible git forge

A single-file, stdlib-only Python 3.13 program (`main.py`, ~2,900 lines) that
serves git smart HTTP, the REST v3 API, GraphQL v4 and MCP from its own
in-memory store. No UI, no dependencies, no paid API key, fully deterministic.

Contest: https://make.fun/bounty/github-core-clone-01 — $500 USDC,
deadline Sat 21 Nov 13:30 WIB (2026-11-21T06:30:00Z).

## Run it

```sh
MAKEFUN_SHOP_TOKEN=shpat_<32 hex> FORGE_PORT=18099 python3 main.py
```

With no `MAKEFUN_ENV_URL` set it serves locally on `$FORGE_PORT`
(default 0 = ephemeral). In the trial the harness sets `MAKEFUN_ENV_URL`
and `MAKEFUN_SHOP_TOKEN`; the entry then follows the kit lifecycle:

1. **Bind an ephemeral port** (port 0, never 8080 — that is the env's port).
2. **Register**: `POST $MAKEFUN_ENV_URL/register` with `{"port": N}`.
   First register wins (later ones get 409); retried with backoff.
3. **Wait for done**: poll `GET $MAKEFUN_ENV_URL/done` until
   `{"done": true}`, then exit 0.

## The surface

| Route | What it is |
| --- | --- |
| `GET /healthz` | 200 `{"ok": true}` — the env's boot probe |
| `GET /api/v3/` | API root, `X-OAuth-Scopes: repo, read:org` |
| `/api/v3/...` | REST v3: repos, branches, commits, contents, issues, comments, labels, pull requests, files, merges |
| `POST /api/graphql` | GraphQL v4: the documents `gh` and `github-mcp-server` send |
| `POST /mcp` | MCP over streamable HTTP: initialize, server/discover, tools/list, tools/call, ping |
| `/<owner>/<repo>.git/...` | git smart HTTP: `info/refs`, `git-upload-pack` (v0 + v2), `git-receive-pack` |
| anything else | 404 `{"message": "Not Found"}` |

Every API reply carries a non-empty `X-GitHub-Request-Id`. Bearer auth is
`MAKEFUN_SHOP_TOKEN`; a missing or wrong token is 401, a repo under another
owner is 404 (never 403).

## How it works

- **Store**: one in-memory store (repos, refs, objects, issues, pulls,
  labels, counters). All ids are counters; all server timestamps come from
  `MAKEFUN_TRIAL_NOW` (or a fixed epoch), so two runs of the same trial
  produce identical responses.
- **Git objects**: own SHA-1 object store with packfile parse (plain,
  OFS-delta, REF-delta) and generation (undeltified). `git` itself is never
  invoked — the trial has no git binary.
- **Protocol v0 and v2**: v0 advertisement (symref capability, peeled tag
  lines), v0 upload/receive-pack with sideband framing; v2 advertisement
  (`ls-refs=unborn`, `fetch=shallow wait-for-done`, `object-format=sha1`),
  ls-refs (symrefs, peel, ref-prefix, unborn), fetch with acknowledgments
  (`ACK <oid>` + bare `ready`), shallow-info (`shallow`/`unshallow`),
  `deepen` (absolute and `deepen-relative`) and sideband packfiles.
- **Deterministic scenario**: the viewer login is derived at startup by
  porting the kit's own RNG (fnv1a64 / mix64 / xorshift64*) and name lists,
  with a hardcoded fallback to `adaiversen`.
- **GraphQL**: a hand-rolled parser + executor for exactly the documents
  the suite's clients send, including opaque stable global ids, cursor
  pagination, mutations and introspection.
- **MCP**: JSON-RPC 2.0 with six tools (`create_repo`, `get_repo`,
  `create_issue`, `list_issues`, `create_pull_request`,
  `merge_pull_request`) acting on the same store as REST.

## Validation

Local smoke tests (not part of the entry): `/tmp/smoke.py` (60 checks) and
`/tmp/smoke2.py` (36 checks) — REST, real `git` CLI push/clone/fetch
(v0 and v2, full and shallow, deepen, annotated tags, delete-only push,
100-file push), GraphQL, MCP. Both pass 100%. See BUILD_LOG.md for the
full history, including the genuine revisions made after the first run.

## Layout

- `main.py` — the whole forge (entry point).
- `LICENSE` — MIT, copied from the kit starter.
- `README.md` — this file.
- `BUILD_LOG.md` — approach, validation results, revision history.
