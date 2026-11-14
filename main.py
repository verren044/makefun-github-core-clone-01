#!/usr/bin/env python3
"""github-core-clone-01: a standalone GitHub-compatible git forge, one process.

Serves git smart HTTP (upload-pack v2, receive-pack v0), REST v3 under
/api/v3, GraphQL v4 at /api/graphql and MCP at /mcp, all from an in-memory
store. Python 3.13 standard library only; no git binary, no network beyond
the suite at $MAKEFUN_ENV_URL.

Lifecycle (per the kit's wire contract): bind an ephemeral port, POST
{"port": N} to $MAKEFUN_ENV_URL/register, poll GET $MAKEFUN_ENV_URL/done
until {"done": true}, then exit 0. Without MAKEFUN_ENV_URL the server just
serves on $FORGE_PORT (default 0 -> ephemeral, printed on stdout) for
local smoke testing.

Determinism: every id is a counter, every timestamp comes from
MAKEFUN_TRIAL_NOW (or a fixed constant), no wall clock or randomness is
used anywhere in a response, so a live pass and its replay behave
identically.
"""

import base64
import hashlib
import json
import os
import re
import struct
import sys
import threading
import time
import urllib.parse
import urllib.request
import zlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# ---------------------------------------------------------------- config

TOKEN = os.environ.get("MAKEFUN_SHOP_TOKEN", "")
ENV_URL = os.environ.get("MAKEFUN_ENV_URL", "").rstrip("/")
TRIAL_NOW = os.environ.get("MAKEFUN_TRIAL_NOW", "") or "2026-11-14T06:30:00Z"
# Fixed fallback timestamp used for server-created commits/issues when no
# trial clock is available (local smoke tests). Never a wall clock.
FIXED_NOW = "2026-11-14T06:30:00Z"
VERSION = "1.0.0-forge"

ZERO_SHA = "0" * 40


def now_iso():
    return TRIAL_NOW


# ------------------------------------------------------- scenario logins
# The suite's scenario is drawn from one constant seed (kit wire.ts,
# SCENARIO_SEED = 0x6a09e667f3bcc909). The viewer/stranger logins are the
# same in every trial. Port the generator exactly and compute them; fall
# back to the verified values if anything ever differs.

_U64 = (1 << 64) - 1
_FIRST = ['ada', 'bram', 'cora', 'dmitri', 'elena', 'felix', 'greta', 'hugo',
          'iris', 'jonas', 'kira', 'lars', 'mira', 'nico', 'olga', 'pavel',
          'quinn', 'rosa', 'sven', 'tess', 'ulla', 'vito', 'wren', 'yara',
          'zane']
_LAST = ['anders', 'bauer', 'costa', 'dahl', 'eriksen', 'fischer', 'garcia',
         'holm', 'iversen', 'jansen', 'keller', 'lind', 'moreau', 'novak',
         'olsen', 'peters', 'quist', 'rossi', 'sato', 'turi', 'ulrich',
         'vega', 'weber', 'xu', 'yilmaz', 'zimmer']


def _fnv1a64(text):
    h = 0xcbf29ce484222325
    for ch in text:
        h ^= ord(ch)
        h = (h * 0x100000001b3) & _U64
    return h


def _mix64(v):
    z = (v + 0x9e3779b97f4a7c15) & _U64
    z = ((z ^ (z >> 30)) * 0xbf58476d1ce4e5b9) & _U64
    z = ((z ^ (z >> 27)) * 0x94d049bb133111eb) & _U64
    return (z ^ (z >> 31)) & _U64


class _Rng:
    def __init__(self, seed):
        self.state = seed & _U64
        if self.state == 0:
            self.state = 0x6a09e667f3bcc909

    def next_u64(self):
        x = self.state
        x = (x ^ (x >> 12)) & _U64
        x = (x ^ (x << 25)) & _U64
        x = (x ^ (x >> 27)) & _U64
        self.state = x
        return (x * 0x2545f4914f6cdd1d) & _U64

    def next_u32(self):
        return self.next_u64() >> 32

    def next_int(self, n):
        if n == 1:
            return 0
        limit = 4294967296 - (4294967296 % n)
        while True:
            v = self.next_u32()
            if v < limit:
                return v % n

    def pick(self, items):
        return items[self.next_int(len(items))]


def _scenario_users():
    rng = _Rng(_mix64(0x6a09e667f3bcc909 ^ _fnv1a64("users")))
    used, users = set(), []
    for i in range(12):
        first, last = rng.pick(_FIRST), rng.pick(_LAST)
        login = re.sub(r"[^a-z0-9-]", "", first + last)
        suffix = 2
        while login in used:
            login = "%s-%d" % (first + last, suffix)
            suffix += 1
        used.add(login)
        users.append({"id": 1000 + i, "login": login,
                      "name": "%s %s" % (first, last),
                      "email": "%s@example.invalid" % login})
    return users


try:
    _USERS = _scenario_users()
    VIEWER = _USERS[0]
    STRANGER = _USERS[2]
except Exception:
    VIEWER = {"id": 1000, "login": "adaiversen", "name": "ada iversen",
              "email": "adaiversen@example.invalid"}
    STRANGER = {"id": 1002, "login": "olgabauer", "name": "olga bauer",
                "email": "olgabauer@example.invalid"}

VIEWER_LOGIN = VIEWER["login"]


# ---------------------------------------------------------------- store

class Store:
    """All forge state, guarded by one re-entrant lock."""

    def __init__(self):
        self.lock = threading.RLock()
        self.repos = {}          # (owner, name) -> Repo
        self.repo_order = []     # creation order of (owner, name)
        self.next_repo_dbid = 1
        self.next_gid = 1        # global counter for opaque GraphQL ids
        self.next_seq = 1        # monotonic creation sequence (ordering)
        self.req_counter = 0

    def seq(self):
        s = self.next_seq
        self.next_seq += 1
        return s

    def gid(self, kind):
        n = self.next_gid
        self.next_gid += 1
        return "%s%d" % (kind, n)

    def request_id(self):
        self.req_counter += 1
        return "req-%06d" % self.req_counter


STORE = Store()


class Repo:
    def __init__(self, name, owner, description, private, dbid):
        self.name = name
        self.owner = owner
        self.description = description or ""
        self.private = bool(private)
        self.dbid = dbid
        self.default_branch = "main"
        self.created_seq = STORE.seq()
        # git state
        self.objects = {}        # sha -> (type, bytes)
        self.refs = {}           # "refs/heads/<branch>" -> sha ("" = unborn)
        self.head_ref = "refs/heads/main"
        # forge state
        self.labels = {}         # name -> {"name":..., "color":...}
        self.issues = {}         # number -> Issue
        self.issue_order = []
        self.next_issue = 1
        self.pulls = {}          # number -> Pull
        self.pull_order = []
        self.next_pull = 1

    def full_name(self):
        return "%s/%s" % (self.owner, self.name)

    def gid(self):
        return "R%d" % self.dbid

    def branch_tip(self, branch):
        return self.refs.get("refs/heads/" + branch, "")

    def branches(self):
        return sorted(b for b in self.refs if b.startswith("refs/heads/"))


class Issue:
    def __init__(self, repo, number, title, body, labels):
        self.repo = repo
        self.number = number
        self.title = title
        self.body = body if body is not None else ""
        self.state = "open"
        self.labels = list(labels)
        self.comments = []       # {"id": int, "body": str}
        self.next_comment = 1
        self.created_seq = STORE.seq()
        self.gid = STORE.gid("I")
        self.created_at = now_iso()
        self.updated_at = now_iso()

    def rest(self):
        return {
            "id": self.number * 100000 + self.repo.dbid,
            "node_id": self.gid,
            "number": self.number,
            "title": self.title,
            "body": self.body,
            "state": self.state,
            "labels": [{"name": n} for n in self.labels],
            "user": {"login": VIEWER_LOGIN, "id": VIEWER["id"]},
            "created_at": self.created_at,
            "updated_at": self.updated_at,
            "html_url": "",
            "url": "",
        }


class Pull:
    def __init__(self, repo, number, title, body, head, base, head_sha,
                 base_sha):
        self.repo = repo
        self.number = number
        self.title = title
        self.body = body if body is not None else ""
        self.head = head
        self.base = base
        self.head_sha = head_sha
        self.base_sha = base_sha
        self.state = "open"
        self.merged = False
        self.merge_commit_sha = None
        self.merged_at = None
        self.created_seq = STORE.seq()
        self.gid = STORE.gid("P")
        self.created_at = now_iso()
        self.updated_at = now_iso()

    def rest(self):
        d = {
            "id": self.number * 100000 + self.repo.dbid,
            "node_id": self.gid,
            "number": self.number,
            "title": self.title,
            "body": self.body,
            "state": self.state,
            "head": {"ref": self.head, "sha": self.head_sha or ZERO_SHA,
                     "label": "%s:%s" % (self.repo.owner, self.head)},
            "base": {"ref": self.base, "sha": self.base_sha or ZERO_SHA,
                     "label": "%s:%s" % (self.repo.owner, self.base)},
            "merged": self.merged,
            "mergeable": True,
            "user": {"login": VIEWER_LOGIN, "id": VIEWER["id"]},
            "created_at": self.created_at,
            "updated_at": self.updated_at,
            "html_url": "",
            "url": "",
        }
        if self.merged:
            d["merged_at"] = self.merged_at
            d["merge_commit_sha"] = self.merge_commit_sha
        return d


# ------------------------------------------------------------- git objects

def git_hash(otype, data):
    h = hashlib.sha1()
    h.update(otype.encode() + b" " + str(len(data)).encode() + b"\x00" + data)
    return h.hexdigest()


def store_object(repo, otype, data):
    sha = git_hash(otype, data)
    repo.objects.setdefault(sha, (otype, data))
    return sha


def get_object(repo, sha):
    return repo.objects.get(sha)


def parse_tag(data):
    """Return the target sha of an annotated tag object."""
    head, _, message = data.partition(b"\n\n")
    target = None
    for line in head.split(b"\n"):
        if line.startswith(b"object "):
            target = line[7:].decode()
    return target, message


def parse_commit(data):
    """Return (tree_sha, [parent_shas], message_bytes, raw_headers)."""
    head, _, message = data.partition(b"\n\n")
    tree = None
    parents = []
    for line in head.split(b"\n"):
        if line.startswith(b"tree "):
            tree = line[5:].decode()
        elif line.startswith(b"parent "):
            parents.append(line[7:].decode())
    return tree, parents, message, head


def make_commit(repo, tree_sha, parents, message, author=None):
    who = author or ("%s <%s>" % (VIEWER["name"], VIEWER["email"]))
    ts = 1735689600  # 2025-01-01T00:00:00Z, fixed: deterministic
    ident = ("%s %d +0000" % (who, ts)).encode()
    out = b"tree " + tree_sha.encode() + b"\n"
    for p in parents:
        out += b"parent " + p.encode() + b"\n"
    out += b"author " + ident + b"\ncommitter " + ident + b"\n\n"
    out += message if isinstance(message, bytes) else message.encode()
    return store_object(repo, "commit", out)


def parse_tree(data):
    """Return list of (mode_str, name_bytes, sha_hex)."""
    out = []
    i = 0
    while i < len(data):
        sp = data.index(b" ", i)
        mode = data[i:sp].decode()
        nul = data.index(b"\x00", sp)
        name = data[sp + 1:nul]
        sha = data[nul + 1:nul + 21].hex()
        out.append((mode, name, sha))
        i = nul + 21
    return out


def build_tree(entries):
    out = b""
    # git sorts tree entries with directories compared as if their name
    # had a trailing slash.
    keyed = sorted(entries, key=lambda e: e[1] + b"/"
                   if e[0] == "40000" else e[1])
    for mode, name, sha in keyed:
        out += mode.encode() + b" " + name + b"\x00" + bytes.fromhex(sha)
    return out


def tree_get(repo, tree_sha, path_parts):
    """Resolve path parts in a tree; return (mode, sha) or None."""
    cur = tree_sha
    for idx, part in enumerate(path_parts):
        obj = get_object(repo, cur)
        if obj is None or obj[0] != "tree":
            return None
        found = None
        for mode, name, sha in parse_tree(obj[1]):
            if name == part.encode():
                found = (mode, sha)
                break
        if found is None:
            return None
        if idx == len(path_parts) - 1:
            return found
        cur = found[1]
    return None


