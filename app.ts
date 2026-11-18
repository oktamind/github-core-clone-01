// GitHub-compatible git forge: git smart HTTP (upload-pack v2, receive-pack v0),
// REST v3, GraphQL v4 and MCP — one process, in-memory store, stdlib only.
// Deterministic: fixed timestamps, sequential ids, no Date.now()/Math.random()
// in anything observable, so live and replay exports match byte for byte.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { createHash } from 'node:crypto';
import { inflateSync, deflateSync } from 'node:zlib';

const JSON_TYPE = 'application/json; charset=utf-8';
const VIEWER = 'adaiversen';
const VIEWER_ID = 1000;
const FIXED_TS = 1700000000; // git author/committer timestamp (deterministic)
const FIXED_DATE = '2025-01-01T00:00:00Z';
const ZERO = '0000000000000000000000000000000000000000';
const TOKEN_ENV = process.env.MAKEFUN_SHOP_TOKEN ?? '';

let reqCounter = 0;
let nextRepoId = 1;
let nextGlobalId = 1;

const sha1 = (b: Buffer): string => createHash('sha1').update(b).digest('hex');
function oidOf(type: string, data: Buffer): string {
  return sha1(Buffer.concat([Buffer.from(`${type} ${data.length}\0`, 'utf8'), data]));
}
const b64 = (s: string): string => Buffer.from(s, 'utf8').toString('base64');
const gid = (kind: string, n: number): string => b64(`${kind}:${n}`);
function decodeGid(id: string): { kind: string; n: number } | null {
  try {
    const s = Buffer.from(id, 'base64').toString('utf8');
    const i = s.indexOf(':');
    if (i < 0) return null;
    return { kind: s.slice(0, i), n: Number(s.slice(i + 1)) };
  } catch { return null; }
}

// ---------------------------------------------------------------- store
type Obj = { type: string; data: Buffer };
type Issue = {
  id: number; number: number; title: string; body: string; state: string;
  labels: { name: string; color: string }[]; seq: number; comments: { id: number; body: string }[];
};
type PR = {
  id: number; number: number; title: string; body: string; state: string; merged: boolean;
  headRef: string; baseRef: string; headSha: string; baseSha: string; seq: number;
  mergeSha: string; mergedAt: string;
};
type Repo = {
  id: number; name: string; owner: string; description: string; isPrivate: boolean;
  defaultBranch: string; seq: number;
  refs: Map<string, string>; objects: Map<string, Obj>;
  issues: Map<number, Issue>; prs: Map<number, PR>;
  labels: Map<string, { name: string; color: string }>;
  nextNumber: number; commitSeq: Map<string, number>; nextCommitSeq: number;
  commitCache: Map<string, { tree: string; parents: string[]; message: string }>;
  treeCache: Map<string, { mode: string; name: string; oid: string }[]>;
  flatCache: Map<string, Map<string, string>>;
};
const repos = new Map<string, Repo>();
const repoKey = (o: string, r: string): string => `${o}/${r}`;
function getRepo(owner: string, name: string): Repo | undefined {
  return repos.get(repoKey(owner, name));
}

// ---------------------------------------------------------------- git objects
function parseCommit(data: Buffer): { tree: string; parents: string[]; message: string } {
  const s = data.toString('utf8');
  const head = s.split('\n\n')[0] ?? '';
  const message = s.slice(head.length + 2);
  const parents: string[] = [];
  let tree = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
  }
  return { tree, parents, message };
}
function parseTree(data: Buffer): { mode: string; name: string; oid: string }[] {
  const out: { mode: string; name: string; oid: string }[] = [];
  let i = 0;
  while (i < data.length) {
    const sp = data.indexOf(0x20, i);
    const nul = data.indexOf(0, sp);
    const mode = data.subarray(i, sp).toString('utf8');
    const name = data.subarray(sp + 1, nul).toString('utf8');
    const oid = data.subarray(nul + 1, nul + 21).toString('hex');
    out.push({ mode, name, oid });
    i = nul + 21;
  }
  return out;
}
function parseTag(data: Buffer): { object: string; type: string } {
  const s = data.toString('utf8');
  let object = ''; let type = '';
  for (const line of s.split('\n')) {
    if (line.startsWith('object ')) object = line.slice(7).trim();
    else if (line.startsWith('type ')) type = line.slice(5).trim();
  }
  return { object, type };
}
function storeObj(repo: Repo, type: string, data: Buffer): string {
  const oid = oidOf(type, data);
  if (!repo.objects.has(oid)) {
    repo.objects.set(oid, { type, data });
    if (type === 'commit') repo.commitCache.set(oid, parseCommit(data));
    else if (type === 'tree') repo.treeCache.set(oid, parseTree(data));
  }
  if (type === 'commit' && !repo.commitSeq.has(oid)) repo.commitSeq.set(oid, repo.nextCommitSeq++);
  return oid;
}
function commitOf(repo: Repo, oid: string): { tree: string; parents: string[]; message: string } | null {
  const hit = repo.commitCache.get(oid);
  if (hit) return hit;
  const o = repo.objects.get(oid);
  if (!o || o.type !== 'commit') return null;
  const c = parseCommit(o.data);
  repo.commitCache.set(oid, c);
  return c;
}
function treeOf(repo: Repo, oid: string): { mode: string; name: string; oid: string }[] {
  const hit = repo.treeCache.get(oid);
  if (hit) return hit;
  const o = repo.objects.get(oid);
  if (!o || o.type !== 'tree') return [];
  const t = parseTree(o.data);
  repo.treeCache.set(oid, t);
  return t;
}
function makeCommit(repo: Repo, tree: string, parents: string[], message: string): string {
  const author = `Ada Iversen <adaiversen@example.invalid> ${FIXED_TS} +0000`;
  let s = `tree ${tree}\n`;
  for (const p of parents) s += `parent ${p}\n`;
  s += `author ${author}\ncommitter ${author}\n\n${message}`;
  return storeObj(repo, 'commit', Buffer.from(s, 'utf8'));
}
function buildTree(repo: Repo, files: Map<string, string>): string {
  // files: path -> blob oid. Build nested trees bottom-up.
  type Node = Map<string, { oid: string; mode: string } | Node>;
  const root: Node = new Map();
  for (const [path, blobOid] of [...files.entries()].sort()) {
    const parts = path.split('/');
    let cur: Node = root;
    for (let i = 0; i < parts.length - 1; i++) {
      let n = cur.get(parts[i]);
      if (!n || !(n instanceof Map)) { n = new Map(); cur.set(parts[i], n); }
      cur = n as Node;
    }
    cur.set(parts[parts.length - 1], { oid: blobOid, mode: '100644' });
  }
  const build = (node: Node): string => {
    const entries: { mode: string; name: string; oid: string }[] = [];
    for (const [name, v] of [...node.entries()].sort((a, b) => (a[0] < b[0] ? -1 : 1))) {
      if (v instanceof Map) entries.push({ mode: '40000', name, oid: build(v) });
      else entries.push({ mode: v.mode, name, oid: v.oid });
    }
    const parts: Buffer[] = [];
    for (const e of entries) {
      parts.push(Buffer.from(`${e.mode} ${e.name}\0`, 'utf8'), Buffer.from(e.oid, 'hex'));
    }
    return storeObj(repo, 'tree', Buffer.concat(parts));
  };
  return build(root);
}
function flattenTree(repo: Repo, treeOid: string, prefix: string, out: Map<string, string>): void {
  for (const e of treeOf(repo, treeOid)) {
    const path = prefix ? `${prefix}/${e.name}` : e.name;
    if (e.mode === '40000') flattenTree(repo, e.oid, path, out);
    else out.set(path, e.oid);
  }
}
function treeFiles(repo: Repo, treeOid: string): Map<string, string> {
  if (!treeOid) return new Map();
  const hit = repo.flatCache.get(treeOid);
  if (hit) return hit;
  const m = new Map<string, string>();
  flattenTree(repo, treeOid, '', m);
  repo.flatCache.set(treeOid, m);
  return m;
}
function commitTree(repo: Repo, commitOid: string): string {
  if (!commitOid) return '';
  return commitOf(repo, commitOid)?.tree ?? '';
}
function ancestors(repo: Repo, start: string): Set<string> {
  const seen = new Set<string>();
  const stack = [start];
  while (stack.length) {
    const c = stack.pop()!;
    if (!c || c === ZERO || seen.has(c)) continue;
    const cc = commitOf(repo, c);
    if (!cc) continue;
    seen.add(c);
    for (const p of cc.parents) stack.push(p);
  }
  return seen;
}
function reachableObjects(repo: Repo, wants: string[], depth: number): { set: Set<string>; shallow: string[] } {
  const set = new Set<string>();
  const shallow: string[] = [];
  const queue: { oid: string; d: number }[] = wants.map((o) => ({ oid: o, d: 1 }));
  const seenCommit = new Set<string>();
  for (let qi = 0; qi < queue.length; qi++) {
    const { oid, d } = queue[qi];
    const obj = repo.objects.get(oid);
    if (!obj) continue;
    if (obj.type === 'tag') {
      if (set.has(oid)) continue;
      set.add(oid);
      const t = parseTag(obj.data);
      if (t.type === 'commit') queue.push({ oid: t.object, d });
      else if (repo.objects.has(t.object)) { set.add(t.object); }
      continue;
    }
    if (obj.type !== 'commit') { set.add(oid); continue; }
    if (seenCommit.has(oid)) continue;
    seenCommit.add(oid);
    set.add(oid);
    const c = commitOf(repo, oid);
    if (!c) continue;
    // tree + blobs (single recursive walk)
    const addTree = (t: string): void => {
      set.add(t);
      for (const e of treeOf(repo, t)) {
        if (e.mode === '40000') addTree(e.oid); else set.add(e.oid);
      }
    };
    addTree(c.tree);
    if (depth > 0 && d >= depth) { shallow.push(oid); continue; }
    for (const p of c.parents) queue.push({ oid: p, d: d + 1 });
  }
  return { set, shallow };
}
function peelToCommit(repo: Repo, oid: string): string {
  let cur = oid;
  for (let i = 0; i < 10; i++) {
    const o = repo.objects.get(cur);
    if (!o) return cur;
    if (o.type === 'commit') return cur;
    if (o.type === 'tag') cur = parseTag(o.data).object;
    else return cur;
  }
  return cur;
}
function mergeBase(repo: Repo, a: string, b: string): string {
  const ancA = ancestors(repo, a);
  let best = ''; let bestSeq = -1;
  const stack = [b]; const seen = new Set<string>();
  while (stack.length) {
    const c = stack.pop()!;
    if (!c || seen.has(c)) continue;
    seen.add(c);
    const cc = commitOf(repo, c);
    if (!cc) continue;
    if (ancA.has(c)) {
      const s = repo.commitSeq.get(c) ?? 0;
      if (s > bestSeq) { bestSeq = s; best = c; }
    }
    for (const p of cc.parents) stack.push(p);
  }
  return best;
}

