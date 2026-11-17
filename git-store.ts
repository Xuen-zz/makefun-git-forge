import { createHash, hash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

const formats = ['', 'commit', 'tree', 'blob', 'tag'] as const;
export type Kind = 'commit' | 'tree' | 'blob' | 'tag';
export type Entry = { mode: string; oid: string };
type TreeEntry = Entry & { name: string };
type Commit = { tree: string; parents: string[]; message: string; author: string; committer: string };
type ObjectData = { kind: Kind; data: Buffer; commit?: Commit; tree?: TreeEntry[]; target?: string; packed?: Buffer };
type Objects = Map<string, ObjectData>;

export const ZERO = '0'.repeat(40);
export const isOid = (s: unknown): s is string => typeof s === 'string' && /^[0-9a-f]{40}$/.test(s);
export const branchRef = (branch: string) => `refs/heads/${branch}`;
export const sha1 = (data: Buffer) => hash('sha1', data, 'buffer');
const SMALL_OBJECT_SIZE = 2048;
const objectHashScratch = Buffer.allocUnsafe(SMALL_OBJECT_SIZE + 32);

function objectId(kind: Kind, data: Buffer): string {
  const header = `${kind} ${data.length}\0`;
  if (data.length > SMALL_OBJECT_SIZE) return createHash('sha1').update(header).update(data).digest('hex');
  // Both the scratch copy and one-shot digest finish before the next object is processed.
  const offset = objectHashScratch.write(header, 'ascii');
  data.copy(objectHashScratch, offset);
  return hash('sha1', objectHashScratch.subarray(0, offset + data.length), 'hex');
}
const isTreeMode = (mode: string) => mode === '40000' || mode === '040000';

export function validRef(ref: unknown): ref is string {
  return typeof ref === 'string' && ref.startsWith('refs/') && !ref.includes('..') && !ref.includes('@{') &&
    !/[\x00-\x20\x7f~^:?*\[\\]/.test(ref) && !ref.endsWith('.') &&
    ref.split('/').every(part => part && !part.startsWith('.') && !part.endsWith('.lock'));
}

export function validPath(path: string): boolean {
  return path.split('/').every(part => part && part !== '.' && part !== '..' && part.toLowerCase() !== '.git' && !/[\0\\]/.test(part));
}

export function asCommit(object?: ObjectData): Commit | undefined {
  if (object?.kind !== 'commit') return;
  if (object.commit) return object.commit;
  const text = object.data.toString();
  const end = text.indexOf('\n\n');
  let tree = '', author = '', committer = '';
  const parents: string[] = [];
  for (const line of text.slice(0, end < 0 ? undefined : end).split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5);
    if (line.startsWith('parent ')) parents.push(line.slice(7));
    if (line.startsWith('author ')) author = line.slice(7);
    if (line.startsWith('committer ')) committer = line.slice(10);
  }
  if (!isOid(tree) || !parents.every(isOid)) throw new Error('Invalid commit object');
  return object.commit = { tree, parents, author, committer, message: end < 0 ? '' : text.slice(end + 2) };
}

export function asTree(object?: ObjectData): TreeEntry[] {
  if (object?.kind !== 'tree') throw new Error('Missing tree object');
  if (object.tree) return object.tree;
  const { data } = object;
  const entries: TreeEntry[] = [];
  for (let pos = 0; pos < data.length;) {
    const space = data.indexOf(32, pos);
    const nul = data.indexOf(0, space + 1);
    if (space <= pos || nul <= space + 1 || nul + 21 > data.length) throw new Error('Invalid tree entry');
    entries.push({ mode: data.toString('ascii', pos, space), name: data.toString('utf8', space + 1, nul), oid: data.toString('hex', nul + 1, nul + 21) });
    pos = nul + 21;
  }
  return object.tree = entries;
}

export function tagTarget(object?: ObjectData): string | undefined {
  if (object?.kind !== 'tag') return;
  if (object.target) return object.target;
  const target = object.data.toString('ascii', 0, 47);
  if (!/^object [0-9a-f]{40}$/.test(target)) throw new Error('Invalid tag object');
  return object.target = target.slice(7);
}

const NO_REFERENCES: readonly string[] = [];

export function references(object: ObjectData, includeParents = true): readonly string[] {
  switch (object.kind) {
    case 'commit': {
      const commit = asCommit(object)!;
      return includeParents ? [commit.tree, ...commit.parents] : [commit.tree];
    }
    case 'tree': return asTree(object).filter(entry => entry.mode !== '160000').map(entry => entry.oid);
    case 'tag': return [tagTarget(object)!];
    case 'blob': return NO_REFERENCES;
  }
}

export class GitStore {
  readonly objects = new Map<string, ObjectData>();
  private readonly refMap = new Map<string, string>();
  readonly refs: ReadonlyMap<string, string> = this.refMap;
  refVersion = 0;

  setRef(ref: string, oid: string) {
    if (this.refMap.get(ref) === oid) return;
    this.refMap.set(ref, oid);
    this.refVersion++;
  }

  deleteRef(ref: string) {
    if (this.refMap.delete(ref)) this.refVersion++;
  }

  put(kind: Kind, data: Buffer): string {
    const oid = objectId(kind, data);
    if (!this.objects.has(oid)) this.objects.set(oid, { kind, data });
    return oid;
  }

  peel(oid: string): string {
    const seen = new Set<string>();
    for (let target = tagTarget(this.objects.get(oid)); target; target = tagTarget(this.objects.get(oid))) {
      if (seen.has(oid)) throw new Error('Cyclic tag');
      seen.add(oid);
      oid = target;
    }
    return oid;
  }

  resolve(ref: string): string | undefined {
    const oid = this.refs.get(ref) ?? this.refs.get(branchRef(ref)) ?? this.refs.get(`refs/tags/${ref}`) ?? (isOid(ref) ? ref : undefined);
    if (oid === undefined) return;
    const peeled = this.peel(oid);
    if (this.objects.get(peeled)?.kind === 'commit') return peeled;
  }

  *history(start: string, lookup = (oid: string) => this.objects.get(oid)): Generator<string> {
    const queue = [start];
    const seen = new Set<string>();
    for (let index = 0; index < queue.length; index++) {
      const oid = queue[index];
      if (seen.has(oid)) continue;
      const commit = asCommit(lookup(oid));
      if (!commit) continue;
      seen.add(oid);
      yield oid;
      for (const parent of commit.parents) queue.push(parent);
    }
  }

  files(commit?: string): Map<string, Entry> {
    const files = new Map<string, Entry>();
    if (!commit) return files;
    const root = asCommit(this.objects.get(commit));
    if (!root) return files;
    const visit = (tree: string, prefix: string, depth: number) => {
      if (depth > 256) throw new Error('Tree exceeds maximum depth');
      for (const { name, mode, oid } of asTree(this.objects.get(tree))) {
        if (isTreeMode(mode)) visit(oid, `${prefix}${name}/`, depth + 1);
        else files.set(prefix + name, { mode, oid });
      }
    };
    visit(root.tree, '', 0);
    return files;
  }

  writeCommit(files: ReadonlyMap<string, Entry>, parents: string[], message: string, identity: string): string {
    type Directory = { files: Map<string, Entry>; dirs: Map<string, Directory> };
    const directory = (): Directory => ({ files: new Map(), dirs: new Map() });
    const root = directory();
    for (const [path, entry] of files) {
      const parts = path.split('/');
      const name = parts.pop()!;
      let current = root;
      for (const part of parts) {
        if (current.files.has(part)) throw new Error('Path is already a file');
        if (!current.dirs.has(part)) current.dirs.set(part, directory());
        current = current.dirs.get(part)!;
      }
      if (current.dirs.has(name)) throw new Error('Path is already a directory');
      current.files.set(name, entry);
    }
    const writeTree = (dir: Directory): string => {
      const children = [...dir.files].map(([name, entry]) => ({ name, ...entry }));
      for (const [name, child] of dir.dirs) children.push({ name, mode: '40000', oid: writeTree(child) });
      const sortKey = (e: TreeEntry) => Buffer.from(e.name + (isTreeMode(e.mode) ? '/' : ''));
      children.sort((a, b) => Buffer.compare(sortKey(a), sortKey(b)));
      return this.put('tree', Buffer.concat(children.flatMap(({ mode, name, oid }) => [Buffer.from(`${mode} ${name}\0`), Buffer.from(oid, 'hex')])));
    };
    const header = [`tree ${writeTree(root)}`, ...parents.map(oid => `parent ${oid}`), `author ${identity}`, `committer ${identity}`];
    const text = message.endsWith('\n') ? message : `${message}\n`;
    return this.put('commit', Buffer.from(`${header.join('\n')}\n\n${text}`));
  }
}

const MAX_OBJECT = 32 * 1024 * 1024;
const MAX_INFLATED_PACK = 128 * 1024 * 1024;

function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let pos = 0;
  const integer = () => {
    let value = 0;
    for (let scale = 1; pos < delta.length && scale <= MAX_OBJECT * 128; scale *= 128) {
      const byte = delta[pos++];
      value += (byte & 127) * scale;
      if (!(byte & 128)) return value;
    }
    throw new Error('Invalid delta length');
  };
  if (integer() !== base.length) throw new Error('Delta base size differs');
  const size = integer();
  if (size > MAX_OBJECT) throw new Error('Delta result is too large');
  const result = Buffer.allocUnsafe(size);
  let written = 0;
  while (pos < delta.length) {
    const op = delta[pos++];
    if (!op) throw new Error('Invalid delta instruction');
    if (op & 128) {
      let offset = 0;
      let count = 0;
      for (let i = 0; i < 7; i++) {
        if (!(op & (1 << i))) continue;
        if (pos >= delta.length) throw new Error('Truncated delta copy');
        const byte = delta[pos++];
        if (i < 4) offset += byte * 256 ** i;
        else count += byte * 256 ** (i - 4);
      }
      count ||= 65536;
      if (offset + count > base.length || written + count > size) throw new Error('Invalid delta copy range');
      base.copy(result, written, offset, offset + count);
      written += count;
    } else {
      if (pos + op > delta.length || written + op > size) throw new Error('Invalid delta literal range');
      delta.copy(result, written, pos, pos + op);
      pos += op;
      written += op;
    }
  }
  if (written !== size) throw new Error('Incomplete delta result');
  return result;
}