def tree_set(repo, tree_sha, path_parts, mode, blob_sha):
    """Return new tree sha with path set to (mode, blob_sha)."""
    if not path_parts:
        return tree_sha
    entries = []
    if tree_sha:
        obj = get_object(repo, tree_sha)
        if obj is not None and obj[0] == "tree":
            entries = parse_tree(obj[1])
    part = path_parts[0]
    rest = path_parts[1:]
    out = []
    replaced = False
    for m, name, sha in entries:
        if name == part.encode():
            replaced = True
            if rest:
                new_sub = tree_set(repo, sha, rest, mode, blob_sha)
                out.append((m, name, new_sub))
            else:
                if blob_sha is None:
                    continue  # delete
                out.append((mode, name, blob_sha))
        else:
            out.append((m, name, sha))
    if not replaced and blob_sha is not None:
        if rest:
            new_sub = tree_set(repo, None, rest, mode, blob_sha)
            out.append(("40000", part.encode(), new_sub))
        else:
            out.append((mode, part.encode(), blob_sha))
    return store_object(repo, "tree", build_tree(out))


def is_ancestor(repo, anc, desc):
    """True when commit anc is reachable from commit desc (or equal)."""
    if anc == desc:
        return True
    if not anc or not desc:
        return False
    seen = set()
    stack = [desc]
    while stack:
        cur = stack.pop()
        if cur == anc:
            return True
        if cur in seen:
            continue
        seen.add(cur)
        obj = get_object(repo, cur)
        if obj is None or obj[0] != "commit":
            continue
        _, parents, _, _ = parse_commit(obj[1])
        stack.extend(parents)
    return False


def commit_history(repo, tip, limit=1000):
    """Commits reachable from tip, newest-first (BFS by generation)."""
    out = []
    seen = set()
    queue = [(tip, 0)]
    while queue and len(out) < limit:
        cur, _ = queue.pop(0)
        if cur in seen or not cur:
            continue
        seen.add(cur)
        obj = get_object(repo, cur)
        if obj is None or obj[0] != "commit":
            continue
        _, parents, _, _ = parse_commit(obj[1])
        out.append(cur)
        for p in parents:
            queue.append((p, 0))
    return out


def tree_paths(repo, tree_sha, prefix=""):
    """All (path, mode, sha) file entries under a tree, recursively."""
    obj = get_object(repo, tree_sha)
    if obj is None or obj[0] != "tree":
        return []
    out = []
    for mode, name, sha in parse_tree(obj[1]):
        path = prefix + name.decode("utf-8", "replace")
        if mode == "40000":
            out.extend(tree_paths(repo, sha, path + "/"))
        else:
            out.append((path, mode, sha))
    return out


def diff_paths(repo, base_tree, head_tree):
    """Paths whose blob sha differs between two trees (added/modified/deleted)."""
    b = {p: s for p, _, s in tree_paths(repo, base_tree)} if base_tree else {}
    h = {p: s for p, _, s in tree_paths(repo, head_tree)} if head_tree else {}
    changed = set()
    for p in set(b) | set(h):
        if b.get(p) != h.get(p):
            changed.add(p)
    return changed


# ------------------------------------------------------------ pkt-line / pack

def pkt_line(data):
    if isinstance(data, str):
        data = data.encode()
    return ("%04x" % (len(data) + 4)).encode() + data


FLUSH = b"0000"
DELIM = b"0001"


def pkt_lines_all(data):
    """All pkt-line payloads in a stream, ignoring flush/delim markers
    (they separate sections but lines after them still matter, e.g. the
    v0 upload-pack "done" that follows the want section's flush)."""
    lines = []
    i = 0
    while i + 4 <= len(data):
        n = int(data[i:i + 4], 16)
        if n == 0:
            lines.append("flush")
            i += 4
            continue
        if n == 1:
            lines.append("delim")
            i += 4
            continue
        lines.append(data[i + 4:i + n])
        i += n
    return lines, b""


def pkt_lines(data):
    """Split raw bytes into pkt-line payloads; returns (lines, rest).
    lines: list of bytes payloads (without length prefix); 'flush' for 0000,
    'delim' for 0001. rest: bytes after the first flush pkt (packfile)."""
    lines = []
    i = 0
    while i + 4 <= len(data):
        n = int(data[i:i + 4], 16)
        if n == 0:
            return lines + ["flush"], data[i + 4:]
        if n == 1:
            lines.append("delim")
            i += 4
            continue
        lines.append(data[i + 4:i + n])
        i += n
    return lines, b""


def sideband_pack(pack_bytes, chan=b"\x01"):
    out = bytearray()
    # A pkt-line is at most 65520 bytes total (4 length + payload), so a
    # sideband chunk carries at most 65515 pack bytes beside its 1-byte
    # channel marker.
    for i in range(0, len(pack_bytes), 65515):
        chunk = chan + pack_bytes[i:i + 65515]
        out += pkt_line(chunk)
    return bytes(out)


def _read_varint(data, pos):
    result = 0
    shift = 0
    while True:
        b = data[pos]
        pos += 1
        result |= (b & 0x7f) << shift
        shift += 7
        if not (b & 0x80):
            break
    return result, pos


def apply_delta(base, delta):
    src_size, pos = _read_varint(delta, 0)
    if src_size != len(base):
        raise ValueError("delta base size mismatch")
    tgt_size, pos = _read_varint(delta, pos)
    out = bytearray()
    while pos < len(delta):
        op = delta[pos]
        pos += 1
        if op & 0x80:
            off = size = 0
            for i in range(4):
                if op & (1 << i):
                    off |= delta[pos] << (8 * i)
                    pos += 1
            for i in range(3):
                if op & (0x10 << i):
                    size |= delta[pos] << (8 * i)
                    pos += 1
            if size == 0:
                size = 0x10000
            out += base[off:off + size]
        elif op:
            out += delta[pos:pos + op]
            pos += op
        else:
            raise ValueError("delta opcode 0")
    if len(out) != tgt_size:
        raise ValueError("delta target size mismatch")
    return bytes(out)


def parse_pack(data):
    """Parse a packfile; return list of (type, bytes). Raises on bad data."""
    if len(data) < 12 or data[:4] != b"PACK":
        raise ValueError("not a packfile")
    version, count = struct.unpack(">II", data[4:12])
    if version != 2:
        raise ValueError("pack version %d" % version)
    pos = 12
    objs = []          # (type_str, data) in file order
    by_offset = {}     # offset -> index into objs
    deferred = []      # (objs index, base_sha, delta) for ref-deltas
    for _ in range(count):
        start = pos
        b = data[pos]
        pos += 1
        otype = (b >> 4) & 7
        size = b & 0x0f
        shift = 4
        while b & 0x80:
            b = data[pos]
            pos += 1
            size |= (b & 0x7f) << shift
            shift += 7
        if otype in (1, 2, 3, 4):
            tname = {1: "commit", 2: "tree", 3: "blob", 4: "tag"}[otype]
            d = zlib.decompressobj()
            raw = d.decompress(data[pos:])
            pos += len(data[pos:]) - len(d.unused_data)
            if len(raw) != size:
                raise ValueError("object size mismatch")
            by_offset[start] = len(objs)
            objs.append((tname, raw))
        elif otype == 6:  # ofs-delta
            b = data[pos]
            pos += 1
            off = b & 0x7f
            while b & 0x80:
                b = data[pos]
                pos += 1
                off = ((off + 1) << 7) | (b & 0x7f)
            base_idx = by_offset[start - off]
            d = zlib.decompressobj()
            delta = d.decompress(data[pos:])
            pos += len(data[pos:]) - len(d.unused_data)
            base_type, base_data = objs[base_idx]
            raw = apply_delta(base_data, delta)
            by_offset[start] = len(objs)
            objs.append((base_type, raw))
        elif otype == 7:  # ref-delta
            base_sha = data[pos:pos + 20].hex()
            pos += 20
            d = zlib.decompressobj()
            delta = d.decompress(data[pos:])
            pos += len(data[pos:]) - len(d.unused_data)
            # A ref-delta base may appear later in the pack, so defer
            # resolution until every object has been read; keep a
            # placeholder so later entries keep their indexes.
            deferred.append((len(objs), base_sha, delta))
            objs.append((None, None))
        else:
            raise ValueError("unknown pack type %d" % otype)
    # Resolve deferred ref-deltas now that all pack objects are known.
    for idx, base_sha, delta in deferred:
        base_type, base_data = None, None
        for t, raw in objs:
            if t is not None and git_hash(t, raw) == base_sha:
                base_type, base_data = t, raw
                break
        if base_data is None:
            raise ValueError("ref-delta base %s not in pack" % base_sha)
        raw = apply_delta(base_data, delta)
        objs[idx] = (base_type, raw)
    # trailing sha1 checksum (20 bytes) ignored
    return objs


def reachable_objects(repo, tip):
    """All object shas reachable from a commit tip (for have/ack logic)."""
    seen = set()
    stack = [tip]
    while stack:
        cur = stack.pop()
        if not cur or cur in seen:
            continue
        obj = get_object(repo, cur)
        if obj is None:
            continue
        seen.add(cur)
        if obj[0] == "commit":
            tree, parents, _, _ = parse_commit(obj[1])
            stack.append(tree)
            stack.extend(parents)
        elif obj[0] == "tree":
            for _m, _n, sha in parse_tree(obj[1]):
                stack.append(sha)
    return seen


def reachable_tree_objects(repo, tree_sha):
    """A tree plus every subtree and blob under it (no commits). Used
    for a shallow boundary: the client owns this commit's own tree and
    blobs, but none of its ancestors."""
    seen = set()
    stack = [tree_sha]
    while stack:
        cur = stack.pop()
        if not cur or cur in seen:
            continue
        obj = get_object(repo, cur)
        if obj is None or obj[0] != "tree":
            continue
        seen.add(cur)
        for mode, _n, sha in parse_tree(obj[1]):
            if mode == "40000":
                stack.append(sha)
            else:
                seen.add(sha)
    return seen


# ------------------------------------------------------------ git smart HTTP

UPLOAD_V0_CAPS = ("multi_ack thin-pack side-band side-band-64k ofs-delta "
                  "shallow deepen-since deepen-not deepen-relative no-progress "
                  "include-tag multi_ack_detailed no-done object-format=sha1 "
                  "agent=forge/1.0")
RECEIVE_V0_CAPS = ("report-status delete-refs side-band-64k quiet atomic "
                   "ofs-delta agent=forge/1.0")
V2_CAPS = ["ls-refs=unborn", "fetch=shallow wait-for-done",
           "server-option", "object-format=sha1", "agent=forge/1.0"]


def git_advertise_v0(repo, service):
    caps = UPLOAD_V0_CAPS if service == "git-upload-pack" else RECEIVE_V0_CAPS
    out = pkt_line("# service=%s\n" % service) + FLUSH
    head_sha = repo.refs.get(repo.head_ref, "")
    # The first line carries the capability list; a symref entry tells
    # the client what HEAD points at.
    caps = "symref=HEAD:%s %s" % (repo.head_ref, caps)
    refs = sorted(repo.refs.items())
    first = True
    lines = []
    if head_sha:
        lines.append((head_sha, "HEAD"))
    for ref, sha in refs:
        if sha:
            lines.append((sha, ref))
    if not lines:
        # empty repo: advertise the capabilities on a zero-id HEAD line
        out += pkt_line(ZERO_SHA + " capabilities^{}\x00" + caps + "\n")
        out += FLUSH
        return out
    for sha, ref in lines:
        if first:
            out += pkt_line("%s %s\x00%s\n" % (sha, ref, caps))
            first = False
        else:
            out += pkt_line("%s %s\n" % (sha, ref))
        if ref.startswith("refs/tags/"):
            obj = get_object(repo, sha)
            if obj is not None and obj[0] == "tag":
                target, _ = parse_tag(obj[1])
                if target:
                    out += pkt_line("%s %s^{}\n" % (target, ref))
    out += FLUSH
    return out