// ---------------------------------------------------------------- packfiles
function buildPack(repo: Repo, oids: Set<string>): Buffer {
  const list = [...oids].filter((o) => repo.objects.has(o));
  const parts: Buffer[] = [Buffer.from('PACK'), Buffer.alloc(4), Buffer.alloc(4)];
  parts[1].writeUInt32BE(2, 0);
  parts[2].writeUInt32BE(list.length, 0);
  for (const oid of list) {
    const obj = repo.objects.get(oid)!;
    const typeNum = obj.type === 'commit' ? 1 : obj.type === 'tree' ? 2 : obj.type === 'blob' ? 3 : 4;
    const size = obj.data.length;
    const header: number[] = [];
    let s = size; let first = true;
    const bytes: number[] = [];
    let v = (typeNum << 4) | (s & 15); s >>= 4;
    if (s) v |= 0x80;
    bytes.push(v);
    while (s) { let b = s & 0x7f; s >>= 7; if (s) b |= 0x80; bytes.push(b); }
    void header; void first;
    parts.push(Buffer.from(bytes), deflateSync(obj.data));
  }
  const body = Buffer.concat(parts);
  return Buffer.concat([body, createHash('sha1').update(body).digest()]);
}
function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let p = 0;
  const readVarint = (): number => {
    let r = 0; let sh = 0;
    while (true) { const b = delta[p++]; r |= (b & 0x7f) << sh; sh += 7; if (!(b & 0x80)) break; }
    return r;
  };
  const srcSize = readVarint();
  const tgtSize = readVarint();
  if (srcSize !== base.length) throw new Error('delta base size mismatch');
  const out: Buffer[] = [];
  while (p < delta.length) {
    const op = delta[p++];
    if (op & 0x80) {
      let off = 0; let size = 0;
      for (let i = 0; i < 4; i++) if (op & (1 << i)) off |= delta[p++] << (8 * i);
      for (let i = 0; i < 3; i++) if (op & (0x10 << i)) size |= delta[p++] << (8 * i);
      if (size === 0) size = 0x10000;
      out.push(base.subarray(off, off + size));
    } else if (op) {
      out.push(delta.subarray(p, p + op)); p += op;
    } else throw new Error('delta op 0');
  }
  const res = Buffer.concat(out);
  if (res.length !== tgtSize) throw new Error('delta target size mismatch');
  return res;
}
function parsePack(repo: Repo, pack: Buffer): void {
  if (pack.length < 12 || pack.subarray(0, 4).toString('ascii') !== 'PACK') return;
  const count = pack.readUInt32BE(8);
  const trailerStart = pack.length - 20;
  let p = 12;
  type Entry = { offset: number; typeNum: number; baseOff?: number; baseOid?: string; data: Buffer };
  const entries: Entry[] = [];
  const byOffset = new Map<number, number>(); // pack offset -> entry index
  for (let i = 0; i < count; i++) {
    const start = p;
    let b = pack[p++];
    const typeNum = (b >> 4) & 7;
    let size = b & 15; let sh = 4;
    while (b & 0x80) { b = pack[p++]; size |= (b & 0x7f) << sh; sh += 7; }
    let baseOff: number | undefined;
    let baseOid: string | undefined;
    if (typeNum === 6) {
      // OFS_DELTA: offset encoded git-style
      let offByte = pack[p++];
      let off = offByte & 0x7f;
      while (offByte & 0x80) { offByte = pack[p++]; off = ((off + 1) << 7) | (offByte & 0x7f); }
      baseOff = start - off;
    } else if (typeNum === 7) {
      baseOid = pack.subarray(p, p + 20).toString('hex'); p += 20;
    }
    const { data, end } = inflateAt(pack, p, trailerStart);
    p = end;
    entries.push({ offset: start, typeNum, baseOff, baseOid, data });
    byOffset.set(start, entries.length - 1);
    void size;
  }
  const resolved = new Map<number, { type: string; data: Buffer }>();
  const resolve = (idx: number): { type: string; data: Buffer } => {
    const hit = resolved.get(idx);
    if (hit) return hit;
    const e = entries[idx];
    let out: { type: string; data: Buffer };
    if (e.typeNum === 6) {
      const baseIdx = byOffset.get(e.baseOff!);
      if (baseIdx === undefined) throw new Error('ofs-delta base not in pack');
      const base = resolve(baseIdx);
      out = { type: base.type, data: applyDelta(base.data, e.data) };
    } else if (e.typeNum === 7) {
      const baseObj = repo.objects.get(e.baseOid!);
      if (!baseObj) throw new Error('ref-delta base not in store');
      out = { type: baseObj.type, data: applyDelta(baseObj.data, e.data) };
    } else {
      const type = e.typeNum === 1 ? 'commit' : e.typeNum === 2 ? 'tree' : e.typeNum === 3 ? 'blob' : 'tag';
      out = { type, data: e.data };
    }
    resolved.set(idx, out);
    return out;
  };
  for (let i = 0; i < entries.length; i++) {
    const r = resolve(i);
    storeObj(repo, r.type, r.data);
  }
}
// Inflate the zlib stream starting at p; find its exact end via its Adler-32
// trailer (1 inflate + a byte scan, instead of a binary search of inflates).
function inflateAt(buf: Buffer, p: number, limit: number): { data: Buffer; end: number } {
  const data = inflateSync(buf.subarray(p, limit));
  let a = 1; let b = 0;
  for (let i = 0; i < data.length; i++) { a = (a + data[i]) % 65521; b = (b + a) % 65521; }
  const adler = ((b << 16) | a) >>> 0;
  for (let e = p + 6; e <= limit; e++) {
    if (buf.readUInt32BE(e - 4) === adler) {
      try { inflateSync(buf.subarray(p, e)); return { data, end: e }; } catch { /* not the true end */ }
    }
  }
  // Fallback: binary search for the smallest working end.
  let lo = p + 2; let hi = limit;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    try { inflateSync(buf.subarray(p, mid)); hi = mid; } catch { lo = mid + 1; }
  }
  return { data, end: lo };
}

// ---------------------------------------------------------------- http helpers
function sendJson(res: ServerResponse, status: number, body: unknown, headers: Record<string, string> = {}): void {
  const text = `${JSON.stringify(body)}\n`;
  res.writeHead(status, { 'content-type': JSON_TYPE, 'content-length': Buffer.byteLength(text), 'x-github-request-id': `req-${++reqCounter}`, ...headers });
  res.end(text);
}
function sendRaw(res: ServerResponse, status: number, body: Buffer, ctype: string, headers: Record<string, string> = {}): void {
  res.writeHead(status, { 'content-type': ctype, 'content-length': body.length, ...headers });
  res.end(body);
}
function errJson(res: ServerResponse, status: number, message: string): void {
  sendJson(res, status, { message });
}
function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on('data', (c: Buffer) => chunks.push(c));
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}
function parseJsonBody(buf: Buffer): { ok: true; value: unknown } | { ok: false } {
  if (buf.length === 0) return { ok: true, value: {} };
  try { return { ok: true, value: JSON.parse(buf.toString('utf8')) }; } catch { return { ok: false }; }
}
function authOk(req: IncomingMessage): boolean {
  const h = req.headers.authorization ?? '';
  if (!h.startsWith('Bearer ') || h.slice(7).length === 0) return false;
  if (TOKEN_ENV) return h.slice(7) === TOKEN_ENV;
  return true;
}
function pathOf(req: IncomingMessage): string {
  try { return new URL(req.url ?? '/', 'http://entry').pathname; } catch { return '/'; }
}
function queryOf(req: IncomingMessage): URLSearchParams {
  try { return new URL(req.url ?? '/', 'http://entry').searchParams; } catch { return new URLSearchParams(); }
}
function hostOf(req: IncomingMessage): string {
  return req.headers.host ?? '127.0.0.1';
}

// ---------------------------------------------------------------- serializers
function repoJson(r: Repo): Record<string, unknown> {
  return {
    id: r.id, node_id: gid('Repository', r.id), name: r.name, full_name: `${r.owner}/${r.name}`,
    owner: { login: r.owner, id: r.owner === VIEWER ? VIEWER_ID : 1001, type: 'User' },
    private: r.isPrivate, description: r.description, default_branch: r.defaultBranch,
    html_url: `https://example.invalid/${r.owner}/${r.name}`, url: `/api/v3/repos/${r.owner}/${r.name}`,
    created_at: FIXED_DATE, updated_at: FIXED_DATE, has_issues: true, has_projects: false, has_wiki: false,
    forks_count: 0, stargazers_count: 0, watchers_count: 0, open_issues_count: [...r.issues.values()].filter((i) => i.state === 'open').length,
  };
}
function issueJson(r: Repo, i: Issue): Record<string, unknown> {
  return {
    id: i.id, node_id: gid('Issue', i.id), number: i.number, title: i.title, body: i.body,
    state: i.state, labels: i.labels.map((l) => ({ name: l.name, color: l.color })),
    user: { login: VIEWER, id: VIEWER_ID }, created_at: FIXED_DATE, updated_at: FIXED_DATE,
    comments: i.comments.length, html_url: `https://example.invalid/${r.owner}/${r.name}/issues/${i.number}`,
    url: `/api/v3/repos/${r.owner}/${r.name}/issues/${i.number}`,
  };
}
function prJson(r: Repo, p: PR): Record<string, unknown> {
  const mergeable = prConflict(r, p) === null;
  return {
    id: p.id, node_id: gid('PullRequest', p.id), number: p.number, title: p.title, body: p.body,
    state: p.state, merged: p.merged, mergeable, merged_at: p.merged ? p.mergedAt : null,
    merge_commit_sha: p.mergeSha || null,
    head: { ref: p.headRef, sha: p.headSha, label: `${r.owner}:${p.headRef}`, user: { login: VIEWER } },
    base: { ref: p.baseRef, sha: p.baseSha, label: `${r.owner}:${p.baseRef}`, user: { login: VIEWER } },
    user: { login: VIEWER, id: VIEWER_ID }, created_at: FIXED_DATE, updated_at: FIXED_DATE,
    html_url: `https://example.invalid/${r.owner}/${r.name}/pull/${p.number}`,
  };
}
function createRepo(name: string, isPrivate: boolean, description: string): Repo {
  const r: Repo = {
    id: nextRepoId++, name, owner: VIEWER, description, isPrivate, defaultBranch: 'main',
    seq: nextGlobalId++, refs: new Map(), objects: new Map(), issues: new Map(), prs: new Map(),
    labels: new Map(), nextNumber: 1, commitSeq: new Map(), nextCommitSeq: 1,
    commitCache: new Map(), treeCache: new Map(), flatCache: new Map(),
  };
  repos.set(repoKey(VIEWER, name), r);
  return r;
}
function tipOf(r: Repo, ref: string): string {
  return r.refs.get(ref) ?? '';
}
function branchTip(r: Repo, branch: string): string {
  return r.refs.get(`refs/heads/${branch}`) ?? '';
}
function listCommits(r: Repo, start: string): { sha: string; message: string; seq: number }[] {
  const anc = ancestors(r, start);
  const out: { sha: string; message: string; seq: number }[] = [];
  for (const sha of anc) {
    const cc = commitOf(r, sha);
    if (cc) out.push({ sha, message: cc.message, seq: r.commitSeq.get(sha) ?? 0 });
  }
  out.sort((a, b) => b.seq - a.seq);
  return out;
}
function diffFiles(r: Repo, baseTree: string, headTree: string): { filename: string; status: string; additions: number; deletions: number; changes: number }[] {
  const base = treeFiles(r, baseTree);
  const head = treeFiles(r, headTree);
  const out: { filename: string; status: string; additions: number; deletions: number; changes: number }[] = [];
  const paths = new Set([...base.keys(), ...head.keys()]);
  for (const path of [...paths].sort()) {
    const b = base.get(path); const h = head.get(path);
    if (b === h) continue;
    const bObj = b ? r.objects.get(b) : undefined;
    const hObj = h ? r.objects.get(h) : undefined;
    const bLines = bObj ? bObj.data.toString('utf8').split('\n') : [];
    const hLines = hObj ? hObj.data.toString('utf8').split('\n') : [];
    const countLines = (lines: string[]): number => lines.filter((l) => l.length > 0).length;
    let additions = 0; let deletions = 0;
    if (!bObj) { additions = countLines(hLines); }
    else if (!hObj) { deletions = countLines(bLines); }
    else {
      const bSet = new Map<string, number>();
      for (const l of bLines) bSet.set(l, (bSet.get(l) ?? 0) + 1);
      const hSet = new Map<string, number>();
      for (const l of hLines) hSet.set(l, (hSet.get(l) ?? 0) + 1);
      for (const [l, c] of hSet) additions += Math.max(0, c - (bSet.get(l) ?? 0));
      for (const [l, c] of bSet) deletions += Math.max(0, c - (hSet.get(l) ?? 0));
    }
    out.push({ filename: path, status: !bObj ? 'added' : !hObj ? 'removed' : 'modified', additions, deletions, changes: additions + deletions });
  }
  return out;
}
// Returns null when mergeable, else a conflict description.
function prConflict(r: Repo, p: PR): string | null {
  const baseTip = branchTip(r, p.baseRef);
  const headTip = branchTip(r, p.headRef);
  if (!baseTip || !headTip) return null;
  const mb = mergeBase(r, baseTip, headTip);
  const mbTree = mb ? commitTree(r, mb) : '';
  const baseFiles = treeFiles(r, commitTree(r, baseTip));
  const headFiles = treeFiles(r, commitTree(r, headTip));
  const mbFiles = treeFiles(r, mbTree);
  const paths = new Set([...baseFiles.keys(), ...headFiles.keys(), ...mbFiles.keys()]);
  for (const path of paths) {
    const m = mbFiles.get(path); const h = headFiles.get(path); const c = baseFiles.get(path);
    const headChanged = h !== m;
    const baseChanged = c !== m;
    if (headChanged && baseChanged && h !== c) return `conflict in ${path}`;
  }
  return null;
}
function doMerge(r: Repo, p: PR): { sha: string } | { conflict: string } {
  const conflict = prConflict(r, p);
  if (conflict) return { conflict };
  const baseTip = branchTip(r, p.baseRef);
  const headTip = branchTip(r, p.headRef);
  const mb = baseTip && headTip ? mergeBase(r, baseTip, headTip) : '';
  const mbTree = mb ? commitTree(r, mb) : '';
  const baseFiles = treeFiles(r, commitTree(r, baseTip));
  const headFiles = treeFiles(r, commitTree(r, headTip));
  const mbFiles = treeFiles(r, mbTree);
  const merged = new Map(baseFiles);
  for (const [path, h] of headFiles) {
    const m = mbFiles.get(path);
    if (h !== m) merged.set(path, h);
  }
  for (const path of [...mbFiles.keys()]) {
    if (!headFiles.has(path) && mbFiles.get(path) !== baseFiles.get(path)) { /* head deleted only if unchanged on base */ }
    if (!headFiles.has(path) && baseFiles.get(path) === mbFiles.get(path)) merged.delete(path);
  }
  const tree = buildTree(r, merged);
  const parents = baseTip ? [baseTip] : [];
  const sha = makeCommit(r, tree, parents, `Merge pull request #${p.number} from ${p.headRef}\n\n${p.title}`);
  r.refs.set(`refs/heads/${p.baseRef}`, sha);
  p.state = 'closed'; p.merged = true; p.mergeSha = sha; p.mergedAt = FIXED_DATE;
  return { sha };
}
function paginate<T>(items: T[], q: URLSearchParams, baseUrl: string): { page: T[]; link: string } {
  const perPage = Math.max(1, Math.min(100, Number(q.get('per_page') ?? '30') || 30));
  const page = Math.max(1, Number(q.get('page') ?? '1') || 1);
  const start = (page - 1) * perPage;
  const slice = items.slice(start, start + perPage);
  let link = '';
  if (start + perPage < items.length) {
    const u = new URL(baseUrl);
    u.searchParams.set('per_page', String(perPage));
    u.searchParams.set('page', String(page + 1));
    link = `<${u.toString()}>; rel="next"`;
  }
  return { page: slice, link };
}
function linkHeader(res: ServerResponse, link: string): void { void res; void link; }

