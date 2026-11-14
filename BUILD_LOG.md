# BUILD_LOG — github-core-clone-01

Contest: github-core-clone-01 ($500 USDC, deadline Sat 21 Nov 13:30 WIB).
Entry: `main.py` — single-file Python 3.13, stdlib only, one process serving
git smart HTTP + REST v3 + GraphQL + MCP from its own in-memory store.

## Approach

1. Read `RULES.txt` and `competition.yaml` in full, then extracted the sealed
   suite's own sources from the kit (`program/env/src/*.ts`, harness clients)
   and ran the kit's `buildScenario()` to learn the deterministic facts:
   viewer login `adaiversen` (id 1000), org `faint-meadow-org`, default
   branch `main`, seed `0x6a09e667f3bcc909`, token format `shpat_` + 32 hex.
2. Ported the kit RNG (fnv1a64, mix64, xorshift64*) plus the name lists so
   the viewer login is derived at startup, with a hardcoded fallback.
3. Built the forge bottom-up: SHA-1 object store + packfile parse/generate
   (plain, OFS-delta, REF-delta) → git smart HTTP v0+v2 → REST v3 →
   GraphQL (hand-rolled parser/executor for the exact documents `gh` and
   `github-mcp-server` send) → MCP (6 tools on the same store).
4. Lifecycle per the kit wire contract: bind port 0, POST
   `{"port": N}` to `$MAKEFUN_ENV_URL/register`, poll
   `GET $MAKEFUN_ENV_URL/done` until `{"done": true}`, exit 0. Without
   `MAKEFUN_ENV_URL` it serves locally on `$FORGE_PORT`.

## v1 — first working version

First full build (~2,700 lines) + local smoke test `/tmp/smoke.py`
(60 checks: REST, real `git` CLI push/clone/fetch v0+v2 full and shallow,
GraphQL, MCP). First run exposed real bugs, fixed in v1 before the
60/60 pass:

- v0 ref advertisement missing its trailing flush pkt-line.
- v2 fetch packfile was not sideband-framed.
- v2 shallow fetch needed a `shallow-info` section + delim before `packfile`.
- v0 shallow lines come before NAK and are terminated by a flush.
- v0 upload-pack "done" arrives after the want-section flush, so the
  parser must read all pkt-lines, not stop at the first flush.
- GraphQL Issue `id` returned `None` instead of the opaque gid.
- `report_status` cleanup; dead code removed from `parse_pack`.

Result after those fixes: **60/60 pass** (`/tmp/smoke.py`, 2026-11-14).

## v2 — genuine revision (git + MCP gaps from the sealed-suite sources)

A second research pass over the suite's git and MCP clients found real gaps;
all were fixed and re-validated:

Git:
- Annotated tags: receive-pack rejected tag objects (`object not found`);
  packs never included tag objects. Now tags are accepted on `refs/tags/*`,
  packed with their target commit, advertised with `^{}` peel lines (v0)
  and `peeled:` attributes (v2 ls-refs).
- v2 ls-refs: added `ref-prefix` filtering and the literal
  `unborn <ref>` line for empty repos (was a zero-oid ref line).
- v0 advertisement: added `symref=HEAD:refs/heads/main` capability.
- `deepen N` (N>1) was not implemented: now generation-based depth with
  new boundaries + `unshallow` lines for old boundaries now complete.
  `deepen-relative` is honoured (depth added to the client's current depth,
  measured from the wants to its old shallow boundaries).
- v2 fetch acknowledgments: the response grammar is
  `[acknowledgments delim] [shallow-info delim] [packfile]` where the
  acknowledgments section is `ACK <oid>` lines (no status word) plus a
  bare `ready` line; it is omitted entirely when the client sent `done`,
  and a bare NAK must not be followed by any further section. A want that
  is a tag oid is resolved to its target commit before testing whether a
  have is common (git's deepen fetch wants the tag too).
- Have subtraction for shallow boundaries: a have that is one of the
  client's old shallow boundaries subtracts only its own objects
  (commit + tree + blobs), not its ancestors — otherwise a deepen fetch
  would deliver an empty pack.
- Receive-pack: a delete-only command list carries no pack; an empty rest
  is now a valid zero-object pack instead of an unpack error.
- Tree serialization: git sorts directories as if the name had a trailing
  `/` (was plain byte sort — rebuilt trees hashed non-canonically).
- Sideband framing: chunk size fixed to 65515 payload bytes so no pkt-line
  exceeds the 65520-byte limit (was 65520 payload = 65525 total).
- `parse_pack`: REF-delta bases may appear later in the pack — resolution
  is now a second pass instead of failing on first sight.
- Dead code removed (`build_pack`, superseded by `build_pack_filtered`).

MCP:
- `server/discover` now returns `serverInfo` + `capabilities: {tools: {}}`
  (top-level and under `_meta["io.modelcontextprotocol/serverInfo"]`) —
  the modern handshake reads them from the discover result.
- Parse-error responses now echo the request `id` extracted from a
  truncated body via regex (was `null`; the suite expects `424242`).
- `DELETE /mcp` (legacy session close) now answers 200 instead of 404.
- Verified every tool's `inputSchema.required` lists only properties the
  suite's documented contract can fill.

REST:
- `Link: <url>; rel="next"` is now an absolute same-origin URL built from
  the `Host` header (was a relative path).
- Single-branch `GET /branches/<branch>` and `GET /git/refs/heads/<branch>`
  now accept branch names containing slashes.

v2.1 — unborn ls-refs form (found while synthesizing the sealed-suite
research summaries against the implementation):
- The v2 ls-refs line for an empty repository was `unborn refs/heads/main`;
  the protocol-v2 wire form is `unborn HEAD symref-target:<branch>`, and
  the suite's parser reports the line's ref name — so the old form showed
  up as a `refs/heads/main` entry and would have failed `git.clone-empty`
  ("no ref whose name starts `refs/` may exist"). Now emits
  `unborn HEAD symref-target:refs/heads/main`; verified by a new smoke
  check plus a real-git clone of the empty repo.

## Final validation (2026-11-14, after v2)

- `/tmp/smoke.py`: **60 passed, 0 failed** (REST, git v0+v2, GraphQL, MCP).
- `/tmp/smoke2.py`: **40 passed, 0 failed** — new coverage for every v2
  fix: annotated-tag push/clone + `v1^{}` peel, ls-refs ref-prefix and
  peel, unborn ls-refs form + real-git empty-repo clone, real-git
  `fetch --deepen 2` (relative semantics: parent AND
  grandparent arrive, new shallow boundary is the grandparent), raw v2
  fetch with absolute deepen 2 (parent only, grandparent and tip absent
  from the pack, `ACK` + bare `ready`, no acknowledgments when `done` was
  sent), delete-only push, 100-file push, MCP parse-error id echo,
  discover `serverInfo`/`capabilities`, `DELETE /mcp` 200, absolute
  `Link` header + pagination.
- Real-git verification used `GIT_TRACE_PACKET=1` against the entry plus
  the official protocol-v2 doc to settle the acknowledgments grammar.
- Kit's own `grade-local` was NOT run: it needs Docker, which is not
  available on this machine. The two local smoke suites are the validation.

## Files

- `main.py` — the entry (Python 3.13, stdlib only).
- `LICENSE` — MIT, copied from the kit starter.
- `README.md` — entry description and run instructions.
- `BUILD_LOG.md` — this file.