def git_advertise_v2():
    out = pkt_line("version 2\n")
    for cap in V2_CAPS:
        out += pkt_line(cap + "\n")
    out += FLUSH
    return out


def handle_upload_pack_v0(repo, body):
    # The whole request is one pkt-line stream; "done" comes AFTER the
    # flush that ends the want section, so every line must be read, not
    # just the ones before the first flush.
    lines, _ = pkt_lines_all(body)
    wants = []
    haves = set()
    old_shallow = set()
    caps = ""
    deepen = None
    done = False
    for ln in lines:
        if ln in ("flush", "delim"):
            continue
        text = ln.decode("utf-8", "replace").strip()
        if "\x00" in text:
            text, caps = text.split("\x00", 1)
        if text.startswith("want "):
            wants.append(text[5:].split(" ")[0])
        elif text.startswith("have "):
            haves.add(text[5:].split(" ")[0])
        elif text.startswith("shallow "):
            old_shallow.add(text[8:].split(" ")[0])
        elif text.startswith("deepen "):
            try:
                deepen = int(text[7:])
            except ValueError:
                deepen = None
        elif text == "done":
            done = True
    if wants:
        # Remember the negotiation: over stateless HTTP the client
        # sends "done" in a follow-up request that carries no wants.
        repo.last_upload = (list(dict.fromkeys(wants)), deepen,
                            set(old_shallow))
    if not done:
        # Negotiation round: with a deepen request the server answers
        # the shallow list (terminated by a flush) and nothing else;
        # otherwise it answers NAK and waits for more haves or done.
        if deepen is not None and wants:
            _pack, boundaries, _un = build_pack_filtered(
                repo, wants, haves, deepen, old_shallow)
            out = bytearray()
            for w in sorted(boundaries):
                out += pkt_line("shallow %s\n" % w)
            out += FLUSH
            return bytes(out)
        return pkt_line("NAK\n")
    # Final request (carries "done"): shallow list, then NAK, then pack.
    if not wants:
        remembered = getattr(repo, "last_upload", None)
        if remembered is not None:
            wants, deepen, old_shallow = remembered
    pack, boundaries, _un = build_pack_filtered(repo, wants, haves, deepen,
                                                old_shallow)
    out = bytearray()
    if boundaries:
        for w in sorted(boundaries):
            out += pkt_line("shallow %s\n" % w)
        out += FLUSH
    out += pkt_line("NAK\n")
    out += sideband_pack(pack)
    out += FLUSH
    return bytes(out)


def build_pack_filtered(repo, wants, haves, deepen=None, old_shallow=(),
                        deepen_relative=False):
    """Build a packfile for a fetch, and report the shallow bookkeeping.

    Returns (pack_bytes, boundaries, unshallowed):
    - pack: every object reachable from the wants, minus objects the
      client already has. A tag want contributes the tag object itself
      plus its target commit. A have that is one of the client's old
      shallow boundaries subtracts only that commit's own objects
      (commit + tree + blobs) — the client owns the boundary but none
      of its ancestors; any other have subtracts its full closure.
    - boundaries: the new shallow-boundary commits when deepen is set
      (depth N = wants plus N-1 generations of parents; the boundary is
      the deepest included generation). With deepen-relative the depth
      is added to the client's current depth, measured from the wants
      to the old shallow boundaries.
    - unshallowed: the client's old shallow boundaries whose parents are
      now included, so they stop being shallow.
    """
    have_objs = set()
    for h in haves:
        if h in old_shallow:
            obj = get_object(repo, h)
            if obj is not None and obj[0] == "commit":
                have_objs.add(h)
                tree, _, _, _ = parse_commit(obj[1])
                have_objs |= reachable_tree_objects(repo, tree)
        else:
            have_objs |= reachable_objects(repo, h)
    # BFS from the wants, tracking generation distance. Tag wants resolve
    # to their target commit at the same distance, and the tag object
    # itself is packed.
    tags = set()
    dist = {}
    queue = []
    for w in dict.fromkeys(wants):
        queue.append((w, 0))
    qi = 0
    while qi < len(queue):
        cur, d = queue[qi]
        qi += 1
        if not cur or cur in dist:
            continue
        obj = get_object(repo, cur)
        if obj is None:
            continue
        if obj[0] == "tag":
            tags.add(cur)
            target, _ = parse_tag(obj[1])
            if target:
                queue.append((target, d))
            continue
        if obj[0] != "commit":
            continue
        dist[cur] = d
        _, parents, _, _ = parse_commit(obj[1])
        for p in parents:
            queue.append((p, d + 1))
    # Depth limit: with deepen-relative the requested depth is added to
    # the client's current depth (distance from the wants to its old
    # shallow boundaries, plus one level for the boundary itself).
    eff_deepen = deepen
    if deepen is not None and deepen_relative:
        cur_depth = 1
        for b in old_shallow:
            if b in dist:
                cur_depth = max(cur_depth, dist[b] + 1)
        eff_deepen = deepen + cur_depth
    if eff_deepen is not None:
        dist = {c: d for c, d in dist.items() if d < eff_deepen}
        boundaries = {c for c, d in dist.items() if d == eff_deepen - 1}
    else:
        boundaries = set()
    unshallowed = {s for s in old_shallow
                   if s in dist and s not in boundaries}
    commits = set(dist)
    trees, blobs = set(), set()
    stack = []
    for c in commits:
        obj = get_object(repo, c)
        tree, _, _, _ = parse_commit(obj[1])
        stack.append(tree)
    while stack:
        t = stack.pop()
        if not t or t in trees:
            continue
        obj = get_object(repo, t)
        if obj is None or obj[0] != "tree":
            continue
        trees.add(t)
        for mode, _n, sha in parse_tree(obj[1]):
            if mode == "40000":
                stack.append(sha)
            else:
                blobs.add(sha)
    ordered = []
    for sha in sorted(tags):
        if sha not in have_objs:
            ordered.append(("tag", sha))
    for sha in sorted(commits):
        if sha not in have_objs:
            ordered.append(("commit", sha))
    for sha in sorted(trees):
        if sha not in have_objs:
            ordered.append(("tree", sha))
    for sha in sorted(blobs):
        if sha not in have_objs:
            ordered.append(("blob", sha))
    out = bytearray()
    out += b"PACK" + struct.pack(">II", 2, len(ordered))
    for otype, sha in ordered:
        data = get_object(repo, sha)[1]
        tnum = {"commit": 1, "tree": 2, "blob": 3, "tag": 4}[otype]
        size = len(data)
        hdr = bytearray()
        b0 = (tnum << 4) | (size & 0x0f)
        size >>= 4
        if size:
            b0 |= 0x80
        hdr.append(b0)
        while size:
            b = size & 0x7f
            size >>= 7
            if size:
                b |= 0x80
            hdr.append(b)
        out += bytes(hdr) + zlib.compress(data)
    out += hashlib.sha1(bytes(out)).digest()
    return bytes(out), boundaries, unshallowed


def handle_receive_pack_v0(repo, body):
    lines, rest = pkt_lines(body)
    cmds = []   # (old, new, ref)
    caps = ""
    for ln in lines:
        if ln in ("flush", "delim"):
            break
        text = ln.decode("utf-8", "replace").strip()
        if "\x00" in text:
            text, caps = text.split("\x00", 1)
        parts = text.split(" ")
        if len(parts) >= 3:
            cmds.append((parts[0], parts[1], parts[2]))
    use_sideband = "side-band-64k" in caps or "side-band" in caps
    # A delete-only command list carries no pack at all; the rest of the
    # body is empty, and that is a valid zero-object pack.
    if rest:
        try:
            objs = parse_pack(rest)
        except Exception as exc:
            return report_status([], [("unpack", str(exc))], use_sideband,
                                 ok=False)
    else:
        objs = []
    with STORE.lock:
        for otype, data in objs:
            store_object(repo, otype, data)
        results = []  # (ref, ok, reason)
        for old, new, ref in cmds:
            if new == ZERO_SHA:
                # delete
                if ref in repo.refs:
                    del repo.refs[ref]
                results.append((ref, True, ""))
                continue
            obj = get_object(repo, new)
            if obj is None:
                results.append((ref, False, "object not found"))
                continue
            if ref.startswith("refs/tags/"):
                # Tag refs point at a tag object (annotated) or a commit
                # (lightweight); tags are not fast-forward checked.
                if obj[0] not in ("tag", "commit"):
                    results.append((ref, False, "object not found"))
                    continue
                repo.refs[ref] = new
                results.append((ref, True, ""))
                continue
            if obj[0] != "commit":
                results.append((ref, False, "object not found"))
                continue
            if old == ZERO_SHA:
                repo.refs[ref] = new
                results.append((ref, True, ""))
            elif is_ancestor(repo, old, new):
                repo.refs[ref] = new
                results.append((ref, True, ""))
            else:
                results.append((ref, False, "non-fast-forward"))
    return report_status(results, [], use_sideband, ok=True)


def report_status(results, errors, use_sideband, ok=True):
    if not ok:
        body = pkt_line("unpack %s\n" % (errors[0][1] if errors else "error"))
        body += FLUSH
        return _sideband_wrap(body) if use_sideband else body
    body = pkt_line("unpack ok\n")
    for ref, good, reason in results:
        if good:
            body += pkt_line("ok %s\n" % ref)
        else:
            body += pkt_line("ng %s %s\n" % (ref, reason))
    body += FLUSH
    if use_sideband:
        return _sideband_wrap(body)
    return body


def _sideband_wrap(body):
    out = bytearray()
    for i in range(0, len(body), 65515):
        chunk = b"\x01" + body[i:i + 65515]
        out += pkt_line(chunk)
    out += FLUSH
    return bytes(out)