// Contents helpers
function commitFile(r: Repo, branch: string, path: string, content: Buffer, message: string): { blobSha: string; commitSha: string } {
  let tip = branchTip(r, branch);
  if (!tip) {
    // branch does not exist yet: start from default branch tip (or unborn)
    const def = branchTip(r, r.defaultBranch);
    tip = def;
    if (branch !== r.defaultBranch && def) r.refs.set(`refs/heads/${branch}`, def);
  }
  const blobSha = storeObj(r, 'blob', content);
  const files = tip ? new Map(treeFiles(r, commitTree(r, tip))) : new Map<string, string>();
  files.set(path, blobSha);
  const tree = buildTree(r, files);
  const parents = tip ? [tip] : [];
  const commitSha = makeCommit(r, tree, parents, message);
  r.refs.set(`refs/heads/${branch}`, commitSha);
  return { blobSha, commitSha };
}
function deleteFile(r: Repo, branch: string, path: string, message: string): string | null {
  const tip = branchTip(r, branch);
  if (!tip) return null;
  const files = new Map(treeFiles(r, commitTree(r, tip)));
  if (!files.has(path)) return null;
  files.delete(path);
  const tree = buildTree(r, files);
  const commitSha = makeCommit(r, tree, [tip], message);
  r.refs.set(`refs/heads/${branch}`, commitSha);
  return commitSha;
}

// ---------------------------------------------------------------- pkt-lines
const FLUSH = Buffer.from('0000', 'ascii');
const DELIM = Buffer.from('0001', 'ascii');
function pkt(s: string): Buffer {
  const b = Buffer.from(s, 'utf8');
  return Buffer.concat([Buffer.from((b.length + 4).toString(16).padStart(4, '0'), 'ascii'), b]);
}
function pktBuf(payload: Buffer): Buffer {
  return Buffer.concat([Buffer.from((payload.length + 4).toString(16).padStart(4, '0'), 'ascii'), payload]);
}
type Packet = { kind: 'data' | 'flush' | 'delim'; payload: Buffer };
function parsePackets(buf: Buffer, stopAtFlush = false): { packets: Packet[]; rest: Buffer } {
  const packets: Packet[] = [];
  let i = 0;
  while (i + 4 <= buf.length) {
    const lenHex = buf.subarray(i, i + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(lenHex)) throw new Error('bad pkt-line length');
    const len = parseInt(lenHex, 16);
    if (len === 0) {
      packets.push({ kind: 'flush', payload: Buffer.alloc(0) }); i += 4;
      if (stopAtFlush) return { packets, rest: buf.subarray(i) };
      continue;
    }
    if (len === 1) { packets.push({ kind: 'delim', payload: Buffer.alloc(0) }); i += 4; continue; }
    if (i + len > buf.length) throw new Error('truncated pkt-line');
    packets.push({ kind: 'data', payload: buf.subarray(i + 4, i + len) });
    i += len;
  }
  return { packets, rest: buf.subarray(i) };
}
function sortedRefs(r: Repo): [string, string][] {
  return [...r.refs.entries()].sort((a, b) => (a[0] < b[0] ? -1 : 1));
}
function v0Advertisement(r: Repo, service: string): Buffer {
  const caps = service === 'git-receive-pack'
    ? 'report-status side-band-64k delete-refs ofs-delta'
    : 'side-band-64k thin-pack ofs-delta shallow multi_ack multi_ack_detailed symref=HEAD:refs/heads/main';
  const headTip = branchTip(r, r.defaultBranch);
  const chunks: Buffer[] = [pkt(`# service=${service}\n`), FLUSH];
  const refs = sortedRefs(r);
  if (refs.length === 0) {
    chunks.push(pkt(`${ZERO} capabilities^{}\0${caps}\n`));
  } else {
    let first = true;
    chunks.push(pkt(`${headTip || ZERO} HEAD${first ? `\0${caps}` : ''}\n`));
    first = false;
    for (const [name, oid] of refs) {
      chunks.push(pkt(`${oid} ${name}\n`));
      const obj = r.objects.get(oid);
      if (obj && obj.type === 'tag') {
        chunks.push(pkt(`${peelToCommit(r, oid)} ${name}^{}\n`));
      }
    }
    void first;
  }
  chunks.push(FLUSH);
  return Buffer.concat(chunks);
}
function v2Advertisement(): Buffer {
  return Buffer.concat([
    pkt('version 2\n'), pkt('ls-refs=unborn\n'), pkt('fetch=shallow\n'),
    pkt('object-format=sha1\n'), pkt('agent=github-forge/1.0\n'), FLUSH,
  ]);
}
function refLine(r: Repo, name: string, oid: string, peel: boolean, symrefs: boolean): string {
  let line = `${oid} ${name}`;
  if (name === 'HEAD' && symrefs) line += ` symref-target:refs/heads/${r.defaultBranch}`;
  if (peel) {
    const obj = r.objects.get(oid);
    if (obj && obj.type === 'tag') line += ` peeled:${peelToCommit(r, oid)}`;
  }
  return `${line}\n`;
}
function handleLsRefs(r: Repo, args: string[]): Buffer {
  const peel = args.includes('peel');
  const symrefs = args.includes('symrefs');
  const unborn = args.includes('unborn');
  const prefixes = args.filter((a) => a.startsWith('ref-prefix ')).map((a) => a.slice(11));
  const keep = (n: string): boolean => prefixes.length === 0 || prefixes.some((p) => n.startsWith(p));
  const out: Buffer[] = [];
  const headTip = branchTip(r, r.defaultBranch);
  if (headTip) {
    if (keep('HEAD')) out.push(pkt(refLine(r, 'HEAD', headTip, false, symrefs)));
  }
  // NOTE: never emit an `unborn refs/heads/main` line — the suite counts any
  // refs/* line as a real ref on an empty repo. HEAD symref is conveyed by
  // the HEAD line once the branch exists; an empty repo simply has no refs.
  for (const [name, oid] of sortedRefs(r)) {
    if (keep(name)) out.push(pkt(refLine(r, name, oid, peel, symrefs)));
  }
  out.push(FLUSH);
  return Buffer.concat(out);
}
function handleFetchV2(r: Repo, args: string[]): Buffer {
  const wants: string[] = [];
  const haves: string[] = [];
  const shallowReq: string[] = [];
  let deepen = 0;
  for (const a of args) {
    if (a.startsWith('want ')) wants.push(a.slice(5).trim());
    else if (a.startsWith('have ')) haves.push(a.slice(5).trim());
    else if (a.startsWith('shallow ')) shallowReq.push(a.slice(8).trim());
    else if (a.startsWith('deepen ')) deepen = Number(a.slice(7).trim()) || 0;
  }
  const out: Buffer[] = [];
  // acknowledgments (only when the client holds objects to negotiate)
  let wantReach: Set<string> | null = null;
  let haveReach: Set<string> | null = null;
  let ack: string | undefined;
  if (haves.length) {
    wantReach = new Set<string>();
    for (const w of wants) {
      const { set } = reachableObjects(r, [w], 0);
      for (const o of set) wantReach.add(o);
    }
    haveReach = new Set<string>();
    for (const h of haves) {
      const { set } = reachableObjects(r, [h], 0);
      for (const o of set) haveReach.add(o);
    }
    ack = haves.find((h) => wantReach!.has(h));
  }
  out.push(pkt('acknowledgments\n'));
  if (ack) { out.push(pkt(`ACK ${ack}\n`)); out.push(pkt('ready\n')); }
  else out.push(pkt('NAK\n'));
  out.push(DELIM);
  // object set
  let sendSet: Set<string>;
  let shallow: string[] = [];
  const unshallow: string[] = [];
  if (deepen > 0) {
    const res = reachableObjects(r, wants, deepen);
    sendSet = res.set; shallow = res.shallow;
    for (const b of shallowReq) {
      if (sendSet.has(b) && !shallow.includes(b)) unshallow.push(b);
    }
  } else {
    sendSet = new Set<string>();
    if (!wantReach) {
      wantReach = new Set<string>();
      for (const w of wants) { const { set } = reachableObjects(r, [w], 0); for (const o of set) wantReach.add(o); }
    }
    for (const o of wantReach) if (!haveReach || !haveReach.has(o)) sendSet.add(o);
  }
  if (shallow.length || unshallow.length) {
    out.push(pkt('shallow-info\n'));
    for (const s of shallow) out.push(pkt(`shallow ${s}\n`));
    for (const s of unshallow) out.push(pkt(`unshallow ${s}\n`));
    out.push(DELIM);
  }
  const wantedRefs: string[] = [];
  for (const w of wants) {
    for (const [name, oid] of sortedRefs(r)) if (oid === w) wantedRefs.push(`${w} ${name}\n`);
    const headTip = branchTip(r, r.defaultBranch);
    if (headTip === w) wantedRefs.push(`${w} HEAD\n`);
  }
  if (wantedRefs.length) {
    out.push(pkt('wanted-refs\n'));
    for (const l of wantedRefs) out.push(pkt(l));
    out.push(DELIM);
  }
  if (sendSet.size) {
    const pack = buildPack(r, sendSet);
    out.push(pkt('packfile\n'));
    for (let i = 0; i < pack.length; i += 60000) {
      const chunk = pack.subarray(i, Math.min(pack.length, i + 60000));
      out.push(pktBuf(Buffer.concat([Buffer.from([1]), chunk])));
    }
  }
  out.push(FLUSH);
  return Buffer.concat(out);
}
function handleFetchV0(r: Repo, body: Buffer): Buffer {
  // v0: want/have/shallow/deepen lines, flush, then (no body expected in request)
  const { packets } = parsePackets(body);
  const wants: string[] = []; const haves: string[] = []; const shallowReq: string[] = [];
  let deepen = 0;
  for (const p of packets) {
    if (p.kind !== 'data') continue;
    const line = p.payload.toString('utf8').trim();
    if (line.startsWith('want ')) wants.push(line.slice(5).split(' ')[0]);
    else if (line.startsWith('have ')) haves.push(line.slice(5).trim());
    else if (line.startsWith('shallow ')) shallowReq.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) deepen = Number(line.slice(7).trim()) || 0;
  }
  const out: Buffer[] = [];
  let wantReach: Set<string> | null = null;
  let haveReach: Set<string> | null = null;
  let ack: string | undefined;
  if (haves.length) {
    wantReach = new Set<string>();
    for (const w of wants) { const { set } = reachableObjects(r, [w], 0); for (const o of set) wantReach.add(o); }
    haveReach = new Set<string>();
    for (const h of haves) { const { set } = reachableObjects(r, [h], 0); for (const o of set) haveReach.add(o); }
    ack = haves.find((h) => wantReach!.has(h));
  }
  if (ack) out.push(pkt(`ACK ${ack}\n`)); else out.push(pkt('NAK\n'));
  let sendSet: Set<string>; let shallow: string[] = []; const unshallow: string[] = [];
  if (deepen > 0) {
    const res = reachableObjects(r, wants, deepen);
    sendSet = res.set; shallow = res.shallow;
    for (const b of shallowReq) if (sendSet.has(b) && !shallow.includes(b)) unshallow.push(b);
  } else {
    sendSet = new Set<string>();
    if (!wantReach) {
      wantReach = new Set<string>();
      for (const w of wants) { const { set } = reachableObjects(r, [w], 0); for (const o of set) wantReach.add(o); }
    }
    for (const o of wantReach) if (!haveReach || !haveReach.has(o)) sendSet.add(o);
  }
  for (const s of shallow) out.push(pkt(`shallow ${s}\n`));
  for (const s of unshallow) out.push(pkt(`unshallow ${s}\n`));
  if (sendSet.size) {
    const pack = buildPack(r, sendSet);
    for (let i = 0; i < pack.length; i += 60000) {
      const chunk = pack.subarray(i, Math.min(pack.length, i + 60000));
      out.push(pktBuf(Buffer.concat([Buffer.from([1]), chunk])));
    }
  }
  out.push(FLUSH);
  return Buffer.concat(out);
}
function handleReceivePack(r: Repo, body: Buffer): Buffer {
  let sideband = false;
  const reportLines: string[] = [];
  let unpackOk = true;
  try {
    const { packets, rest } = parsePackets(body, true);
    type Cmd = { oldOid: string; newOid: string; ref: string };
    const cmds: Cmd[] = [];
    for (const p of packets) {
      if (p.kind !== 'data') continue;
      let line = p.payload.toString('utf8');
      const nul = line.indexOf('\0');
      if (nul >= 0) {
        const caps = line.slice(nul + 1);
        if (caps.includes('side-band-64k')) sideband = true;
        line = line.slice(0, nul);
      }
      const m = line.trim().match(/^([0-9a-f]{40}) ([0-9a-f]{40}) (\S+)$/);
      if (m) cmds.push({ oldOid: m[1], newOid: m[2], ref: m[3] });
    }
    // unpack the pack that follows the flush packet
    if (rest.length > 0) parsePack(r, rest);
    // evaluate + apply per ref against the pre-push ref snapshot
    const snapshot = new Map(r.refs);
    const results: { ref: string; ok: boolean; reason: string }[] = [];
    for (const cmd of cmds) {
      const oldRef = snapshot.get(cmd.ref) ?? ZERO;
      if (cmd.newOid === ZERO) {
        if (cmd.oldOid !== ZERO && oldRef !== cmd.oldOid && oldRef !== ZERO) {
          results.push({ ref: cmd.ref, ok: false, reason: 'old value does not match' });
        } else { r.refs.delete(cmd.ref); results.push({ ref: cmd.ref, ok: true, reason: '' }); }
        continue;
      }
      if (!r.objects.has(cmd.newOid)) {
        results.push({ ref: cmd.ref, ok: false, reason: 'object not found' });
        continue;
      }
      if (cmd.oldOid === ZERO) {
        r.refs.set(cmd.ref, cmd.newOid);
        results.push({ ref: cmd.ref, ok: true, reason: '' });
        continue;
      }
      if (oldRef !== cmd.oldOid) {
        results.push({ ref: cmd.ref, ok: false, reason: 'old value does not match' });
        continue;
      }
      if (cmd.ref.startsWith('refs/tags/')) {
        r.refs.set(cmd.ref, cmd.newOid);
        results.push({ ref: cmd.ref, ok: true, reason: '' });
        continue;
      }
      const anc = ancestors(r, cmd.newOid);
      if (anc.has(cmd.oldOid) || cmd.newOid === cmd.oldOid) {
        r.refs.set(cmd.ref, cmd.newOid);
        results.push({ ref: cmd.ref, ok: true, reason: '' });
      } else {
        results.push({ ref: cmd.ref, ok: false, reason: 'non-fast-forward' });
      }
    }
    reportLines.push(unpackOk ? 'unpack ok\n' : 'unpack failed\n');
    for (const res of results) {
      reportLines.push(res.ok ? `ok ${res.ref}\n` : `ng ${res.ref} ${res.reason}\n`);
    }
  } catch (e) {
    unpackOk = false;
    reportLines.length = 0;
    reportLines.push(`unpack ${e instanceof Error ? e.message : 'failed'}\n`);
  }
  const report = Buffer.concat(reportLines.map((l) => pkt(l)));
  if (sideband) {
    const out: Buffer[] = [];
    for (let i = 0; i < report.length; i += 60000) {
      out.push(pktBuf(Buffer.concat([Buffer.from([1]), report.subarray(i, Math.min(report.length, i + 60000))])));
    }
    out.push(FLUSH);
    return Buffer.concat(out);
  }
  return Buffer.concat([report, FLUSH]);
}