/** Decode a pack into a quarantine, with optional thin-pack bases from this repository. */
export function readPack(pack: Buffer, existing: Objects): Objects {
  if (pack.length < 32 || pack.toString('ascii', 0, 4) !== 'PACK') throw new Error('Missing pack header');
  const end = pack.length - 20;
  const version = pack.readUInt32BE(4);
  const count = pack.readUInt32BE(8);
  if ((version !== 2 && version !== 3) || count > (end - 12) / 3 || !sha1(pack.subarray(0, end)).equals(pack.subarray(end))) {
    throw new Error('Invalid pack header or checksum');
  }
  let pos = 12;
  let inflated = 0;
  const objects: Objects = new Map();
  const offsets = new Map<number, ObjectData>();
  const pending: { offset: number; base: string | number; data: Buffer }[] = [];
  const byte = () => {
    if (pos >= end) throw new Error('Truncated pack');
    return pack[pos++];
  };
  const add = (offset: number, kind: Kind, data: Buffer, packedEnd?: number) => {
    const oid = objectId(kind, data);
    let object = existing.get(oid) ?? objects.get(oid);
    if (!object) {
      const packed = packedEnd === undefined ? undefined : Buffer.from(pack.subarray(offset, packedEnd));
      object = { kind, data, packed };
    }
    objects.set(oid, object);
    offsets.set(offset, object);
  };
  for (let index = 0; index < count; index++) {
    const offset = pos;
    let header = byte();
    const type = (header >> 4) & 7;
    let length = header & 15;
    for (let scale = 16; header & 128; scale *= 128) {
      if (scale > MAX_OBJECT * 128) throw new Error('Invalid pack length');
      header = byte();
      length += (header & 127) * scale;
    }
    inflated += length;
    if (length > MAX_OBJECT || inflated > MAX_INFLATED_PACK) throw new Error('Pack exceeds inflate limit');
    let base: string | number | undefined;
    if (type === 6) {
      let digit = byte();
      let distance = digit & 127;
      while (digit & 128) {
        digit = byte();
        distance = (distance + 1) * 128 + (digit & 127);
        if (distance > offset) throw new Error('Invalid delta base offset');
      }
      base = offset - distance;
      if (distance === 0 || base < 12) throw new Error('Invalid delta base offset');
    } else if (type === 7) {
      if (pos + 20 > end) throw new Error('Truncated delta base id');
      base = pack.toString('hex', pos, pos + 20);
      pos += 20;
    } else if (!formats[type]) throw new Error('Unsupported pack object type');
    // Node returns both the inflated buffer and the zlib engine when info is enabled.
    const { buffer: data, engine } = inflateSync(pack.subarray(pos, end), {
      info: true, maxOutputLength: Math.max(1, length), chunkSize: Math.min(16_384, Math.max(64, length + 1)),
    }) as unknown as { buffer: Buffer; engine: { bytesWritten: number } };
    pos += engine.bytesWritten;
    if (data.length !== length) throw new Error('Pack entry size differs');
    if (base === undefined) add(offset, formats[type] as Kind, data, pos);
    else pending.push({ offset, base, data });
  }
  if (pos !== end) throw new Error('Trailing bytes in pack');
  while (pending.length) {
    let resolved = 0;
    for (const entry of pending) {
      const base = typeof entry.base === 'number' ? offsets.get(entry.base) : objects.get(entry.base) ?? existing.get(entry.base);
      if (!base) { pending[resolved++] = entry; continue; }
      const data = applyDelta(base.data, entry.data);
      inflated += data.length;
      if (inflated > MAX_INFLATED_PACK) throw new Error('Pack exceeds delta expansion limit');
      add(entry.offset, base.kind, data);
    }
    if (resolved === pending.length) throw new Error('Missing or cyclic delta bases');
    pending.length = resolved;
  }
  return objects;
}

function packEntry(object: ObjectData): Buffer {
  if (object.packed) return object.packed;
  let size = object.data.length;
  const header = [(formats.indexOf(object.kind) << 4) | (size & 15)];
  size = Math.floor(size / 16);
  while (size) {
    header[header.length - 1] |= 128;
    header.push(size & 127);
    size = Math.floor(size / 128);
  }
  return object.packed = Buffer.concat([Buffer.from(header), deflateSync(object.data, { level: 1 })]);
}

export function writePack(objects: ObjectData[]): Buffer {
  const entries = objects.map(packEntry);
  let end = 12;
  for (const entry of entries) end += entry.length;
  const pack = Buffer.allocUnsafe(end + 20);
  pack.write('PACK', 0, 'ascii');
  pack.writeUInt32BE(2, 4);
  pack.writeUInt32BE(objects.length, 8);
  let offset = 12;
  for (const entry of entries) offset += entry.copy(pack, offset);
  sha1(pack.subarray(0, end)).copy(pack, end);
  return pack;
}