def handle_upload_pack_v2(repo, body):
    """Protocol v2: one command per request (ls-refs or fetch)."""
    lines, _ = pkt_lines(body)
    cmd = None
    args = []
    in_args = False
    for ln in lines:
        if ln == "flush":
            break
        if ln == "delim":
            in_args = True
            continue
        text = ln.decode("utf-8", "replace").strip()
        if not in_args and text.startswith("command="):
            cmd = text[len("command="):]
        elif in_args:
            args.append(text)
    if cmd == "ls-refs":
        out = bytearray()
        unborn = "unborn" in args
        peel = "peel" in args
        symrefs = "symrefs" in args
        prefixes = [a[len("ref-prefix "):] for a in args
                    if a.startswith("ref-prefix ")]
        head_sha = repo.refs.get(repo.head_ref, "")

        def prefix_ok(refname):
            return not prefixes or any(refname.startswith(p)
                                       for p in prefixes)

        if head_sha and prefix_ok("HEAD"):
            line = "%s HEAD" % head_sha
            if symrefs:
                line += " symref-target:%s" % repo.head_ref
            out += pkt_line(line + "\n")
        for ref in sorted(repo.refs):
            sha = repo.refs[ref]
            if not sha or not prefix_ok(ref):
                continue
            line = "%s %s" % (sha, ref)
            if peel and ref.startswith("refs/tags/"):
                obj = get_object(repo, sha)
                if obj is not None and obj[0] == "tag":
                    target, _ = parse_tag(obj[1])
                    if target:
                        line += " peeled:%s" % target
            out += pkt_line(line + "\n")
        if unborn and not head_sha and (prefix_ok("HEAD") or
                                        prefix_ok(repo.head_ref)):
            # The wire form for an unborn HEAD names the ref "HEAD"
            # (with the branch as its symref target), so no refs/
            # entry appears for an empty repository.
            out += pkt_line("unborn HEAD symref-target:%s\n" %
                            repo.head_ref)
        out += FLUSH
        return bytes(out)
    if cmd == "fetch":
        wants, haves, deepen = [], set(), None
        old_shallow = set()
        deepen_relative = False
        done = False
        for a in args:
            if a.startswith("want "):
                wants.append(a[5:].split(" ")[0])
            elif a.startswith("have "):
                haves.add(a[5:].split(" ")[0])
            elif a.startswith("deepen "):
                try:
                    deepen = int(a[7:])
                except ValueError:
                    deepen = None
            elif a == "deepen-relative":
                deepen_relative = True
            elif a == "done":
                done = True
            elif a.startswith("shallow "):
                old_shallow.add(a[8:].split(" ")[0])
        pack, boundaries, unshallowed = build_pack_filtered(
            repo, wants, haves, deepen, old_shallow, deepen_relative)
        out = bytearray()
        # v2 fetch response grammar: [acknowledgments delim-pkt]
        # [shallow-info delim-pkt] [wanted-refs delim-pkt] packfile.
        # The acknowledgments section is present only when the client
        # sent haves without "done" (a client that sent "done" has
        # finished negotiating, so the section MUST be omitted). Its
        # lines are "ACK <oid>" for each common have and then a bare
        # "ready" line; without "ready" no further section may follow.
        if haves and not done:
            # A want may be a tag object; resolve it to the commit it
            # points at before testing whether a have is common.
            want_commits = set()
            for w in wants:
                cur = w
                for _ in range(8):
                    obj = get_object(repo, cur)
                    if obj is not None and obj[0] == "tag":
                        target, _ = parse_tag(obj[1])
                        if not target:
                            break
                        cur = target
                    else:
                        break
                want_commits.add(cur)
            common = []
            for h in sorted(haves):
                if get_object(repo, h) is None:
                    continue
                if any(h == w or is_ancestor(repo, h, w)
                       for w in want_commits):
                    common.append(h)
            out += pkt_line("acknowledgments\n")
            if common:
                for h in common:
                    out += pkt_line("ACK %s\n" % h)
                out += pkt_line("ready\n")
            else:
                out += pkt_line("NAK\n")
            out += DELIM
            if not common:
                # No common have: negotiation is not over and no pack
                # may follow; the client sends more haves next round.
                return bytes(out)
        if boundaries or unshallowed:
            # The shallow-info section tells the client which commits
            # are shallow boundaries and which old boundaries are now
            # complete.
            out += pkt_line("shallow-info\n")
            for w in sorted(boundaries):
                out += pkt_line("shallow %s\n" % w)
            for w in sorted(unshallowed):
                out += pkt_line("unshallow %s\n" % w)
            out += DELIM
        out += pkt_line("packfile\n")
        # Protocol v2 fetch: the packfile follows as sideband-framed
        # pkt-lines (channel 1), terminated by a flush — that is what a
        # real git client reads after the "packfile" line, whether or not
        # it sent "sideband-all".
        out += sideband_pack(pack)
        out += FLUSH
        return out
    # unknown v2 command
    return pkt_line("version 2\n") + FLUSH


def git_lookup(owner, name):
    with STORE.lock:
        return STORE.repos.get((owner, name))


# ---------------------------------------------------------------- REST v3

class ApiError(Exception):
    def __init__(self, status, message):
        super().__init__(message)
        self.status = status
        self.message = message


def repo_rest(repo):
    return {
        "id": repo.dbid,
        "node_id": repo.gid(),
        "name": repo.name,
        "full_name": repo.full_name(),
        "owner": {"login": repo.owner, "id": VIEWER["id"]},
        "private": repo.private,
        "description": repo.description,
        "default_branch": repo.default_branch,
        "html_url": "",
        "url": "",
        "created_at": now_iso(),
        "updated_at": now_iso(),
        "has_issues": True,
        "has_projects": True,
        "has_wiki": True,
        "fork": False,
        "archived": False,
        "disabled": False,
        "visibility": "private" if repo.private else "public",
    }


def find_repo(owner, name):
    repo = STORE.repos.get((owner, name))
    if repo is None:
        raise ApiError(404, "Not Found")
    return repo


def require_owner(owner):
    # A repository belongs to the login the token names; the same name
    # under another owner is 404, never 403.
    if owner != VIEWER_LOGIN:
        raise ApiError(404, "Not Found")


def create_repo(name, description, private):
    if not name or not isinstance(name, str):
        raise ApiError(422, "name is required")
    with STORE.lock:
        if (VIEWER_LOGIN, name) in STORE.repos:
            raise ApiError(422, "Repository already exists")
        dbid = STORE.next_repo_dbid
        STORE.next_repo_dbid += 1
        repo = Repo(name, VIEWER_LOGIN, description, private, dbid)
        STORE.repos[(VIEWER_LOGIN, name)] = repo
        STORE.repo_order.append((VIEWER_LOGIN, name))
        return repo


def contents_commit(repo, branch, path_parts, content_bytes, message,
                    author=None):
    """Create a commit on branch setting path to content_bytes (or deleting
    when content_bytes is None). Returns (commit_sha, blob_sha_or_None)."""
    ref = "refs/heads/" + branch
    tip = repo.refs.get(ref, "")
    base_tree = None
    parents = []
    if tip:
        obj = get_object(repo, tip)
        if obj is not None and obj[0] == "commit":
            base_tree, parents, _, _ = parse_commit(obj[1])
            parents = [tip]
    blob_sha = None
    if content_bytes is not None:
        blob_sha = store_object(repo, "blob", content_bytes)
    new_tree = tree_set(repo, base_tree, path_parts, "100644", blob_sha)
    msg = message or ("Update %s" % "/".join(path_parts))
    commit_sha = make_commit(repo, new_tree, parents, msg, author)
    repo.refs[ref] = commit_sha
    return commit_sha, blob_sha


def rest_dispatch(method, path, query, body, raw_body, headers):
    """Route a REST request. Returns (status, json_obj_or_None, extra_headers)."""
    seg = [urllib.parse.unquote(s) for s in path.split("/") if s != ""]
    if seg[:2] != ["api", "v3"]:
        raise ApiError(404, "Not Found")
    rest = seg[2:]

    def need_json():
        if body is None:
            raise ApiError(400, "Invalid JSON")
        return body

    # GET /api/v3/  (root)
    if method == "GET" and rest == []:
        return 200, {"current_user_url": "/api/v3/user",
                     "rate_limit_url": "/api/v3/rate_limit"}, \
            {"X-OAuth-Scopes": "repo, read:org"}
    # GET /api/v3/meta
    if method == "GET" and rest == ["meta"]:
        return 200, {"installed_version": VERSION}, {}
    # GET /api/v3/user/repos  (list, paginated)
    if rest == ["user", "repos"]:
        if method == "GET":
            page = int(query.get("page", ["1"])[0] or 1)
            per_page = int(query.get("per_page", ["30"])[0] or 30)
            per_page = max(1, min(per_page, 100))
            with STORE.lock:
                names = list(STORE.repo_order)
                total = len(names)
                start = (page - 1) * per_page
                chunk = names[start:start + per_page]
                items = [repo_rest(STORE.repos[k]) for k in chunk]
            extra = {}
            if start + per_page < total:
                nxt = "/api/v3/user/repos?page=%d&per_page=%d" % (page + 1, per_page)
                host = headers.get("Host", "")
                if host:
                    nxt = "http://" + host + nxt
                extra["Link"] = '<%s>; rel="next"' % nxt
            return 200, items, extra
        if method == "POST":
            data = need_json()
            repo = create_repo(data.get("name"), data.get("description"),
                               data.get("private", False))
            return 201, repo_rest(repo), {}
        raise ApiError(404, "Not Found")
    # GET /api/v3/user
    if method == "GET" and rest == ["user"]:
        return 200, {"login": VIEWER_LOGIN, "id": VIEWER["id"],
                     "name": VIEWER["name"],
                     "email": VIEWER["email"]}, {}
    # /api/v3/repos/{owner}/{repo}[...]
    if len(rest) >= 3 and rest[0] == "repos":
        owner, name = rest[1], rest[2]
        tail = rest[3:]
        require_owner(owner)
        if tail == []:
            if method == "GET":
                return 200, repo_rest(find_repo(owner, name)), {}
            if method == "PATCH":
                data = need_json()
                repo = find_repo(owner, name)
                with STORE.lock:
                    if "description" in data:
                        repo.description = data["description"]
                return 200, repo_rest(repo), {}
            if method == "DELETE":
                with STORE.lock:
                    if (owner, name) not in STORE.repos:
                        raise ApiError(404, "Not Found")
                    del STORE.repos[(owner, name)]
                    STORE.repo_order.remove((owner, name))
                return 204, None, {}
            raise ApiError(404, "Not Found")
        return rest_repo_sub(method, tail, query, body, owner, name)
    raise ApiError(404, "Not Found")


def rest_repo_sub(method, tail, query, body, owner, name):
    repo = find_repo(owner, name)
    # ---- git/refs
    if tail == ["git", "refs"] and method == "POST":
        data = body or {}
        ref = data.get("ref", "")
        sha = data.get("sha", "")
        if not ref.startswith("refs/heads/") or not sha:
            raise ApiError(422, "Invalid ref")
        with STORE.lock:
            if ref in repo.refs and repo.refs[ref]:
                raise ApiError(422, "Reference already exists")
            repo.refs[ref] = sha
        return 201, {"ref": ref, "object": {"sha": sha}}, {}
    if len(tail) >= 4 and tail[:2] == ["git", "refs"] and method == "GET":
        # ref names may contain slashes: everything after git/refs is
        # the ref path (heads/<branch>, tags/<tag>, ...).
        ref = "refs/" + "/".join(tail[2:])
        sha = repo.refs.get(ref)
        if not sha:
            raise ApiError(404, "Not Found")
        return 200, {"ref": ref, "object": {"sha": sha}}, {}
    # ---- branches
    if tail == ["branches"] and method == "GET":
        out = []
        with STORE.lock:
            for ref in repo.branches():
                sha = repo.refs[ref]
                out.append({"name": ref[len("refs/heads/"):],
                            "commit": {"sha": sha},
                            "protected": False})
        return 200, out, {}
    if len(tail) >= 2 and tail[0] == "branches" and method == "GET":
        branch = "/".join(tail[1:])
        sha = repo.branch_tip(branch)
        if not sha:
            raise ApiError(404, "Not Found")
        return 200, {"name": branch, "commit": {"sha": sha},
                     "protected": False}, {}
    # ---- commits
    if tail == ["commits"] and method == "GET":
        branch = query.get("sha", [None])[0]
        tip = repo.branch_tip(branch) if branch else \
            repo.branch_tip(repo.default_branch)
        out = []
        if tip:
            for sha in commit_history(repo, tip):
                obj = get_object(repo, sha)
                _, _, message, _ = parse_commit(obj[1])
                out.append({"sha": sha,
                            "commit": {"message": message.decode("utf-8", "replace")},
                            "author": {"login": VIEWER_LOGIN}})
        return 200, out, {}
    if len(tail) == 2 and tail[0] == "commits" and method == "GET":
        obj = get_object(repo, tail[1])
        if obj is None or obj[0] != "commit":
            raise ApiError(404, "Not Found")
        _, _, message, _ = parse_commit(obj[1])
        return 200, {"sha": tail[1],
                     "commit": {"message": message.decode("utf-8", "replace")}}, {}
    # ---- contents
    if tail and tail[0] == "contents":
        return rest_contents(method, tail[1:], query, body, repo)
    # ---- labels
    if tail == ["labels"]:
        if method == "GET":
            return 200, list(repo.labels.values()), {}
        if method == "POST":
            data = body or {}
            lname = data.get("name")
            if not lname:
                raise ApiError(422, "name is required")
            label = {"name": lname, "color": data.get("color", "ededed"),
                     "description": data.get("description", "")}
            with STORE.lock:
                repo.labels[lname] = label
            return 201, label, {}
        raise ApiError(404, "Not Found")
    # ---- issues
    if tail == ["issues"]:
        if method == "GET":
            return 200, list_issues(repo, query), {}
        if method == "POST":
            data = body or {}
            labels = data.get("labels") or []
            label_names = []
            for lb in labels:
                lname = lb if isinstance(lb, str) else lb.get("name")
                if lname not in repo.labels:
                    raise ApiError(422, "Label does not exist")
                label_names.append(lname)
            with STORE.lock:
                num = repo.next_issue
                repo.next_issue += 1
                issue = Issue(repo, num, data.get("title", ""),
                              data.get("body", ""), label_names)
                repo.issues[num] = issue
                repo.issue_order.append(num)
            return 201, issue.rest(), {}
        raise ApiError(404, "Not Found")
    if len(tail) >= 2 and tail[0] == "issues":
        try:
            num = int(tail[1])
        except ValueError:
            raise ApiError(404, "Not Found")
        issue = repo.issues.get(num)
        if issue is None:
            raise ApiError(404, "Not Found")
        if len(tail) == 2:
            if method == "GET":
                return 200, issue.rest(), {}
            if method == "PATCH":
                data = body or {}
                with STORE.lock:
                    if "state" in data:
                        issue.state = data["state"]
                    if "title" in data:
                        issue.title = data["title"]
                    if "body" in data:
                        issue.body = data["body"]
                    issue.updated_at = now_iso()
                return 200, issue.rest(), {}
            raise ApiError(404, "Not Found")
        if tail[2:] == ["comments"]:
            if method == "GET":
                return 200, list(issue.comments), {}
            if method == "POST":
                data = body or {}
                with STORE.lock:
                    cid = issue.next_comment
                    issue.next_comment += 1
                    comment = {"id": cid, "body": data.get("body", ""),
                               "user": {"login": VIEWER_LOGIN}}
                    issue.comments.append(comment)
                return 201, comment, {}
            raise ApiError(404, "Not Found")
        raise ApiError(404, "Not Found")
    # ---- pulls
    if tail == ["pulls"]:
        if method == "GET":
            state = query.get("state", ["open"])[0]
            out = []
            with STORE.lock:
                for num in repo.pull_order:
                    pr = repo.pulls[num]
                    if state == "all" or pr.state == state:
                        out.append(pr.rest())
            return 200, out, {}
        if method == "POST":
            data = body or {}
            head = data.get("head", "")
            base = data.get("base", repo.default_branch)
            head_sha = repo.branch_tip(head)
            if not head_sha:
                raise ApiError(422, "Head branch does not exist")
            base_sha = repo.branch_tip(base) or ""
            with STORE.lock:
                num = repo.next_pull
                repo.next_pull += 1
                pr = Pull(repo, num, data.get("title", ""),
                          data.get("body", ""), head, base, head_sha,
                          base_sha)
                repo.pulls[num] = pr
                repo.pull_order.append(num)
            return 201, pr.rest(), {}
        raise ApiError(404, "Not Found")
    if len(tail) >= 2 and tail[0] == "pulls":
        try:
            num = int(tail[1])
        except ValueError:
            raise ApiError(404, "Not Found")
        pr = repo.pulls.get(num)
        if len(tail) == 2:
            if pr is None:
                raise ApiError(404, "Not Found")
            if method == "GET":
                return 200, pr.rest(), {}
            raise ApiError(404, "Not Found")
        if tail[2:] == ["files"] and method == "GET":
            if pr is None:
                raise ApiError(404, "Not Found")
            return 200, pull_files(repo, pr), {}
        if tail[2:] == ["merge"] and method == "PUT":
            if pr is None:
                raise ApiError(404, "Not Found")
            return merge_pull(repo, pr, body or {})
        raise ApiError(404, "Not Found")
    raise ApiError(404, "Not Found")