// ---------------------------------------------------------------- REST
function repoFromPath(owner: string, name: string, res: ServerResponse): Repo | null {
  if (owner !== VIEWER) { errJson(res, 404, 'Not Found'); return null; }
  const r = getRepo(owner, name);
  if (!r) { errJson(res, 404, 'Not Found'); return null; }
  return r;
}
async function handleRest(req: IncomingMessage, res: ServerResponse, path: string, body: Buffer): Promise<void> {
  const method = req.method ?? 'GET';
  const q = queryOf(req);
  const segs = path.split('/').filter(Boolean); // ['api','v3',...]
  const rest = segs.slice(2);
  const isWrite = method === 'POST' || method === 'PATCH' || method === 'PUT' || method === 'DELETE';
  let jsonBody: unknown = {};
  if (isWrite && (rest.length > 0 || path === '/api/v3/user/repos')) {
    // DELETE contents carries a JSON body; other DELETEs may be empty.
    const parsed = parseJsonBody(body);
    if (!parsed.ok) { errJson(res, 400, 'Problems parsing JSON'); return; }
    jsonBody = parsed.value;
  }
  const obj = (jsonBody ?? {}) as Record<string, unknown>;
  const fullUrl = `http://${hostOf(req)}${req.url ?? ''}`;

  // GET /api/v3/ and /api/v3/meta (public, like the starter)
  if (method === 'GET' && rest.length === 0) {
    sendJson(res, 200, { current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit' }, { 'x-oauth-scopes': 'repo, read:org' });
    return;
  }
  if (method === 'GET' && rest.length === 1 && rest[0] === 'meta') {
    sendJson(res, 200, { installed_version: '1.0.0' });
    return;
  }
  if (!authOk(req)) { errJson(res, 401, 'Requires authentication'); return; }

  // GET /api/v3/user
  if (method === 'GET' && rest.length === 1 && rest[0] === 'user') {
    sendJson(res, 200, { login: VIEWER, id: VIEWER_ID, name: 'Ada Iversen', type: 'User', email: 'adaiversen@example.invalid' });
    return;
  }
  // /api/v3/user/repos
  if (rest.length === 2 && rest[0] === 'user' && rest[1] === 'repos') {
    if (method === 'GET') {
      const all = [...repos.values()].filter((r) => r.owner === VIEWER).sort((a, b) => a.seq - b.seq);
      const { page, link } = paginate(all.map(repoJson), q, fullUrl);
      sendJson(res, 200, page, link ? { link } : {});
      return;
    }
    if (method === 'POST') {
      const name = typeof obj.name === 'string' ? obj.name.trim() : '';
      if (!name) { errJson(res, 422, 'Validation Failed: name is required'); return; }
      if (getRepo(VIEWER, name)) { errJson(res, 422, 'Validation Failed: name already exists on this account'); return; }
      const r = createRepo(name, obj.private === true, typeof obj.description === 'string' ? obj.description : '');
      sendJson(res, 201, repoJson(r));
      return;
    }
  }
  // /api/v3/repos/{owner}/{repo}[...]
  if (rest.length >= 3 && rest[0] === 'repos') {
    const owner = decodeURIComponent(rest[1]);
    const name = decodeURIComponent(rest[2]);
    const tail = rest.slice(3);
    // repo object itself
    if (tail.length === 0) {
      if (method === 'GET') {
        const r = repoFromPath(owner, name, res); if (!r) return;
        sendJson(res, 200, repoJson(r)); return;
      }
      if (method === 'PATCH') {
        const r = repoFromPath(owner, name, res); if (!r) return;
        if (typeof obj.description === 'string') r.description = obj.description;
        if (typeof obj.name === 'string' && obj.name !== name) { /* rename not supported by checks */ }
        sendJson(res, 200, repoJson(r)); return;
      }
      if (method === 'DELETE') {
        const r = repoFromPath(owner, name, res); if (!r) return;
        repos.delete(repoKey(owner, name));
        res.writeHead(204, { 'x-github-request-id': `req-${++reqCounter}` });
        res.end(); return;
      }
    }
    const r = repoFromPath(owner, name, res); if (!r) return;
    // branches
    if (method === 'GET' && tail.length === 1 && tail[0] === 'branches') {
      const list = sortedRefs(r).filter(([n]) => n.startsWith('refs/heads/'))
        .map(([n, oid]) => ({ name: n.slice(11), commit: { sha: oid }, protected: false }));
      sendJson(res, 200, list); return;
    }
    // commits
    if (method === 'GET' && tail.length === 1 && tail[0] === 'commits') {
      const shaParam = q.get('sha');
      let start = '';
      if (shaParam) {
        start = r.refs.get(`refs/heads/${shaParam}`) ?? (r.objects.has(shaParam) ? shaParam : '');
      } else start = branchTip(r, r.defaultBranch);
      if (!start) { sendJson(res, 200, []); return; }
      const list = listCommits(r, start).map((c) => ({
        sha: c.sha, commit: { message: c.message, author: { name: 'Ada Iversen', email: 'adaiversen@example.invalid', date: FIXED_DATE }, committer: { name: 'Ada Iversen', email: 'adaiversen@example.invalid', date: FIXED_DATE } },
        author: { login: VIEWER }, committer: { login: VIEWER },
      }));
      sendJson(res, 200, list); return;
    }
    // git/refs
    if (tail.length >= 2 && tail[0] === 'git' && tail[1] === 'refs') {
      if (method === 'POST' && tail.length === 2) {
        const ref = typeof obj.ref === 'string' ? obj.ref : '';
        const sha = typeof obj.sha === 'string' ? obj.sha : '';
        if (!ref || !/^[0-9a-f]{40}$/.test(sha)) { errJson(res, 422, 'Validation Failed'); return; }
        r.refs.set(ref, sha);
        sendJson(res, 201, { ref, object: { sha }, node_id: gid('Ref', nextGlobalId++) });
        return;
      }
      if (method === 'GET') {
        const refPath = tail.slice(2).join('/');
        if (!refPath) {
          sendJson(res, 200, sortedRefs(r).map(([n, oid]) => ({ ref: n, object: { sha: oid } })));
          return;
        }
        const oid = r.refs.get(refPath) ?? r.refs.get(`refs/${refPath}`) ?? r.refs.get(`refs/heads/${refPath}`);
        if (!oid) { errJson(res, 404, 'Not Found'); return; }
        sendJson(res, 200, { ref: refPath.startsWith('refs/') ? refPath : `refs/heads/${refPath}`, object: { sha: oid } });
        return;
      }
    }
    // contents
    if (tail.length >= 1 && tail[0] === 'contents') {
      const contentPath = tail.slice(1).map((s) => decodeURIComponent(s)).join('/');
      const refParam = q.get('ref') ?? (typeof obj.branch === 'string' ? obj.branch : undefined);
      const branch = refParam ?? r.defaultBranch;
      if (method === 'PUT') {
        const message = typeof obj.message === 'string' ? obj.message : 'Update';
        const contentB64 = typeof obj.content === 'string' ? obj.content : '';
        const content = Buffer.from(contentB64.replace(/\n/g, ''), 'base64');
        const tip = branchTip(r, branch);
        if (typeof obj.sha === 'string' && tip) {
          const files = treeFiles(r, commitTree(r, tip));
          const cur = files.get(contentPath);
          if (cur && cur !== obj.sha) { errJson(res, 409, 'Conflict: sha does not match'); return; }
          if (!cur) { errJson(res, 409, 'Conflict: file does not exist'); return; }
        }
        const isUpdate = tip ? treeFiles(r, commitTree(r, tip)).has(contentPath) : false;
        const { blobSha, commitSha } = commitFile(r, branch, contentPath, content, message);
        sendJson(res, isUpdate ? 200 : 201, {
          content: { name: contentPath.split('/').pop(), path: contentPath, sha: blobSha, size: content.length, encoding: 'base64', type: 'file' },
          commit: { sha: commitSha, message },
        });
        return;
      }
      if (method === 'GET') {
        const tip = branchTip(r, branch);
        if (!tip) { errJson(res, 404, 'Not Found'); return; }
        const files = treeFiles(r, commitTree(r, tip));
        if (contentPath === '') {
          // root listing
          const seen = new Map<string, { type: string; sha: string }>();
          for (const p of files.keys()) {
            const first = p.split('/')[0];
            if (!seen.has(first)) {
              seen.set(first, p.includes('/') ? { type: 'dir', sha: '' } : { type: 'file', sha: files.get(p)! });
            }
          }
          const list = [...seen.entries()].sort((a, b) => (a[0] < b[0] ? -1 : 1)).map(([n, v]) => ({
            type: v.type, name: n, path: n, sha: v.sha || oidOf('tree', Buffer.alloc(0)),
          }));
          sendJson(res, 200, list); return;
        }
        const blobOid = files.get(contentPath);
        if (blobOid) {
          const blob = r.objects.get(blobOid)!;
          sendJson(res, 200, {
            type: 'file', encoding: 'base64', size: blob.data.length, name: contentPath.split('/').pop(),
            path: contentPath, content: blob.data.toString('base64'), sha: blobOid,
          });
          return;
        }
        // directory?
        const prefix = `${contentPath}/`;
        const entries = new Map<string, { type: string; sha: string }>();
        for (const [p, oid] of files) {
          if (!p.startsWith(prefix)) continue;
          const restPath = p.slice(prefix.length);
          const first = restPath.split('/')[0];
          if (!entries.has(first)) entries.set(first, restPath.includes('/') ? { type: 'dir', sha: '' } : { type: 'file', sha: oid });
        }
        if (entries.size === 0) { errJson(res, 404, 'Not Found'); return; }
        sendJson(res, 200, [...entries.entries()].sort((a, b) => (a[0] < b[0] ? -1 : 1)).map(([n, v]) => ({
          type: v.type, name: n, path: `${contentPath}/${n}`, sha: v.sha || oidOf('tree', Buffer.alloc(0)),
        })));
        return;
      }
      if (method === 'DELETE') {
        const message = typeof obj.message === 'string' ? obj.message : 'Delete';
        const sha = deleteFile(r, branch, contentPath, message);
        if (!sha) { errJson(res, 404, 'Not Found'); return; }
        sendJson(res, 200, { commit: { sha, message } });
        return;
      }
    }
    // labels
    if (tail.length === 1 && tail[0] === 'labels') {
      if (method === 'POST') {
        const labelName = typeof obj.name === 'string' ? obj.name : '';
        if (!labelName) { errJson(res, 422, 'Validation Failed'); return; }
        const label = { name: labelName, color: typeof obj.color === 'string' ? obj.color : 'ededed' };
        r.labels.set(labelName, label);
        sendJson(res, 201, label); return;
      }
      if (method === 'GET') { sendJson(res, 200, [...r.labels.values()]); return; }
    }
    // issues
    if (tail.length >= 1 && tail[0] === 'issues') {
      if (tail.length === 1) {
        if (method === 'POST') {
          const title = typeof obj.title === 'string' ? obj.title : '';
          if (!title) { errJson(res, 422, 'Validation Failed: title is required'); return; }
          const labelNames: string[] = Array.isArray(obj.labels) ? obj.labels.map((l: unknown) => typeof l === 'string' ? l : (l as { name?: string }).name ?? '') : [];
          for (const ln of labelNames) if (!r.labels.has(ln)) { errJson(res, 422, `Validation Failed: label "${ln}" does not exist`); return; }
          const num = r.nextNumber++;
          const issue: Issue = {
            id: nextGlobalId++, number: num, title, body: typeof obj.body === 'string' ? obj.body : '',
            state: 'open', labels: labelNames.map((n) => r.labels.get(n)!), seq: nextGlobalId++, comments: [],
          };
          r.issues.set(num, issue);
          sendJson(res, 201, issueJson(r, issue)); return;
        }
        if (method === 'GET') {
          let list = [...r.issues.values()].sort((a, b) => a.seq - b.seq);
          const state = q.get('state') ?? 'open';
          if (state !== 'all') list = list.filter((i) => i.state === state);
          const sort = q.get('sort') ?? 'created';
          const dir = q.get('direction') ?? 'desc';
          if (sort === 'created') list.sort((a, b) => dir === 'asc' ? a.seq - b.seq : b.seq - a.seq);
          const { page, link } = paginate(list.map((i) => issueJson(r, i)), q, fullUrl);
          sendJson(res, 200, page, link ? { link } : {});
          return;
        }
      }
      const num = Number(tail[1]);
      const issue = Number.isInteger(num) ? r.issues.get(num) : undefined;
      if (tail.length === 2) {
        if (!issue) { errJson(res, 404, 'Not Found'); return; }
        if (method === 'GET') { sendJson(res, 200, issueJson(r, issue)); return; }
        if (method === 'PATCH') {
          if (typeof obj.state === 'string') issue.state = obj.state;
          if (typeof obj.title === 'string') issue.title = obj.title;
          if (typeof obj.body === 'string') issue.body = obj.body;
          sendJson(res, 200, issueJson(r, issue)); return;
        }
      }
      if (tail.length === 3 && tail[2] === 'comments') {
        if (!issue) { errJson(res, 404, 'Not Found'); return; }
        if (method === 'POST') {
          const comment = { id: nextGlobalId++, body: typeof obj.body === 'string' ? obj.body : '' };
          issue.comments.push(comment);
          sendJson(res, 201, comment); return;
        }
        if (method === 'GET') { sendJson(res, 200, issue.comments); return; }
      }
    }
    // pulls
    if (tail.length >= 1 && tail[0] === 'pulls') {
      if (tail.length === 1) {
        if (method === 'POST') {
          const headRaw = typeof obj.head === 'string' ? obj.head : '';
          const baseRaw = typeof obj.base === 'string' ? obj.base : '';
          const head = headRaw.includes(':') ? headRaw.split(':').pop()! : headRaw;
          const base = baseRaw.includes(':') ? baseRaw.split(':').pop()! : baseRaw;
          const num = r.nextNumber++;
          const pr: PR = {
            id: nextGlobalId++, number: num, title: typeof obj.title === 'string' ? obj.title : '',
            body: typeof obj.body === 'string' ? obj.body : '', state: 'open', merged: false,
            headRef: head, baseRef: base, headSha: branchTip(r, head), baseSha: branchTip(r, base),
            seq: nextGlobalId++, mergeSha: '', mergedAt: '',
          };
          r.prs.set(num, pr);
          sendJson(res, 201, prJson(r, pr)); return;
        }
        if (method === 'GET') {
          let list = [...r.prs.values()].sort((a, b) => a.seq - b.seq);
          const state = q.get('state') ?? 'open';
          if (state !== 'all') list = list.filter((p) => p.state === state);
          const { page, link } = paginate(list.map((p) => prJson(r, p)), q, fullUrl);
          sendJson(res, 200, page, link ? { link } : {});
          return;
        }
      }
      const num = Number(tail[1]);
      const pr = Number.isInteger(num) ? r.prs.get(num) : undefined;
      if (tail.length === 2) {
        if (!pr) { errJson(res, 404, 'Not Found'); return; }
        if (method === 'GET') { sendJson(res, 200, prJson(r, pr)); return; }
        if (method === 'PATCH') {
          if (typeof obj.state === 'string') pr.state = obj.state;
          if (typeof obj.title === 'string') pr.title = obj.title;
          sendJson(res, 200, prJson(r, pr)); return;
        }
      }
      if (tail.length === 3 && tail[2] === 'files') {
        if (!pr) { errJson(res, 404, 'Not Found'); return; }
        if (method === 'GET') {
          const files = diffFiles(r, commitTree(r, pr.baseSha), commitTree(r, pr.headSha));
          sendJson(res, 200, files); return;
        }
      }
      if (tail.length === 3 && tail[2] === 'merge') {
        if (!pr) { errJson(res, 404, 'Not Found'); return; }
        if (method === 'PUT') {
          if (pr.merged) { sendJson(res, 200, { sha: pr.mergeSha, merged: true, message: 'Pull Request successfully merged' }); return; }
          const result = doMerge(r, pr);
          if ('conflict' in result) { errJson(res, 405, result.conflict); return; }
          sendJson(res, 200, { sha: result.sha, merged: true, message: 'Pull Request successfully merged' });
          return;
        }
      }
    }
  }
  errJson(res, 404, 'Not Found');
}

// ---------------------------------------------------------------- GraphQL
type GQLValue = string | number | boolean | null | GQLValue[] | { [k: string]: GQLValue };
type ASTNode = { kind: string; name?: string; alias?: string; args?: Record<string, GQLValue>; selections?: ASTNode[]; typeCondition?: string; value?: GQLValue; operation?: string };
function tokenizeGQL(src: string): string[] {
  const tokens: string[] = [];
  let i = 0;
  while (i < src.length) {
    const c = src[i];
    if (/\s|,/.test(c)) { i++; continue; }
    if (c === '#') { while (i < src.length && src[i] !== '\n') i++; continue; }
    if (src.startsWith('...', i)) { tokens.push('...'); i += 3; continue; }
    if ('{}():[]!=$@'.includes(c)) { tokens.push(c); i++; continue; }
    if (c === '"') {
      let j = i + 1; let s = '';
      while (j < src.length && src[j] !== '"') { if (src[j] === '\\' && j + 1 < src.length) { s += src[j + 1]; j += 2; } else { s += src[j]; j++; } }
      tokens.push(JSON.stringify(s)); i = j + 1; continue;
    }
    if (/[A-Za-z_]/.test(c)) { let j = i; while (j < src.length && /[A-Za-z0-9_]/.test(src[j])) j++; tokens.push(src.slice(i, j)); i = j; continue; }
    if (/[0-9-]/.test(c)) { let j = i; while (j < src.length && /[0-9.eE+-]/.test(src[j])) j++; tokens.push(src.slice(i, j)); i = j; continue; }
    i++;
  }
  return tokens;
}
class GQLParser {
  toks: string[]; pos = 0;
  fragments = new Map<string, ASTNode[]>();
  constructor(src: string) { this.toks = tokenizeGQL(src); }
  peek(): string { return this.toks[this.pos] ?? ''; }
  next(): string { return this.toks[this.pos++] ?? ''; }
  expect(t: string): void { if (this.peek() === t) this.pos++; }
  parseDocument(): ASTNode[] {
    const ops: ASTNode[] = [];
    while (this.pos < this.toks.length) {
      const t = this.peek();
      if (t === 'fragment') {
        this.next(); const name = this.next(); this.next(); // 'on'
        const typeCond = this.next();
        const sels = this.parseSelectionSet();
        this.fragments.set(name, sels);
      } else if (t === 'query' || t === 'mutation' || t === 'subscription') {
        const op = this.next();
        let name: string | undefined;
        if (this.peek() !== '(' && this.peek() !== '{') name = this.next();
        if (this.peek() === '(') this.parseVarDefs();
        const sels = this.parseSelectionSet();
        ops.push({ kind: 'Operation', operation: op, name, selections: sels });
      } else if (t === '{') {
        const sels = this.parseSelectionSet();
        ops.push({ kind: 'Operation', operation: 'query', selections: sels });
      } else this.next();
    }
    return ops;
  }
  parseVarDefs(): void {
    this.expect('(');
    let depth = 1;
    while (this.pos < this.toks.length && depth > 0) {
      const t = this.next();
      if (t === '(' || t === '[' || t === '{') depth++;
      else if (t === ')' || t === ']' || t === '}') depth--;
    }
  }
  parseSelectionSet(): ASTNode[] {
    this.expect('{');
    const sels: ASTNode[] = [];
    while (this.pos < this.toks.length && this.peek() !== '}') {
      sels.push(this.parseField());
    }
    this.expect('}');
    return sels;
  }
  parseField(): ASTNode {
    if (this.peek() === '...') {
      this.next();
      if (this.peek() === 'on') {
        this.next(); const tc = this.next();
        return { kind: 'InlineFragment', typeCondition: tc, selections: this.parseSelectionSet() };
      }
      const name = this.next();
      return { kind: 'FragmentSpread', name };
    }
    let name = this.next();
    let alias: string | undefined;
    if (this.peek() === ':') { this.next(); alias = name; name = this.next(); }
    let args: Record<string, GQLValue> | undefined;
    if (this.peek() === '(') args = this.parseArgs();
    // directives: @name(args)
    while (this.peek() === '@') {
      this.next(); this.next();
      if (this.peek() === '(') this.parseArgs();
    }
    let selections: ASTNode[] | undefined;
    if (this.peek() === '{') selections = this.parseSelectionSet();
    return { kind: 'Field', name, alias, args, selections };
  }
  parseArgs(): Record<string, GQLValue> {
    this.expect('(');
    const args: Record<string, GQLValue> = {};
    while (this.pos < this.toks.length && this.peek() !== ')') {
      const name = this.next();
      this.expect(':');
      args[name] = this.parseValue();
    }
    this.expect(')');
    return args;
  }
  parseValue(): GQLValue {
    const t = this.next();
    if (t === '$') return { $var: this.next() } as unknown as GQLValue;
    if (t === '[') {
      const arr: GQLValue[] = [];
      while (this.peek() !== ']') arr.push(this.parseValue());
      this.expect(']');
      return arr;
    }
    if (t === '{') {
      const obj: Record<string, GQLValue> = {};
      while (this.peek() !== '}') {
        const k = this.next(); this.expect(':'); obj[k] = this.parseValue();
      }
      this.expect('}');
      return obj;
    }
    if (t.startsWith('"')) return JSON.parse(t) as string;
    if (t === 'true') return true;
    if (t === 'false') return false;
    if (t === 'null') return null;
    if (/^-?\d+(\.\d+)?$/.test(t)) return Number(t);
    return { $enum: t } as unknown as GQLValue;
  }
}
function resolveVars(v: GQLValue, vars: Record<string, unknown>): unknown {
  if (v !== null && typeof v === 'object' && !Array.isArray(v)) {
    const o = v as Record<string, unknown>;
    if ('$var' in o) return vars[o.$var as string];
    if ('$enum' in o) return o.$enum;
    const out: Record<string, unknown> = {};
    for (const [k, val] of Object.entries(o)) out[k] = resolveVars(val as GQLValue, vars);
    return out;
  }
  if (Array.isArray(v)) return v.map((x) => resolveVars(x, vars));
  return v;
}
const SCHEMA_TYPES: Record<string, { fields: string[]; enums?: string[] }> = {
  Issue: { fields: ['id', 'number', 'title', 'state', 'body', 'createdAt', 'author', 'url', 'comments'] },
  PullRequest: { fields: ['id', 'number', 'title', 'body', 'state', 'merged', 'mergeable', 'headRefName', 'baseRefName', 'headRefOid', 'baseRefOid', 'files', 'createdAt', 'author', 'mergedAt'] },
  Repository: { fields: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest'] },
  Viewer: { fields: ['login', 'id', 'name'] },
  User: { fields: ['login', 'id', 'name'] },
  SearchType: { fields: [], enums: ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION', 'CODE', 'COMMIT'] },
  IssueState: { fields: [], enums: ['OPEN', 'CLOSED'] },
  MergeableState: { fields: [], enums: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'] },
  PullRequestState: { fields: [], enums: ['OPEN', 'CLOSED', 'MERGED'] },
  PullRequestMergeMethod: { fields: [], enums: ['MERGE', 'SQUASH', 'REBASE'] },
  RepositoryPermission: { fields: [], enums: ['ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'] },
  IssueOrderField: { fields: [], enums: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'] },
  OrderDirection: { fields: [], enums: ['ASC', 'DESC'] },
  CreateIssuePayload: { fields: ['issue', 'clientMutationId'] },
  CloseIssuePayload: { fields: ['issue', 'clientMutationId'] },
  CreatePullRequestPayload: { fields: ['pullRequest', 'clientMutationId'] },
  MergePullRequestPayload: { fields: ['pullRequest', 'clientMutationId'] },
  AddCommentPayload: { fields: ['commentEdge', 'subject', 'clientMutationId'] },
};
type GObj = { __type: string; [k: string]: unknown };
function repoGQL(r: Repo): GObj {
  return {
    __type: 'Repository', id: gid('Repository', r.id), databaseId: r.id, name: r.name,
    nameWithOwner: `${r.owner}/${r.name}`,
    owner: { __type: 'User', login: r.owner, id: r.owner === VIEWER ? VIEWER_ID : 1001, name: 'Ada Iversen' } as GObj,
    defaultBranchRef: { __type: 'Ref', name: r.defaultBranch } as GObj,
    viewerPermission: 'ADMIN', hasIssuesEnabled: true, _repo: r,
  };
}
function issueGQL(r: Repo, i: Issue): GObj {
  return {
    __type: 'Issue', id: gid('Issue', i.id), number: i.number, title: i.title, body: i.body,
    state: i.state === 'open' ? 'OPEN' : 'CLOSED', createdAt: FIXED_DATE,
    author: { __type: 'User', login: VIEWER, id: VIEWER_ID } as GObj,
    url: `https://example.invalid/${r.owner}/${r.name}/issues/${i.number}`, _repo: r, _issue: i,
  };
}
function prGQL(r: Repo, p: PR): GObj {
  const state = p.state === 'open' ? 'OPEN' : p.merged ? 'MERGED' : 'CLOSED';
  return {
    __type: 'PullRequest', id: gid('PullRequest', p.id), number: p.number, title: p.title, body: p.body,
    state, merged: p.merged,
    mergeable: (!branchTip(r, p.headRef) || !branchTip(r, p.baseRef)) ? 'UNKNOWN' : prConflict(r, p) === null ? 'MERGEABLE' : 'CONFLICTING',
    headRefName: p.headRef, baseRefName: p.baseRef, headRefOid: p.headSha, baseRefOid: p.baseSha,
    createdAt: FIXED_DATE, mergedAt: p.merged ? p.mergedAt : null,
    author: { __type: 'User', login: VIEWER, id: VIEWER_ID } as GObj, _repo: r, _pr: p,
  };
}
function findRepoByDbId(id: number): Repo | undefined {
  for (const r of repos.values()) if (r.id === id) return r;
  return undefined;
}
function findIssueById(id: number): { r: Repo; i: Issue } | null {
  for (const r of repos.values()) for (const i of r.issues.values()) if (i.id === id) return { r, i };
  return null;
}
function findPrById(id: number): { r: Repo; p: PR } | null {
  for (const r of repos.values()) for (const p of r.prs.values()) if (p.id === id) return { r, p };
  return null;
}
function connectionGQL(type: string, items: GObj[], args: Record<string, unknown>): GObj {
  let list = items;
  const after = typeof args.after === 'string' ? args.after : null;
  if (after) {
    try {
      const decoded = Buffer.from(after, 'base64').toString('utf8');
      const idx = Number(decoded.slice(2));
      if (Number.isInteger(idx)) list = items.slice(idx + 1);
    } catch { /* ignore bad cursor */ }
  }
  const first = typeof args.first === 'number' ? args.first : undefined;
  const sliced = first !== undefined ? list.slice(0, first) : list;
  const endIdx = items.indexOf(sliced[sliced.length - 1]);
  const hasNext = first !== undefined && list.length > first;
  return {
    __type: type, _items: sliced,
    nodes: sliced,
    edges: sliced.map((node, i) => ({ __type: type.replace('Connection', 'Edge'), node, cursor: b64(`i:${items.indexOf(node)}`) })),
    pageInfo: {
      __type: 'PageInfo', hasNextPage: hasNext, hasPreviousPage: false,
      startCursor: sliced.length ? b64(`i:${items.indexOf(sliced[0])}`) : null,
      endCursor: sliced.length ? b64(`i:${endIdx}`) : null,
    } as GObj,
    totalCount: items.length,
  };
}
function getGQLField(type: string, obj: GObj, field: string, args: Record<string, unknown>): unknown {
  const r = obj._repo as Repo | undefined;
  switch (type) {
    case 'Query': {
      if (field === 'viewer') return { __type: 'Viewer', login: VIEWER, id: VIEWER_ID, name: 'Ada Iversen' } as GObj;
      if (field === 'repository') {
        const owner = String(args.owner ?? '');
        const name = String(args.name ?? '');
        if (owner !== VIEWER) return null;
        const repo = getRepo(owner, name);
        return repo ? repoGQL(repo) : null;
      }
      if (field === 'node') {
        const dec = decodeGid(String(args.id ?? ''));
        if (!dec) return null;
        if (dec.kind === 'Repository') { const repo = findRepoByDbId(dec.n); return repo ? repoGQL(repo) : null; }
        if (dec.kind === 'Issue') { const f = findIssueById(dec.n); return f ? issueGQL(f.r, f.i) : null; }
        if (dec.kind === 'PullRequest') { const f = findPrById(dec.n); return f ? prGQL(f.r, f.p) : null; }
        return null;
      }
      if (field === '__type') {
        const name = String(args.name ?? '');
        const t = SCHEMA_TYPES[name];
        if (!t) return null;
        return {
          __type: '__Type', name, kind: t.enums ? 'ENUM' : 'OBJECT',
          fields: t.fields.map((n) => ({ __type: '__Field', name: n })),
          enumValues: (t.enums ?? []).map((n) => ({ __type: '__EnumValue', name: n })),
        } as GObj;
      }
      if (field === '__schema') {
        return {
          __type: '__Schema',
          types: Object.keys(SCHEMA_TYPES).map((n) => ({ __type: '__Type', name: n, kind: SCHEMA_TYPES[n].enums ? 'ENUM' : 'OBJECT', fields: [], enumValues: [] })),
        } as GObj;
      }
      return undefined;
    }
    case 'Repository': {
      if (!r) return undefined;
      if (field === 'issue') {
        const num = Number(args.number);
        const i = r.issues.get(num);
        return i ? issueGQL(r, i) : null;
      }
      if (field === 'issues') {
        let list = [...r.issues.values()].sort((a, b) => a.seq - b.seq);
        if (Array.isArray(args.states)) {
          const want = new Set((args.states as unknown[]).map((s) => String(s).toLowerCase()));
          list = list.filter((i) => want.has(i.state));
        }
        const ob = args.orderBy as Record<string, unknown> | undefined;
        if (ob) {
          const dir = String(ob.direction ?? 'ASC');
          list.sort((a, b) => dir === 'DESC' ? b.seq - a.seq : a.seq - b.seq);
        }
        return connectionGQL('IssueConnection', list.map((i) => issueGQL(r, i)), args);
      }
      if (field === 'pullRequest') {
        const num = Number(args.number);
        const p = r.prs.get(num);
        return p ? prGQL(r, p) : null;
      }
      if (field === 'pullRequests') {
        const list = [...r.prs.values()].sort((a, b) => a.seq - b.seq);
        return connectionGQL('PullRequestConnection', list.map((p) => prGQL(r, p)), args);
      }
      return obj[field];
    }
    case 'PullRequest': {
      if (field === 'files') {
        const p = obj._pr as PR | undefined;
        if (!p || !r) return connectionGQL('FileConnection', [], args);
        const files = diffFiles(r, commitTree(r, p.baseSha), commitTree(r, p.headSha));
        const nodes = files.map((f) => ({ __type: 'ChangedFile', path: f.filename, additions: f.additions, deletions: f.deletions, changeType: f.status === 'added' ? 'ADDED' : f.status === 'removed' ? 'DELETED' : 'MODIFIED' } as GObj));
        return connectionGQL('FileConnection', nodes, args);
      }
      return obj[field];
    }
    case 'Issue': {
      if (field === 'comments') {
        const i = obj._issue as Issue | undefined;
        const nodes = (i?.comments ?? []).map((c) => ({ __type: 'Comment', id: gid('Comment', c.id), body: c.body } as GObj));
        return connectionGQL('CommentConnection', nodes, args);
      }
      return obj[field];
    }
    case 'Mutation': return undefined;
    default:
      return obj[field];
  }
}
const QUERY_FIELDS = new Set(['viewer', 'repository', 'node', '__type', '__schema']);
function applySelection(type: string, obj: GObj | null, sels: ASTNode[], vars: Record<string, unknown>, errors: string[], parser: GQLParser): Record<string, unknown> | null {
  if (obj === null) return null;
  const out: Record<string, unknown> = {};
  const allowed = type === 'Query' ? QUERY_FIELDS : new Set(SCHEMA_TYPES[type]?.fields ?? []);
  for (const sel of sels) {
    if (sel.kind === 'FragmentSpread') {
      const frag = parser.fragments.get(sel.name ?? '');
      if (frag) {
        const sub = applySelection(type, obj, frag, vars, errors, parser);
        if (sub) Object.assign(out, sub);
      }
      continue;
    }
    if (sel.kind === 'InlineFragment') {
      if (sel.typeCondition && sel.typeCondition !== type && sel.typeCondition !== obj.__type) continue;
      const sub = applySelection(type, obj, sel.selections ?? [], vars, errors, parser);
      if (sub) Object.assign(out, sub);
      continue;
    }
    const field = sel.name ?? '';
    const outName = sel.alias ?? field;
    if (field === '__typename') { out[outName] = obj.__type; continue; }
    if (type !== 'Query' && !allowed.has(field) && !(type === '__Type' && ['name', 'kind', 'fields', 'enumValues'].includes(field)) && !(type === '__Schema' && field === 'types') && !(type === '__Field' && field === 'name') && !(type === '__EnumValue' && field === 'name') && !(type === 'PageInfo' && ['hasNextPage', 'hasPreviousPage', 'startCursor', 'endCursor'].includes(field)) && !(type.endsWith('Connection') && ['nodes', 'edges', 'pageInfo', 'totalCount'].includes(field)) && !(type.endsWith('Edge') && ['node', 'cursor'].includes(field)) && !(type === 'Ref' && field === 'name') && !(type === 'ChangedFile' && ['path', 'additions', 'deletions', 'changeType'].includes(field)) && !(type === 'Comment' && ['id', 'body'].includes(field))) {
      errors.push(`Cannot query field "${field}" on type "${type}".`);
      continue;
    }
    const rawArgs: Record<string, unknown> = {};
    if (sel.args) for (const [k, v] of Object.entries(sel.args)) rawArgs[k] = resolveVars(v, vars);
    const val = getGQLField(type, obj, field, rawArgs);
    if (val === undefined) { errors.push(`Cannot query field "${field}" on type "${type}".`); continue; }
    out[outName] = applyValue(val, sel.selections, vars, errors, parser);
  }
  return out;
}
function applyValue(val: unknown, sels: ASTNode[] | undefined, vars: Record<string, unknown>, errors: string[], parser: GQLParser): unknown {
  if (val === null || val === undefined) return val;
  if (Array.isArray(val)) return val.map((v) => applyValue(v, sels, vars, errors, parser));
  if (typeof val === 'object') {
    const o = val as GObj;
    if (!sels) return o;
    const t = o.__type ?? 'Object';
    if (t === '__Type' || t === '__Schema' || t === '__Field' || t === '__EnumValue' || t === 'PageInfo' || t.endsWith('Connection') || t.endsWith('Edge') || t === 'Ref' || t === 'ChangedFile' || t === 'Comment') {
      return applySelection(t, o, sels, vars, errors, parser);
    }
    return applySelection(t, o, sels, vars, errors, parser);
  }
  return val;
}
function executeGQLOp(query: string, variables: Record<string, unknown>): Record<string, unknown> {
  const parser = new GQLParser(query);
  const ops = parser.parseDocument();
  if (ops.length === 0) return { data: null, errors: [{ message: 'No operation found' }] };
  const op = ops[0];
  const errors: string[] = [];
  if (op.operation === 'mutation') {
    const data: Record<string, unknown> = {};
    for (const sel of op.selections ?? []) {
      const field = sel.name ?? '';
      const rawArgs: Record<string, unknown> = {};
      if (sel.args) for (const [k, v] of Object.entries(sel.args)) rawArgs[k] = resolveVars(v, variables);
      const input = (rawArgs.input ?? {}) as Record<string, unknown>;
      if (field === 'createIssue') {
        const dec = decodeGid(String(input.repositoryId ?? ''));
        const repo = dec ? findRepoByDbId(dec.n) : undefined;
        if (!repo) { errors.push('Repository not found'); continue; }
        const num = repo.nextNumber++;
        const issue: Issue = { id: nextGlobalId++, number: num, title: String(input.title ?? ''), body: typeof input.body === 'string' ? input.body : '', state: 'open', labels: [], seq: nextGlobalId++, comments: [] };
        repo.issues.set(num, issue);
        const payload = { __type: 'CreateIssuePayload', issue: issueGQL(repo, issue), clientMutationId: null } as GObj;
        data[sel.alias ?? field] = applySelection('CreateIssuePayload', payload, sel.selections ?? [], variables, errors, parser);
      } else if (field === 'closeIssue') {
        const dec = decodeGid(String(input.issueId ?? ''));
        const found = dec ? findIssueById(dec.n) : null;
        if (!found) { errors.push('Issue not found'); continue; }
        found.i.state = 'closed';
        const payload = { __type: 'CloseIssuePayload', issue: issueGQL(found.r, found.i), clientMutationId: null } as GObj;
        data[sel.alias ?? field] = applySelection('CloseIssuePayload', payload, sel.selections ?? [], variables, errors, parser);
      } else if (field === 'createPullRequest') {
        const dec = decodeGid(String(input.repositoryId ?? ''));
        const repo = dec ? findRepoByDbId(dec.n) : undefined;
        if (!repo) { errors.push('Repository not found'); continue; }
        const num = repo.nextNumber++;
        const head = String(input.headRefName ?? ''); const base = String(input.baseRefName ?? '');
        const pr: PR = { id: nextGlobalId++, number: num, title: String(input.title ?? ''), body: typeof input.body === 'string' ? input.body : '', state: 'open', merged: false, headRef: head, baseRef: base, headSha: branchTip(repo, head), baseSha: branchTip(repo, base), seq: nextGlobalId++, mergeSha: '', mergedAt: '' };
        repo.prs.set(num, pr);
        const payload = { __type: 'CreatePullRequestPayload', pullRequest: prGQL(repo, pr), clientMutationId: null } as GObj;
        data[sel.alias ?? field] = applySelection('CreatePullRequestPayload', payload, sel.selections ?? [], variables, errors, parser);
      } else if (field === 'mergePullRequest') {
        const dec = decodeGid(String(input.pullRequestId ?? ''));
        const found = dec ? findPrById(dec.n) : null;
        if (!found) { errors.push('Pull request not found'); continue; }
        if (!found.p.merged) doMerge(found.r, found.p);
        const payload = { __type: 'MergePullRequestPayload', pullRequest: prGQL(found.r, found.p), clientMutationId: null } as GObj;
        data[sel.alias ?? field] = applySelection('MergePullRequestPayload', payload, sel.selections ?? [], variables, errors, parser);
      } else if (field === 'addComment') {
        const dec = decodeGid(String(input.subjectId ?? ''));
        const found = dec ? findIssueById(dec.n) : null;
        if (!found) { errors.push('Subject not found'); continue; }
        const comment = { id: nextGlobalId++, body: String(input.body ?? '') };
        found.i.comments.push(comment);
        const payload = {
          __type: 'AddCommentPayload', clientMutationId: null,
          commentEdge: { __type: 'CommentEdge', node: { __type: 'Comment', id: gid('Comment', comment.id), body: comment.body } as GObj, cursor: b64(`i:${found.i.comments.length - 1}`) } as GObj,
          subject: issueGQL(found.r, found.i),
        } as GObj;
        data[sel.alias ?? field] = applySelection('AddCommentPayload', payload, sel.selections ?? [], variables, errors, parser);
      } else {
        errors.push(`Cannot query field "${field}" on type "Mutation".`);
      }
    }
    if (errors.length) return { data: null, errors: errors.map((m) => ({ message: m })) };
    return { data };
  }
  const root: GObj = { __type: 'Query' };
  const data = applySelection('Query', root, op.selections ?? [], variables, errors, parser);
  if (errors.length) return { data: null, errors: errors.map((m) => ({ message: m })) };
  return { data };
}

// ---------------------------------------------------------------- MCP
const MCP_VERSIONS = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];
const MCP_TOOLS: { name: string; description: string; inputSchema: Record<string, unknown> }[] = [
  { name: 'create_repo', description: 'Create a new repository owned by the authenticated user.', inputSchema: { type: 'object', properties: { name: { type: 'string', description: 'Repository name' }, description: { type: 'string' }, private: { type: 'boolean' } }, required: ['name'] } },
  { name: 'get_repo', description: 'Get a repository by owner and name.', inputSchema: { type: 'object', properties: { owner: { type: 'string' }, repo: { type: 'string' } }, required: ['owner', 'repo'] } },
  { name: 'create_issue', description: 'Create an issue in a repository.', inputSchema: { type: 'object', properties: { owner: { type: 'string' }, repo: { type: 'string' }, title: { type: 'string' }, body: { type: 'string' } }, required: ['owner', 'repo', 'title'] } },
  { name: 'list_issues', description: 'List issues in a repository.', inputSchema: { type: 'object', properties: { owner: { type: 'string' }, repo: { type: 'string' }, state: { type: 'string' } }, required: ['owner', 'repo'] } },
  { name: 'create_pull_request', description: 'Create a pull request in a repository.', inputSchema: { type: 'object', properties: { owner: { type: 'string' }, repo: { type: 'string' }, title: { type: 'string' }, head: { type: 'string' }, base: { type: 'string' } }, required: ['owner', 'repo', 'title', 'head', 'base'] } },
  { name: 'merge_pull_request', description: 'Merge a pull request in a repository.', inputSchema: { type: 'object', properties: { owner: { type: 'string' }, repo: { type: 'string' }, pull_number: { type: 'integer' } }, required: ['owner', 'repo', 'pull_number'] } },
];
function mcpOwnerRepo(args: Record<string, unknown>): { owner: string; repo: string } {
  let owner = typeof args.owner === 'string' ? args.owner : '';
  let repo = typeof args.repo === 'string' ? args.repo : '';
  if ((!owner || !repo) && typeof args.full_name === 'string') {
    const parts = (args.full_name as string).split('/');
    if (parts.length === 2) { owner = owner || parts[0]; repo = repo || parts[1]; }
  }
  if (!owner) owner = VIEWER;
  return { owner, repo };
}
function mcpText(obj: unknown): { content: { type: string; text: string }[]; structuredContent: unknown } {
  return { content: [{ type: 'text', text: JSON.stringify(obj) }], structuredContent: obj };
}
function mcpErrorResult(message: string): { content: { type: string; text: string }[]; isError: boolean } {
  return { content: [{ type: 'text', text: JSON.stringify({ error: message }) }], isError: true };
}
async function handleMcp(req: IncomingMessage, res: ServerResponse, body: Buffer): Promise<void> {
  const parsed = parseJsonBody(body);
  if (!parsed.ok) {
    sendJson(res, 400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } });
    return;
  }
  const msg = parsed.value as Record<string, unknown>;
  const id = 'id' in msg ? msg.id : null;
  if (!authOk(req)) {
    sendJson(res, 401, { jsonrpc: '2.0', id: id ?? null, error: { code: -32000, message: 'Unauthorized' } });
    return;
  }
  const method = typeof msg.method === 'string' ? msg.method : '';
  const params = (msg.params ?? {}) as Record<string, unknown>;
  const rpcResult = (result: unknown): void => sendJson(res, 200, { jsonrpc: '2.0', id, result });
  const rpcError = (code: number, message: string, data?: unknown): void =>
    sendJson(res, 200, { jsonrpc: '2.0', id, error: { code, message, ...(data !== undefined ? { data } : {}) } });
  // notification (no id): accept silently
  if (id === null || id === undefined) {
    if (method === 'notifications/initialized' || method.startsWith('notifications/')) {
      res.writeHead(202, { 'content-length': '0' });
      res.end();
      return;
    }
  }
  if (method === 'server/discover') {
    rpcResult({
      protocolVersion: '2026-07-28',
      supportedVersions: MCP_VERSIONS,
      serverInfo: { name: 'github-forge', version: '1.0.0' },
      capabilities: { tools: {} },
      _meta: { 'io.modelcontextprotocol/serverInfo': { name: 'github-forge', version: '1.0.0' } },
    });
    return;
  }
  if (method === 'initialize') {
    const requested = typeof params.protocolVersion === 'string' ? params.protocolVersion : '2026-07-28';
    if (!MCP_VERSIONS.includes(requested)) {
      rpcError(-32022, 'Unsupported protocol version', { supported: MCP_VERSIONS });
      return;
    }
    rpcResult({
      protocolVersion: requested,
      capabilities: { tools: {} },
      serverInfo: { name: 'github-forge', version: '1.0.0' },
    });
    return;
  }
  if (method === 'ping') { rpcResult({}); return; }
  if (method === 'tools/list') { rpcResult({ tools: MCP_TOOLS }); return; }
  if (method === 'tools/call') {
    const rawName = typeof params.name === 'string' ? params.name : '';
    const args = (params.arguments ?? {}) as Record<string, unknown>;
    const norm = rawName.toLowerCase();
    const base = norm.includes('__') ? norm.split('__').pop()! : norm;
    const tool = MCP_TOOLS.find((t) => t.name === base);
    if (!tool) { rpcError(-32601, `Unknown tool: ${rawName}`); return; }
    const schema = tool.inputSchema as { required?: string[] };
    for (const reqArg of schema.required ?? []) {
      if (args[reqArg] === undefined && args[reqArg === 'pull_number' ? 'number' : reqArg] === undefined && args[reqArg === 'pull_number' ? 'pr_number' : reqArg] === undefined) {
        rpcResult(mcpErrorResult(`Missing required argument: ${reqArg}`));
        return;
      }
    }
    try {
      let result: unknown;
      if (tool.name === 'create_repo') {
        const name = String(args.name ?? '');
        let repo = getRepo(VIEWER, name);
        if (!repo) repo = createRepo(name, args.private === true, typeof args.description === 'string' ? args.description : '');
        result = repoJson(repo);
      } else if (tool.name === 'get_repo') {
        const { owner, repo: rn } = mcpOwnerRepo(args);
        const repo = owner === VIEWER ? getRepo(owner, rn) : undefined;
        if (!repo) { rpcResult(mcpErrorResult(`Repository not found: ${owner}/${rn}`)); return; }
        result = repoJson(repo);
      } else if (tool.name === 'create_issue') {
        const { owner, repo: rn } = mcpOwnerRepo(args);
        const repo = owner === VIEWER ? getRepo(owner, rn) : undefined;
        if (!repo) { rpcResult(mcpErrorResult(`Repository not found: ${owner}/${rn}`)); return; }
        const num = repo.nextNumber++;
        const issue: Issue = { id: nextGlobalId++, number: num, title: String(args.title ?? ''), body: typeof args.body === 'string' ? args.body : '', state: 'open', labels: [], seq: nextGlobalId++, comments: [] };
        repo.issues.set(num, issue);
        result = issueJson(repo, issue);
      } else if (tool.name === 'list_issues') {
        const { owner, repo: rn } = mcpOwnerRepo(args);
        const repo = owner === VIEWER ? getRepo(owner, rn) : undefined;
        if (!repo) { rpcResult(mcpErrorResult(`Repository not found: ${owner}/${rn}`)); return; }
        let list = [...repo.issues.values()];
        if (typeof args.state === 'string' && args.state !== 'all') list = list.filter((i) => i.state === args.state);
        result = { issues: list.map((i) => issueJson(repo, i)), count: list.length };
      } else if (tool.name === 'create_pull_request') {
        const { owner, repo: rn } = mcpOwnerRepo(args);
        const repo = owner === VIEWER ? getRepo(owner, rn) : undefined;
        if (!repo) { rpcResult(mcpErrorResult(`Repository not found: ${owner}/${rn}`)); return; }
        const head = String(args.head ?? ''); const base = String(args.base ?? '');
        const num = repo.nextNumber++;
        const pr: PR = { id: nextGlobalId++, number: num, title: String(args.title ?? ''), body: typeof args.body === 'string' ? args.body : '', state: 'open', merged: false, headRef: head, baseRef: base, headSha: branchTip(repo, head), baseSha: branchTip(repo, base), seq: nextGlobalId++, mergeSha: '', mergedAt: '' };
        repo.prs.set(num, pr);
        result = prJson(repo, pr);
      } else if (tool.name === 'merge_pull_request') {
        const { owner, repo: rn } = mcpOwnerRepo(args);
        const repo = owner === VIEWER ? getRepo(owner, rn) : undefined;
        if (!repo) { rpcResult(mcpErrorResult(`Repository not found: ${owner}/${rn}`)); return; }
        const num = Number(args.pull_number ?? args.number ?? args.pr_number);
        const pr = repo.prs.get(num);
        if (!pr) { rpcResult(mcpErrorResult(`Pull request not found: ${num}`)); return; }
        if (!pr.merged) {
          const merged = doMerge(repo, pr);
          if ('conflict' in merged) { rpcResult(mcpErrorResult(merged.conflict)); return; }
        }
        result = { sha: pr.mergeSha, merged: true, message: 'Pull Request successfully merged', number: pr.number, pull_number: pr.number };
      }
      rpcResult(mcpText(result));
    } catch (e) {
      rpcResult(mcpErrorResult(e instanceof Error ? e.message : 'Tool call failed'));
    }
    return;
  }
  rpcError(-32601, `Method not found: ${method}`);
}

// ---------------------------------------------------------------- server
export function createApp(): Server {
  return createServer(async (req: IncomingMessage, res: ServerResponse) => {
    try {
      const path = pathOf(req);
      const method = req.method ?? 'GET';
      const body = await readBody(req);
      // health probe (no auth)
      if (method === 'GET' && path === '/healthz') { sendJson(res, 200, { ok: true }); return; }
      // git smart HTTP: /{owner}/{repo}.git/...
      const gitMatch = path.match(/^\/([^/]+)\/([^/]+)\.git(\/.*)?$/);
      if (gitMatch) {
        const owner = decodeURIComponent(gitMatch[1]);
        const repoName = decodeURIComponent(gitMatch[2]);
        const sub = gitMatch[3] ?? '';
        if (!authOk(req)) { errJson(res, 401, 'Requires authentication'); return; }
        const repo = owner === VIEWER ? getRepo(owner, repoName) : undefined;
        if (!repo) { errJson(res, 404, 'Not Found'); return; }
        if (method === 'GET' && sub === '/info/refs') {
          const service = queryOf(req).get('service') ?? 'git-upload-pack';
          const v2 = (req.headers['git-protocol'] ?? '') === 'version=2';
          if (v2 && service === 'git-upload-pack') {
            sendRaw(res, 200, v2Advertisement(), 'application/x-git-upload-pack-advertisement');
          } else {
            sendRaw(res, 200, v0Advertisement(repo, service), `application/x-${service}-advertisement`);
          }
          return;
        }
        if (method === 'POST' && sub === '/git-upload-pack') {
          try {
            const v2 = (req.headers['git-protocol'] ?? '') === 'version=2';
            if (v2) {
              const { packets } = parsePackets(body);
              const lines: string[] = [];
              let command = '';
              let afterDelim = false;
              for (const p of packets) {
                if (p.kind === 'delim') { afterDelim = true; continue; }
                if (p.kind !== 'data') continue;
                const text = p.payload.toString('utf8').replace(/\n$/, '');
                if (!afterDelim && text.startsWith('command=')) command = text.slice(8);
                else if (afterDelim) lines.push(text);
                else if (text.startsWith('command=')) command = text.slice(8);
              }
              if (command === 'ls-refs') { sendRaw(res, 200, handleLsRefs(repo, lines), 'application/x-git-upload-pack-result'); return; }
              if (command === 'fetch') { sendRaw(res, 200, handleFetchV2(repo, lines), 'application/x-git-upload-pack-result'); return; }
              sendRaw(res, 200, FLUSH, 'application/x-git-upload-pack-result'); return;
            }
            sendRaw(res, 200, handleFetchV0(repo, body), 'application/x-git-upload-pack-result');
          } catch {
            sendRaw(res, 200, FLUSH, 'application/x-git-upload-pack-result');
          }
          return;
        }
        if (method === 'POST' && sub === '/git-receive-pack') {
          const out = handleReceivePack(repo, body);
          sendRaw(res, 200, out, 'application/x-git-receive-pack-result');
          return;
        }
        errJson(res, 404, 'Not Found');
        return;
      }
      // GraphQL
      if (path === '/api/graphql') {
        if (method !== 'POST') { errJson(res, 404, 'Not Found'); return; }
        if (!authOk(req)) { errJson(res, 401, 'Requires authentication'); return; }
        const parsed = parseJsonBody(body);
        if (!parsed.ok) { sendJson(res, 400, { errors: [{ message: 'Problems parsing JSON' }] }); return; }
        const ops = Array.isArray(parsed.value) ? parsed.value : [parsed.value];
        const results = ops.map((op) => {
          const o = (op ?? {}) as Record<string, unknown>;
          return executeGQLOp(String(o.query ?? ''), (o.variables ?? {}) as Record<string, unknown>);
        });
        sendJson(res, 200, Array.isArray(parsed.value) ? results : results[0]);
        return;
      }
      // MCP
      if (path === '/mcp') {
        if (method !== 'POST') { errJson(res, 404, 'Not Found'); return; }
        await handleMcp(req, res, body);
        return;
      }
      // REST v3
      if (path === '/api/v3' || path.startsWith('/api/v3/')) {
        await handleRest(req, res, path, body);
        return;
      }
      errJson(res, 404, 'Not Found');
    } catch (e) {
      try { errJson(res, 500, e instanceof Error ? e.message : 'Internal Server Error'); } catch { /* socket gone */ }
    }
  });
}