def list_issues(repo, query):
    state = query.get("state", ["open"])[0]
    sort = query.get("sort", ["created"])[0]
    direction = query.get("direction", ["desc"])[0]
    with STORE.lock:
        items = [repo.issues[n] for n in repo.issue_order]
    if state in ("open", "closed"):
        items = [i for i in items if i.state == state]
    items.sort(key=lambda i: (i.created_seq, i.number),
               reverse=(direction != "asc"))
    return [i.rest() for i in items]


def pull_files(repo, pr):
    base_tree = None
    head_tree = None
    if pr.base_sha:
        obj = get_object(repo, pr.base_sha)
        if obj is not None:
            base_tree, _, _, _ = parse_commit(obj[1])
    if pr.head_sha:
        obj = get_object(repo, pr.head_sha)
        if obj is not None:
            head_tree, _, _, _ = parse_commit(obj[1])
    changed = diff_paths(repo, base_tree, head_tree)
    out = []
    for path in sorted(changed):
        b = tree_get(repo, base_tree, path.split("/")) if base_tree else None
        h = tree_get(repo, head_tree, path.split("/")) if head_tree else None
        if b is None and h is not None:
            status, additions = "added", 1
        elif b is not None and h is None:
            status, additions = "removed", 0
        else:
            status, additions = "modified", 1
        out.append({"filename": path, "status": status,
                    "additions": additions, "deletions": 0,
                    "changes": additions})
    return out


def merge_pull(repo, pr, data):
    if pr.state != "open":
        raise ApiError(405, "Pull request is not open")
    method = (data.get("merge_method") or "squash").lower()
    with STORE.lock:
        base_tip = repo.branch_tip(pr.base)
        head_tip = repo.branch_tip(pr.head)
        if not head_tip or not base_tip:
            raise ApiError(405, "Cannot merge")
        base_tree = head_tree = None
        obj = get_object(repo, base_tip)
        if obj is not None:
            base_tree, _, _, _ = parse_commit(obj[1])
        obj = get_object(repo, head_tip)
        if obj is not None:
            head_tree, _, _, _ = parse_commit(obj[1])
        pr_base_tree = None
        if pr.base_sha:
            obj = get_object(repo, pr.base_sha)
            if obj is not None:
                pr_base_tree, _, _, _ = parse_commit(obj[1])
        # conflict: a path changed on both sides since the PR base
        head_changed = diff_paths(repo, pr_base_tree, head_tree)
        base_changed = diff_paths(repo, pr_base_tree, base_tree)
        if head_changed & base_changed:
            raise ApiError(405, "Merge conflict")
        # squash: new tree = base tree with head's versions of changed paths
        new_tree = base_tree
        for path in head_changed:
            entry = tree_get(repo, head_tree, path.split("/"))
            if entry is None:
                new_tree = tree_set(repo, new_tree, path.split("/"),
                                    "100644", None)
            else:
                new_tree = tree_set(repo, new_tree, path.split("/"),
                                    entry[0], entry[1])
        msg = "Squash merge #%d: %s" % (pr.number, pr.title)
        squash_sha = make_commit(repo, new_tree, [base_tip], msg)
        repo.refs["refs/heads/" + pr.base] = squash_sha
        pr.state = "closed"
        pr.merged = True
        pr.merge_commit_sha = squash_sha
        pr.merged_at = now_iso()
        pr.updated_at = now_iso()
    return 200, {"sha": squash_sha, "merged": True,
                 "message": "Pull Request successfully merged"}, {}


def rest_contents(method, parts, query, body, repo):
    if not parts:
        raise ApiError(404, "Not Found")
    path = "/".join(parts)
    ref = query.get("ref", [repo.default_branch])[0]
    if method == "GET":
        tip = repo.branch_tip(ref)
        if not tip:
            raise ApiError(404, "Not Found")
        obj = get_object(repo, tip)
        tree, _, _, _ = parse_commit(obj[1])
        entry = tree_get(repo, tree, parts)
        if entry is None:
            raise ApiError(404, "Not Found")
        mode, sha = entry
        if mode == "40000":
            out = []
            tobj = get_object(repo, sha)
            for m, name, s in parse_tree(tobj[1]):
                out.append({"type": "dir" if m == "40000" else "file",
                            "name": name.decode("utf-8", "replace"),
                            "path": path + "/" + name.decode("utf-8", "replace"),
                            "sha": s, "size": 0})
            return 200, out, {}
        bobj = get_object(repo, sha)
        content = bobj[1] if bobj else b""
        return 200, {"type": "file", "encoding": "base64",
                     "content": base64.b64encode(content).decode(),
                     "sha": sha, "size": len(content), "name": parts[-1],
                     "path": path}, {}
    if method == "PUT":
        data = body or {}
        branch = data.get("branch", repo.default_branch)
        try:
            content = base64.b64decode(re.sub(r"\s", "", data.get("content", "")))
        except Exception:
            raise ApiError(422, "Invalid base64 content")
        tip = repo.branch_tip(branch)
        cur = None
        if tip:
            obj = get_object(repo, tip)
            tree, _, _, _ = parse_commit(obj[1])
            cur = tree_get(repo, tree, parts)
        existed = cur is not None
        if existed:
            if "sha" in data and data["sha"] and \
                    data["sha"].lower() != cur[1].lower():
                raise ApiError(409, "sha does not match")
        elif "sha" in data and data["sha"]:
            raise ApiError(409, "sha does not match")
        with STORE.lock:
            commit_sha, blob_sha = contents_commit(
                repo, branch, parts, content, data.get("message", ""))
        return (200 if existed else 201), \
            {"content": {"sha": blob_sha, "encoding": "base64",
                         "name": parts[-1], "path": path},
             "commit": {"sha": commit_sha}}, {}
    if method == "DELETE":
        data = body or {}
        branch = data.get("branch", repo.default_branch)
        tip = repo.branch_tip(branch)
        if not tip:
            raise ApiError(404, "Not Found")
        obj = get_object(repo, tip)
        tree, _, _, _ = parse_commit(obj[1])
        cur = tree_get(repo, tree, parts)
        if cur is None:
            raise ApiError(404, "Not Found")
        if "sha" in data and data["sha"] and \
                data["sha"].lower() != cur[1].lower():
            raise ApiError(409, "sha does not match")
        with STORE.lock:
            commit_sha, _ = contents_commit(repo, branch, parts, None,
                                            data.get("message", ""))
        return 200, {"commit": {"sha": commit_sha}}, {}
    raise ApiError(404, "Not Found")


# ------------------------------------------------------------- GraphQL v4

class GqlError(Exception):
    pass


_TOKEN_RE = re.compile(r"""
    \s*(?:
    (?P<name>[A-Za-z_][A-Za-z0-9_]*)
    | (?P<int>-?[0-9]+)
    | (?P<float>-?[0-9]+\.[0-9]+(?:[eE][+-]?[0-9]+)?)
    | (?P<string>"(?:\\.|[^"\\])*")
    | (?P<punct>\.\.\.|[{}\(\)\:\$\!\[\]\=\,\@])
    )""", re.VERBOSE)


def gql_tokenize(text):
    toks = []
    i = 0
    while i < len(text):
        if text[i] in " \t\r\n,":
            i += 1
            continue
        if text[i] == "#":
            while i < len(text) and text[i] != "\n":
                i += 1
            continue
        m = _TOKEN_RE.match(text, i)
        if not m:
            raise GqlError("Unexpected character %r" % text[i])
        i = m.end()
        kind = m.lastgroup
        val = m.group()
        if kind == "string":
            val = json.loads(val)
        toks.append((kind, val))
    toks.append(("eof", None))
    return toks


class GqlParser:
    def __init__(self, text):
        self.toks = gql_tokenize(text)
        self.i = 0

    def peek(self):
        return self.toks[self.i]

    def next(self):
        t = self.toks[self.i]
        self.i += 1
        return t

    def expect(self, kind, val=None):
        t = self.next()
        if t[0] != kind or (val is not None and t[1] != val):
            raise GqlError("Expected %s %s, got %s" % (kind, val, t))
        return t

    def parse(self):
        ops = []
        while self.peek()[0] != "eof":
            ops.append(self.parse_operation())
        return ops

    def parse_operation(self):
        kind, val = self.peek()
        op_type = "query"
        name = None
        if kind == "name" and val in ("query", "mutation", "subscription"):
            self.next()
            op_type = val
            if self.peek()[0] == "name":
                name = self.next()[1]
        elif kind == "punct" and val == "{":
            pass
        else:
            raise GqlError("Expected operation, got %s" % (self.peek(),))
        if self.peek() == ("punct", "("):
            self.parse_var_defs()
        sels = self.parse_selection_set()
        return {"type": op_type, "name": name, "selections": sels}

    def parse_var_defs(self):
        self.expect("punct", "(")
        depth = 1
        while depth:
            k, v = self.next()
            if k == "punct" and v == "(":
                depth += 1
            elif k == "punct" and v == ")":
                depth -= 1
            elif k == "eof":
                raise GqlError("Unterminated variable definitions")

    def parse_selection_set(self):
        self.expect("punct", "{")
        sels = []
        while True:
            k, v = self.peek()
            if k == "punct" and v == "}":
                self.next()
                return sels
            if k == "punct" and v == "...":
                self.next()
                if self.peek() == ("name", "on"):
                    self.next()
                    frag_type = self.expect("name")[1]
                else:
                    frag_type = None
                    self.expect("name")  # fragment name (unsupported)
                sels.append({"fragment": True, "on": frag_type,
                             "selections": self.parse_selection_set()})
                continue
            sels.append(self.parse_field())

    def parse_field(self):
        name = self.expect("name")[1]
        alias = None
        if self.peek() == ("punct", ":"):
            self.next()
            alias = name
            name = self.expect("name")[1]
        args = {}
        if self.peek() == ("punct", "("):
            args = self.parse_args()
        sels = None
        if self.peek() == ("punct", "{"):
            sels = self.parse_selection_set()
        return {"name": name, "alias": alias or name, "args": args,
                "selections": sels}

    def parse_args(self):
        self.expect("punct", "(")
        args = {}
        while True:
            k, v = self.peek()
            if k == "punct" and v == ")":
                self.next()
                return args
            name = self.expect("name")[1]
            self.expect("punct", ":")
            args[name] = self.parse_value()

    def parse_value(self):
        k, v = self.next()
        if k == "punct" and v == "$":
            return ("var", self.expect("name")[1])
        if k == "punct" and v == "[":
            out = []
            while not (self.peek() == ("punct", "]")):
                out.append(self.parse_value())
            self.next()
            return ("list", out)
        if k == "punct" and v == "{":
            out = {}
            while not (self.peek() == ("punct", "}")):
                name = self.expect("name")[1]
                self.expect("punct", ":")
                out[name] = self.parse_value()
            self.next()
            return ("object", out)
        if k == "string":
            return ("lit", v)
        if k == "int":
            return ("lit", int(v))
        if k == "float":
            return ("lit", float(v))
        if k == "name":
            if v == "true":
                return ("lit", True)
            if v == "false":
                return ("lit", False)
            if v == "null":
                return ("lit", None)
            return ("enum", v)
        raise GqlError("Bad value %s" % (v,))


def resolve_value(val, variables):
    kind, v = val
    if kind == "var":
        return variables.get(v)
    if kind == "list":
        return [resolve_value(x, variables) for x in v]
    if kind == "object":
        return {k: resolve_value(x, variables) for k, x in v.items()}
    return v


# Schema field sets, for unknown-field detection.
GQL_FIELDS = {
    "Query": {"viewer", "repository", "node", "__type"},
    "Mutation": {"createIssue", "closeIssue", "createPullRequest",
                 "mergePullRequest", "addComment"},
    "Repository": {"id", "databaseId", "name", "nameWithOwner", "owner",
                   "defaultBranchRef", "viewerPermission",
                   "hasIssuesEnabled", "issue", "issues", "pullRequests",
                   "pullRequest"},
    "Issue": {"id", "number", "title", "state", "body"},
    "PullRequest": {"id", "number", "headRefName", "baseRefName",
                    "mergeable", "state", "merged", "files"},
    "User": {"login", "id", "name"},
    "BranchRef": {"name"},
    "IssueConnection": {"nodes", "pageInfo", "totalCount"},
    "PullRequestConnection": {"nodes", "pageInfo", "totalCount"},
    "FileConnection": {"nodes", "pageInfo"},
    "PageInfo": {"hasNextPage", "endCursor", "hasPreviousPage",
                 "startCursor"},
    "ChangedFile": {"path"},
    "Comment": {"id", "body"},
    "IssueEdge": {"node"},
    "CommentEdge": {"node"},
    "CreateIssuePayload": {"issue"},
    "CloseIssuePayload": {"issue"},
    "CreatePullRequestPayload": {"pullRequest"},
    "MergePullRequestPayload": {"pullRequest"},
    "AddCommentPayload": {"commentEdge"},
    "__Type": {"fields", "enumValues", "name"},
    "__Field": {"name"},
    "__EnumValue": {"name"},
}

GQL_TYPE_FIELDS = {
    "Issue": ["id", "number", "title", "state", "body"],
    "PullRequest": ["id", "number", "headRefName", "baseRefName",
                    "mergeable", "state", "merged", "files"],
    "Repository": ["id", "databaseId", "name", "nameWithOwner", "owner",
                   "defaultBranchRef", "viewerPermission",
                   "hasIssuesEnabled", "issue", "issues", "pullRequests",
                   "pullRequest"],
    "SearchType": None,  # enum
}
GQL_ENUM_VALUES = {"SearchType": ["ISSUE", "PULL_REQUEST", "REPOSITORY",
                                  "USER", "COMMIT"]}


def gql_cursor(number):
    return base64.urlsafe_b64encode(("cursor:%d" % number).encode()).decode()


def gql_cursor_num(cursor):
    try:
        raw = base64.urlsafe_b64decode(cursor.encode()).decode()
        if raw.startswith("cursor:"):
            return int(raw[len("cursor:"):])
    except Exception:
        return None
    return None


def gql_issue_state(issue):
    return issue.state.upper()


def gql_pr_state(pr):
    if pr.merged:
        return "MERGED"
    return pr.state.upper()


def gql_pr_mergeable(repo, pr):
    if pr.merged or pr.state != "open":
        return "UNKNOWN"
    base_tip = repo.branch_tip(pr.base)
    head_tip = repo.branch_tip(pr.head)
    if not base_tip or not head_tip:
        return "UNKNOWN"
    base_tree = head_tree = pr_base_tree = None
    for sha, slot in ((base_tip, "base"), (head_tip, "head"),
                      (pr.base_sha, "prbase")):
        if not sha:
            continue
        obj = get_object(repo, sha)
        if obj is None:
            continue
        tree, _, _, _ = parse_commit(obj[1])
        if slot == "base":
            base_tree = tree
        elif slot == "head":
            head_tree = tree
        else:
            pr_base_tree = tree
    if diff_paths(repo, pr_base_tree, head_tree) & \
            diff_paths(repo, pr_base_tree, base_tree):
        return "CONFLICTING"
    return "MERGEABLE"


class GqlExecutor:
    def __init__(self, variables):
        self.vars = variables or {}
        self.errors = []

    def args(self, sel):
        return {k: resolve_value(v, self.vars)
                for k, v in sel["args"].items()}

    def execute(self, op):
        root_type = "Query" if op["type"] != "mutation" else "Mutation"
        try:
            data = self.exec_selections(root_type, None, op["selections"])
        except GqlError as exc:
            self.errors.append({"message": str(exc)})
            return {"data": None, "errors": self.errors}
        out = {"data": data}
        if self.errors:
            out["errors"] = self.errors
        return out

    def exec_selections(self, type_name, obj, selections):
        allowed = GQL_FIELDS.get(type_name, set())
        result = {}
        for sel in selections:
            if sel.get("fragment"):
                on = sel.get("on")
                if on is not None and on != type_name and \
                        not (type_name == "Query" and on == "Repository"):
                    # inline fragment on a different type: skip unless the
                    # object actually is that type (node() results)
                    if obj is None or obj.get("__typename") != on:
                        continue
                sub = self.exec_selections(on or type_name, obj,
                                           sel["selections"])
                result.update(sub)
                continue
            name = sel["name"]
            if name not in allowed:
                raise GqlError(
                    'Cannot query field "%s" on type "%s"' % (name, type_name))
            result[sel["alias"]] = self.resolve(type_name, obj, sel)
        return result

    def resolve(self, type_name, obj, sel):
        name = sel["name"]
        args = self.args(sel)
        subs = sel["selections"]
        if type_name == "Query":
            if name == "viewer":
                return self.exec_selections("User", {"login": VIEWER_LOGIN},
                                            subs or [])
            if name == "repository":
                repo = STORE.repos.get((args.get("owner"), args.get("name")))
                if repo is None:
                    return None
                return self.exec_selections("Repository", repo, subs or [])
            if name == "node":
                gid = args.get("id", "")
                if isinstance(gid, str) and gid.startswith("R"):
                    try:
                        dbid = int(gid[1:])
                    except ValueError:
                        return None
                    for repo in STORE.repos.values():
                        if repo.dbid == dbid:
                            return self.exec_selections(
                                "Repository", repo, subs or [])
                return None
            if name == "__type":
                return self.introspect(args.get("name"), subs or [])
        if type_name == "Mutation":
            return self.mutation(name, args, subs or [])
        if type_name == "Repository":
            return self.repo_field(obj, name, args, subs or [])
        if type_name == "Issue":
            if name == "state":
                return gql_issue_state(obj)
            if name == "id":
                return obj.gid
            return {"number": obj.number, "title": obj.title,
                    "body": obj.body}.get(name)
        if type_name == "PullRequest":
            return self.pr_field(obj, name, args, subs or [])
        if type_name == "User":
            return {"login": VIEWER_LOGIN}.get(name)
        if type_name == "BranchRef":
            return obj.get("name") if name == "name" else None
        if type_name in ("IssueConnection", "PullRequestConnection",
                         "FileConnection"):
            if name == "nodes":
                nodes = obj.get("nodes", [])
                tname = {"IssueConnection": "Issue",
                         "PullRequestConnection": "PullRequest",
                         "FileConnection": "ChangedFile"}[type_name]
                return [self.exec_selections(tname, n, subs or [])
                        for n in nodes]
            if name == "pageInfo":
                return self.exec_selections("PageInfo", obj.get("pageInfo", {}),
                                            subs or [])
            if name == "totalCount":
                return len(obj.get("nodes", []))
        if type_name == "PageInfo":
            return obj.get(name)
        if type_name == "ChangedFile":
            return obj.get("path") if name == "path" else None
        if type_name == "Comment":
            return obj.get(name)
        if type_name in ("CreateIssuePayload", "CloseIssuePayload"):
            issue = obj["issue"]
            return self.exec_selections("Issue", issue, subs or [])
        if type_name in ("CreatePullRequestPayload",
                         "MergePullRequestPayload"):
            pr = obj["pullRequest"]
            return self.exec_selections("PullRequest", pr, subs or [])
        if type_name == "AddCommentPayload":
            if name == "commentEdge":
                return self.exec_selections(
                    "CommentEdge", {"node": obj["comment"]}, subs or [])
        if type_name == "CommentEdge":
            if name == "node":
                return self.exec_selections("Comment", obj["node"], subs or [])
        if type_name == "__Type":
            if name == "fields":
                return [{"name": f} for f in (obj.get("fields") or [])]
            if name == "enumValues":
                return [{"name": v} for v in (obj.get("enumValues") or [])]
            if name == "name":
                return obj.get("name")
        return None

    def introspect(self, type_name, subs):
        fields = GQL_TYPE_FIELDS.get(type_name)
        enum_values = GQL_ENUM_VALUES.get(type_name)
        obj = {"name": type_name,
               "fields": fields,
               "enumValues": enum_values}
        return self.exec_selections("__Type", obj, subs)

    def repo_field(self, repo, name, args, subs):
        if name == "id":
            return repo.gid()
        if name == "databaseId":
            return repo.dbid
        if name == "name":
            return repo.name
        if name == "nameWithOwner":
            return repo.full_name()
        if name == "owner":
            return self.exec_selections("User", {"login": repo.owner}, subs)
        if name == "defaultBranchRef":
            return self.exec_selections(
                "BranchRef", {"name": repo.default_branch}, subs)
        if name == "viewerPermission":
            return "ADMIN"
        if name == "hasIssuesEnabled":
            return True
        if name == "issue":
            issue = repo.issues.get(args.get("number"))
            if issue is None:
                return None
            return self.exec_selections("Issue", issue, subs)
        if name == "issues":
            return self.issues_conn(repo, args, subs)
        if name == "pullRequests":
            return self.prs_conn(repo, args, subs)
        if name == "pullRequest":
            pr = repo.pulls.get(args.get("number"))
            if pr is None:
                return None
            return self.exec_selections("PullRequest", pr, subs)
        return None

    def issues_conn(self, repo, args, subs):
        with STORE.lock:
            items = [repo.issues[n] for n in repo.issue_order]
        states = args.get("states")
        if states:
            want = {str(s).upper() for s in states}
            items = [i for i in items if gql_issue_state(i) in want]
        order = args.get("orderBy") or {}
        field = (order.get("field") or "CREATED_AT").upper()
        direction = (order.get("direction") or "DESC").upper()
        reverse = direction != "ASC"
        items.sort(key=lambda i: (i.created_seq, i.number), reverse=reverse)
        first = args.get("first")
        after = args.get("after")
        if after:
            num = gql_cursor_num(after)
            if num is not None:
                items = [i for i in items
                         if (i.number < num if reverse else i.number > num)]
        total = len(items)
        page = items[:first] if first is not None else items
        has_next = len(items) > len(page)
        end_cursor = gql_cursor(page[-1].number) if page else None
        return self.exec_selections("IssueConnection", {
            "nodes": page,
            "pageInfo": {"hasNextPage": has_next, "endCursor": end_cursor,
                         "hasPreviousPage": False, "startCursor": None},
        }, subs)

    def prs_conn(self, repo, args, subs):
        with STORE.lock:
            items = [repo.pulls[n] for n in repo.pull_order]
        first = args.get("first")
        page = items[:first] if first is not None else items
        return self.exec_selections("PullRequestConnection", {
            "nodes": page,
            "pageInfo": {"hasNextPage": len(items) > len(page),
                         "endCursor": None, "hasPreviousPage": False,
                         "startCursor": None},
        }, subs)

    def pr_field(self, pr, name, args, subs):
        if name == "id":
            return pr.gid
        if name == "number":
            return pr.number
        if name == "headRefName":
            return pr.head
        if name == "baseRefName":
            return pr.base
        if name == "mergeable":
            return gql_pr_mergeable(pr.repo, pr)
        if name == "state":
            return gql_pr_state(pr)
        if name == "merged":
            return pr.merged
        if name == "files":
            files = pull_files(pr.repo, pr)
            first = args.get("first")
            if first is not None:
                files = files[:first]
            return self.exec_selections("FileConnection", {
                "nodes": files,
                "pageInfo": {"hasNextPage": False, "endCursor": None,
                             "hasPreviousPage": False, "startCursor": None},
            }, subs)
        return None

    def mutation(self, name, args, subs):
        inp = args.get("input") or {}
        if name == "createIssue":
            repo = self.repo_by_gid(inp.get("repositoryId"))
            if repo is None:
                raise GqlError("Repository not found")
            with STORE.lock:
                num = repo.next_issue
                repo.next_issue += 1
                issue = Issue(repo, num, inp.get("title", ""),
                              inp.get("body", ""), [])
                repo.issues[num] = issue
                repo.issue_order.append(num)
            return self.exec_selections("CreateIssuePayload",
                                        {"issue": issue}, subs)
        if name == "closeIssue":
            issue = self.issue_by_gid(inp.get("issueId"))
            if issue is None:
                raise GqlError("Issue not found")
            with STORE.lock:
                issue.state = "closed"
                issue.updated_at = now_iso()
            return self.exec_selections("CloseIssuePayload",
                                        {"issue": issue}, subs)
        if name == "createPullRequest":
            repo = self.repo_by_gid(inp.get("repositoryId"))
            if repo is None:
                raise GqlError("Repository not found")
            head = inp.get("headRefName", "")
            base = inp.get("baseRefName", repo.default_branch)
            head_sha = repo.branch_tip(head)
            if not head_sha:
                raise GqlError("Head branch does not exist")
            with STORE.lock:
                num = repo.next_pull
                repo.next_pull += 1
                pr = Pull(repo, num, inp.get("title", ""),
                          inp.get("body", ""), head, base, head_sha,
                          repo.branch_tip(base) or "")
                repo.pulls[num] = pr
                repo.pull_order.append(num)
            return self.exec_selections("CreatePullRequestPayload",
                                        {"pullRequest": pr}, subs)
        if name == "mergePullRequest":
            pr = self.pr_by_gid(inp.get("pullRequestId"))
            if pr is None:
                raise GqlError("Pull request not found")
            _status, payload, _h = merge_pull(pr.repo, pr, {
                "merge_method": (inp.get("mergeMethod") or "SQUASH").lower()})
            return self.exec_selections("MergePullRequestPayload",
                                        {"pullRequest": pr}, subs)
        if name == "addComment":
            issue = self.issue_by_gid(inp.get("subjectId"))
            if issue is None:
                raise GqlError("Issue not found")
            with STORE.lock:
                cid = issue.next_comment
                issue.next_comment += 1
                comment = {"id": "C%d" % cid, "body": inp.get("body", ""),
                           "user": {"login": VIEWER_LOGIN}}
                issue.comments.append(
                    {"id": cid, "body": inp.get("body", ""),
                     "user": {"login": VIEWER_LOGIN}})
                issue_gid = STORE.gid("C")
                comment["id"] = issue_gid
            return self.exec_selections("AddCommentPayload",
                                        {"comment": comment}, subs)
        raise GqlError('Cannot query field "%s" on type "Mutation"' % name)

    def repo_by_gid(self, gid):
        if not isinstance(gid, str) or not gid.startswith("R"):
            return None
        try:
            dbid = int(gid[1:])
        except ValueError:
            return None
        for repo in STORE.repos.values():
            if repo.dbid == dbid:
                return repo
        return None

    def issue_by_gid(self, gid):
        if not isinstance(gid, str) or not gid.startswith("I"):
            return None
        for repo in STORE.repos.values():
            for issue in repo.issues.values():
                if issue.gid == gid:
                    return issue
        return None

    def pr_by_gid(self, gid):
        if not isinstance(gid, str) or not gid.startswith("P"):
            return None
        for repo in STORE.repos.values():
            for pr in repo.pulls.values():
                if pr.gid == gid:
                    return pr
        return None


def handle_graphql(doc):
    """doc: parsed JSON body (dict or list). Returns (status, payload)."""
    if isinstance(doc, list):
        out = []
        for item in doc:
            out.append(run_gql(item))
        return 200, out
    return 200, run_gql(doc)


def run_gql(item):
    query = item.get("query", "") if isinstance(item, dict) else ""
    variables = item.get("variables") or {}
    try:
        ops = GqlParser(query).parse()
    except GqlError as exc:
        return {"data": None, "errors": [{"message": str(exc)}]}
    if not ops:
        return {"data": None, "errors": [{"message": "Empty query"}]}
    ex = GqlExecutor(variables)
    if len(ops) == 1:
        return ex.execute(ops[0])
    # multiple operations in one document: execute in order, one result each
    results = []
    for op in ops:
        results.append(GqlExecutor(variables).execute(op))
    return results


# ------------------------------------------------------------------- MCP

MCP_VERSIONS = ["2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26",
                "2024-11-05"]

MCP_TOOLS = [
    {"name": "create_repo",
     "description": "Create a new repository owned by the authenticated user.",
     "inputSchema": {"type": "object",
                     "properties": {
                         "name": {"type": "string",
                                  "description": "Repository name"},
                         "description": {"type": "string",
                                         "description": "Repository description"},
                         "private": {"type": "boolean",
                                     "description": "Whether the repository is private"},
                         "full_name": {"type": "string",
                                       "description": "owner/repo"},
                     },
                     "required": ["name"]}},
    {"name": "get_repo",
     "description": "Get a repository by owner and name.",
     "inputSchema": {"type": "object",
                     "properties": {
                         "owner": {"type": "string",
                                   "description": "Repository owner"},
                         "repo": {"type": "string",
                                  "description": "Repository name"},
                         "full_name": {"type": "string",
                                       "description": "owner/repo"},
                     },
                     "required": ["owner", "repo"]}},
    {"name": "create_issue",
     "description": "Create an issue in a repository.",
     "inputSchema": {"type": "object",
                     "properties": {
                         "owner": {"type": "string",
                                   "description": "Repository owner"},
                         "repo": {"type": "string",
                                  "description": "Repository name"},
                         "full_name": {"type": "string",
                                       "description": "owner/repo"},
                         "title": {"type": "string",
                                   "description": "Issue title"},
                         "body": {"type": "string",
                                  "description": "Issue body"},
                     },
                     "required": ["owner", "repo", "title"]}},
    {"name": "list_issues",
     "description": "List issues in a repository, optionally filtered by state.",
     "inputSchema": {"type": "object",
                     "properties": {
                         "owner": {"type": "string",
                                   "description": "Repository owner"},
                         "repo": {"type": "string",
                                  "description": "Repository name"},
                         "full_name": {"type": "string",
                                       "description": "owner/repo"},
                         "state": {"type": "string",
                                   "description": "Issue state: open, closed or all"},
                     },
                     "required": ["owner", "repo"]}},
    {"name": "create_pull_request",
     "description": "Create a pull request from a head branch onto a base branch.",
     "inputSchema": {"type": "object",
                     "properties": {
                         "owner": {"type": "string",
                                   "description": "Repository owner"},
                         "repo": {"type": "string",
                                  "description": "Repository name"},
                         "full_name": {"type": "string",
                                       "description": "owner/repo"},
                         "title": {"type": "string",
                                   "description": "Pull request title"},
                         "body": {"type": "string",
                                  "description": "Pull request body"},
                         "head": {"type": "string",
                                  "description": "Head branch name"},
                         "base": {"type": "string",
                                  "description": "Base branch name"},
                     },
                     "required": ["owner", "repo", "title", "head", "base"]}},
    {"name": "merge_pull_request",
     "description": "Merge a pull request with a squash commit.",
     "inputSchema": {"type": "object",
                     "properties": {
                         "owner": {"type": "string",
                                   "description": "Repository owner"},
                         "repo": {"type": "string",
                                  "description": "Repository name"},
                         "full_name": {"type": "string",
                                       "description": "owner/repo"},
                         "pull_number": {"type": "integer",
                                         "description": "Pull request number"},
                     },
                     "required": ["owner", "repo", "pull_number"]}},
]


def mcp_text_result(payload):
    return {"content": [{"type": "text",
                         "text": json.dumps(payload, sort_keys=True)}],
            "isError": False}


def mcp_split_full_name(args):
    owner = args.get("owner")
    repo = args.get("repo")
    full = args.get("full_name") or args.get("fullName") or ""
    if (not owner or not repo) and isinstance(full, str) and "/" in full:
        o, r = full.split("/", 1)
        owner = owner or o
        repo = repo or r
    return owner, repo


def mcp_call(name, args):
    """Dispatch one tools/call. Returns (result_dict, is_error)."""
    base = name.lower()
    if "__" in base:
        base = base.split("__", 1)[1]
    if base not in {t["name"] for t in MCP_TOOLS}:
        raise _McpError(-32601, "Unknown tool: %s" % name)
    args = args or {}
    if base == "create_repo":
        if not args.get("name"):
            raise _McpError(-32602, "Missing required argument: name")
        repo = create_repo(args["name"], args.get("description"),
                           args.get("private", False))
        return mcp_text_result(repo_rest(repo)), False
    owner, repo_name = mcp_split_full_name(args)
    if not owner or not repo_name:
        raise _McpError(-32602, "Missing required argument: owner/repo")
    if owner != VIEWER_LOGIN:
        raise _McpError(-32602, "Repository not found")
    repo = STORE.repos.get((owner, repo_name))
    if base == "get_repo":
        if repo is None:
            raise _McpError(-32602, "Repository not found")
        return mcp_text_result(repo_rest(repo)), False
    if repo is None:
        raise _McpError(-32602, "Repository not found")
    if base == "create_issue":
        if not args.get("title"):
            raise _McpError(-32602, "Missing required argument: title")
        with STORE.lock:
            num = repo.next_issue
            repo.next_issue += 1
            issue = Issue(repo, num, args["title"], args.get("body", ""), [])
            repo.issues[num] = issue
            repo.issue_order.append(num)
        return mcp_text_result(issue.rest()), False
    if base == "list_issues":
        state = (args.get("state") or "open").lower()
        with STORE.lock:
            items = [repo.issues[n] for n in repo.issue_order]
        if state in ("open", "closed"):
            items = [i for i in items if i.state == state]
        return mcp_text_result([i.rest() for i in items]), False
    if base == "create_pull_request":
        for req in ("title", "head", "base"):
            if not args.get(req):
                raise _McpError(-32602, "Missing required argument: %s" % req)
        head_sha = repo.branch_tip(args["head"])
        if not head_sha:
            raise _McpError(-32602, "Head branch does not exist")
        with STORE.lock:
            num = repo.next_pull
            repo.next_pull += 1
            pr = Pull(repo, num, args["title"], args.get("body", ""),
                      args["head"], args["base"], head_sha,
                      repo.branch_tip(args["base"]) or "")
            repo.pulls[num] = pr
            repo.pull_order.append(num)
        return mcp_text_result(pr.rest()), False
    if base == "merge_pull_request":
        if args.get("pull_number") is None:
            raise _McpError(-32602, "Missing required argument: pull_number")
        try:
            num = int(args["pull_number"])
        except (TypeError, ValueError):
            raise _McpError(-32602, "pull_number must be an integer")
        pr = repo.pulls.get(num)
        if pr is None:
            raise _McpError(-32602, "Pull request not found")
        _status, payload, _h = merge_pull(repo, pr, {"merge_method": "squash"})
        return mcp_text_result(payload), False
    raise _McpError(-32601, "Unknown tool: %s" % name)


class _McpError(Exception):
    def __init__(self, code, message):
        super().__init__(message)
        self.code = code
        self.message = message


def handle_mcp(doc):
    """doc: parsed JSON-RPC body (dict). Returns (status, payload, headers).
    A notification returns (202, None, {})."""
    if not isinstance(doc, dict):
        return 400, {"jsonrpc": "2.0", "id": None,
                     "error": {"code": -32700, "message": "Parse error"}}, {}
    req_id = doc.get("id")
    method = doc.get("method", "")
    params = doc.get("params") or {}
    is_notification = "id" not in doc
    if method == "notifications/initialized" or \
            method.startswith("notifications/"):
        return 202, None, {}
    if method == "initialize":
        proto = params.get("protocolVersion", "2025-11-25")
        result = {
            "protocolVersion": proto,
            "capabilities": {"tools": {}},
            "serverInfo": {"name": "forge", "version": VERSION},
        }
        return 200, {"jsonrpc": "2.0", "id": req_id, "result": result}, {}
    if method == "server/discover":
        # The modern handshake reads serverInfo and capabilities from
        # this result (serverInfo first from _meta, else top level).
        result = {
            "supportedVersions": list(MCP_VERSIONS),
            "serverInfo": {"name": "forge", "version": VERSION},
            "capabilities": {"tools": {}},
            "_meta": {"io.modelcontextprotocol/serverInfo":
                      {"name": "forge", "version": VERSION}},
        }
        return 200, {"jsonrpc": "2.0", "id": req_id, "result": result}, {}
    if method == "ping":
        return 200, {"jsonrpc": "2.0", "id": req_id, "result": {}}, {}
    if method == "tools/list":
        return 200, {"jsonrpc": "2.0", "id": req_id,
                     "result": {"tools": MCP_TOOLS}}, {}
    if method == "tools/call":
        name = params.get("name", "")
        args = params.get("arguments") or {}
        try:
            result, _is_err = mcp_call(name, args)
        except _McpError as exc:
            return 200, {"jsonrpc": "2.0", "id": req_id,
                         "error": {"code": exc.code,
                                   "message": exc.message}}, {}
        except ApiError as exc:
            return 200, {"jsonrpc": "2.0", "id": req_id,
                         "result": {"content": [{"type": "text",
                                                 "text": exc.message}],
                                    "isError": True}}, {}
        return 200, {"jsonrpc": "2.0", "id": req_id, "result": result}, {}
    if is_notification:
        return 202, None, {}
    return 200, {"jsonrpc": "2.0", "id": req_id,
                 "error": {"code": -32601,
                           "message": "Method not found: %s" % method}}, {}


# ------------------------------------------------------------- HTTP server

def check_auth(headers):
    if not TOKEN:
        return  # local smoke-test mode: no token configured
    auth = headers.get("Authorization", "")
    if auth != "Bearer " + TOKEN:
        raise ApiError(401, "Bad credentials")


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "Forge/1.0"

    def log_message(self, *args):
        pass

    def read_body(self):
        if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
            out = bytearray()
            while True:
                size_line = self.rfile.readline(65536).strip()
                try:
                    size = int(size_line.split(b";")[0], 16)
                except ValueError:
                    break
                if size == 0:
                    self.rfile.readline(65536)
                    break
                out += self.rfile.read(size)
                self.rfile.readline(65536)
            return bytes(out)
        length = self.headers.get("Content-Length")
        if length is None:
            return b""
        try:
            n = int(length)
        except ValueError:
            return b""
        return self.rfile.read(n) if n > 0 else b""

    def send_json(self, status, payload, extra=None):
        body = b"" if payload is None else \
            (json.dumps(payload) + "\n").encode()
        headers = {"Content-Type": "application/json; charset=utf-8",
                   "Content-Length": str(len(body)),
                   "X-GitHub-Request-Id": STORE.request_id()}
        if extra:
            headers.update(extra)
        self.send_response(status)
        for k, v in headers.items():
            self.send_header(k, v)
        self.end_headers()
        if body and self.command != "HEAD":
            self.wfile.write(body)

    def send_bytes(self, status, body, ctype, extra=None):
        headers = {"Content-Type": ctype,
                   "Content-Length": str(len(body)),
                   "X-GitHub-Request-Id": STORE.request_id()}
        if extra:
            headers.update(extra)
        self.send_response(status)
        for k, v in headers.items():
            self.send_header(k, v)
        self.end_headers()
        if body and self.command != "HEAD":
            self.wfile.write(body)

    def parse_json_body(self, raw):
        if not raw:
            return None
        try:
            return json.loads(raw.decode("utf-8"))
        except (ValueError, UnicodeDecodeError):
            raise ApiError(400, "Invalid JSON")

    def dispatch(self):
        try:
            parsed = urllib.parse.urlsplit(self.path)
            path = parsed.path
            query = urllib.parse.parse_qs(parsed.query)
            raw = self.read_body()
            method = self.command
            if method == "GET" and path == "/healthz":
                self.send_json(200, {"ok": True})
                return
            # ---- git smart HTTP (no auth enforcement; the suite's git
            # client speaks the protocol, the token rides the API)
            m = re.match(r"^/([^/]+)/([^/]+)\.git(/.*)$", path)
            if m:
                owner, name, rest = m.group(1), m.group(2), m.group(3)
                self.dispatch_git(method, owner, name, rest, parsed, raw)
                return
            # ---- API surfaces require the bearer token
            check_auth(self.headers)
            if path in ("/api/graphql", "/api/graphql/"):
                if method != "POST":
                    raise ApiError(404, "Not Found")
                doc = self.parse_json_body(raw)
                status, payload = handle_graphql(doc)
                self.send_json(status, payload)
                return
            if path == "/mcp":
                if method == "DELETE":
                    # session teardown (legacy close)
                    self.send_json(200, {})
                    return
                if method != "POST":
                    raise ApiError(404, "Not Found")
                if not raw:
                    self.send_json(400, {"jsonrpc": "2.0", "id": None,
                                         "error": {"code": -32700,
                                                   "message": "Parse error"}})
                    return
                try:
                    doc = json.loads(raw.decode("utf-8"))
                except (ValueError, UnicodeDecodeError):
                    # A truncated body can still carry a readable id;
                    # echo it so the caller can match the error.
                    req_id = None
                    text = raw.decode("utf-8", "replace")
                    mm = re.search(r'"id"\s*:\s*("([^"\\]|\\.)*"|-?\d+)',
                                   text)
                    if mm:
                        try:
                            req_id = json.loads(mm.group(1))
                        except ValueError:
                            req_id = None
                    self.send_json(400, {"jsonrpc": "2.0", "id": req_id,
                                         "error": {"code": -32700,
                                                   "message": "Parse error"}})
                    return
                status, payload, _h = handle_mcp(doc)
                if payload is None:
                    self.send_response(status)
                    self.send_header("Content-Length", "0")
                    self.send_header("X-GitHub-Request-Id",
                                     STORE.request_id())
                    self.end_headers()
                    return
                self.send_json(status, payload)
                return
            if path.startswith("/api/v3"):
                body = self.parse_json_body(raw)
                status, payload, extra = rest_dispatch(
                    method, path, query, body, raw, self.headers)
                self.send_json(status, payload, extra)
                return
            raise ApiError(404, "Not Found")
        except ApiError as exc:
            self.send_json(exc.status, {"message": exc.message})
        except BrokenPipeError:
            pass
        except Exception as exc:  # never leak a stack trace or HTML
            try:
                self.send_json(500, {"message": "Internal Server Error"})
            except Exception:
                pass

    def dispatch_git(self, method, owner, name, rest, parsed, raw):
        repo = git_lookup(owner, name)
        if repo is None:
            self.send_json(404, {"message": "Not Found"})
            return
        if method == "GET" and rest == "/info/refs":
            service = parsed.query and \
                urllib.parse.parse_qs(parsed.query).get("service", [None])[0]
            if service == "git-upload-pack":
                proto = self.headers.get("Git-Protocol", "")
                if "version=2" in proto:
                    self.send_bytes(
                        200, git_advertise_v2(),
                        "application/x-git-upload-pack-advertisement")
                else:
                    self.send_bytes(
                        200, git_advertise_v0(repo, "git-upload-pack"),
                        "application/x-git-upload-pack-advertisement")
                return
            if service == "git-receive-pack":
                self.send_bytes(
                    200, git_advertise_v0(repo, "git-receive-pack"),
                    "application/x-git-receive-pack-advertisement")
                return
            self.send_json(404, {"message": "Not Found"})
            return
        if method == "POST" and rest == "/git-upload-pack":
            proto = self.headers.get("Git-Protocol", "")
            with STORE.lock:
                if "version=2" in proto:
                    out = handle_upload_pack_v2(repo, raw)
                else:
                    out = handle_upload_pack_v0(repo, raw)
            self.send_bytes(200, out, "application/x-git-upload-pack-result")
            return
        if method == "POST" and rest == "/git-receive-pack":
            with STORE.lock:
                out = handle_receive_pack_v0(repo, raw)
            self.send_bytes(200, out,
                            "application/x-git-receive-pack-result")
            return
        self.send_json(404, {"message": "Not Found"})

    do_GET = dispatch
    do_POST = dispatch
    do_PUT = dispatch
    do_PATCH = dispatch
    do_DELETE = dispatch


# --------------------------------------------------------------- lifecycle

def http_request(method, url, body=None, timeout=30):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req, timeout=timeout) as res:
        return res.status, res.read()


def register(port):
    attempt = 0
    while True:
        try:
            status, _ = http_request("POST", ENV_URL + "/register",
                                     {"port": port})
            if status in (200, 409):
                return
            if status in (400, 405):
                raise RuntimeError("register answered HTTP %d" % status)
        except RuntimeError:
            raise
        except Exception:
            pass
        time.sleep(min(2.0, 0.25 * (2 ** min(attempt, 8))))
        attempt += 1


def wait_for_done():
    attempt = 0
    while True:
        try:
            status, raw = http_request("GET", ENV_URL + "/done")
            if status == 200:
                doc = json.loads(raw.decode())
                if doc.get("done") is True:
                    return
        except Exception:
            pass
        time.sleep(min(2.0, 0.25 * (2 ** min(attempt, 8))))
        attempt += 1


def main():
    port = int(os.environ.get("FORGE_PORT", "0"))
    server = ThreadingHTTPServer(("0.0.0.0", port), Handler)
    server.daemon_threads = True
    bound = server.server_address[1]
    sys.stdout.write("[entry] listening on port %d\n" % bound)
    sys.stdout.flush()
    if not ENV_URL:
        sys.stdout.write("[entry] no MAKEFUN_ENV_URL; serving locally\n")
        sys.stdout.flush()
        try:
            server.serve_forever()
        except KeyboardInterrupt:
            pass
        return
    register(bound)
    sys.stdout.write("[entry] registered; waiting for the env to say done\n")
    sys.stdout.flush()
    wait_for_done()
    server.shutdown()
    server.server_close()
    sys.stdout.write("[entry] done\n")


if __name__ == "__main__":
    main()
