import { asCommit, branchRef, GitStore, isOid, readPack, references, validRef, writePack, ZERO } from './git-store.ts';

const FLUSH = Buffer.from('0000');
const DELIMITER = Buffer.from('0001');
const ADVERTISE_V2 = Buffer.concat([
  ...['version 2\n', 'ls-refs=unborn\n', 'fetch=shallow\n', 'object-format=sha1\n', 'agent=makefun-forge/1.0\n'].map(packet),
  FLUSH,
]);
const AGENT = 'agent=makefun-forge/1.0';
const RECEIVE_CAPS = `report-status delete-refs side-band-64k ofs-delta object-format=sha1 ${AGENT}`;
const UPLOAD_CAPS = `shallow deepen-relative side-band-64k ofs-delta include-tag object-format=sha1 ${AGENT}`;

type Target = { git: GitStore; defaultBranch: string };
type Packets = { lines: string[]; offset: number; ended: boolean };

function packet(line: string): Buffer {
  const bytes = Buffer.byteLength(line);
  if (bytes > 65516) throw new Error('Packet exceeds line limit');
  return Buffer.from(`${(bytes + 4).toString(16).padStart(4, '0')}${line}`);
}

function sideband(body: Buffer): Buffer[] {
  const output: Buffer[] = [];
  for (let offset = 0; offset < body.length; offset += 65515) {
    const slice = body.subarray(offset, offset + 65515);
    output.push(Buffer.from(`${(slice.length + 5).toString(16).padStart(4, '0')}\x01`), slice);
  }
  return output;
}

function readPackets(body: Buffer, offset = 0): Packets {
  const lines: string[] = [];
  while (offset < body.length) {
    const header = body.toString('ascii', offset, offset + 4);
    if (!/^[0-9a-fA-F]{4}$/.test(header)) throw new Error('Invalid packet length');
    const size = parseInt(header, 16);
    if (!size) return { lines, offset: offset + 4, ended: true };
    if (size === 1) { offset += 4; continue; }
    if (size < 4 || size > 65520 || offset + size > body.length) throw new Error('Invalid packet size');
    lines.push(body.toString('utf8', offset + 4, offset + size).replace(/\n$/, ''));
    offset += size;
  }
  return { lines, offset, ended: false };
}

type Ref = { oid: string; name: string; symref?: string; peeled?: string };
function listRefs({ git, defaultBranch }: Target, receive: boolean): Ref[] {
  const head = branchRef(defaultBranch);
  const refs: Ref[] = [];
  const tip = git.refs.get(head);
  if (!receive && tip) refs.push({ oid: tip, name: 'HEAD', symref: head });
  for (const [name, oid] of git.refs) {
    const peeled = receive ? oid : git.peel(oid);
    refs.push({ oid, name, peeled: peeled !== oid ? peeled : undefined });
  }
  return refs;
}

export function advertise(target: Target, receive: boolean, v2: boolean): Buffer {
  if (!receive && v2) return ADVERTISE_V2;
  const service = receive ? 'git-receive-pack' : 'git-upload-pack';
  const head = branchRef(target.defaultBranch);
  const caps = receive ? RECEIVE_CAPS : `${UPLOAD_CAPS} symref=HEAD:${head}`;
  const refs = listRefs(target, receive);
  const parts = [packet(`# service=${service}\n`), FLUSH];
  if (!refs.length) parts.push(packet(`${ZERO} capabilities^{}\0${caps}\n`));
  for (const [i, { oid, name, peeled }] of refs.entries()) {
    parts.push(packet(`${oid} ${name}${i === 0 ? `\0${caps}` : ''}\n`));
    if (peeled) parts.push(packet(`${peeled} ${name}^{}\n`));
  }
  return Buffer.concat([...parts, FLUSH]);
}

function lsRefs(target: Target, lines: string[]): Buffer {
  const prefixes = lines.filter(line => line.startsWith('ref-prefix ')).map(line => line.slice(11));
  const matches = (name: string) => !prefixes.length || prefixes.some(prefix => name.startsWith(prefix));
  const symrefs = lines.includes('symrefs');
  const peel = lines.includes('peel');
  const head = branchRef(target.defaultBranch);
  const parts: Buffer[] = [];
  if (!target.git.refs.has(head) && lines.includes('unborn') && matches('HEAD')) {
    parts.push(packet(`unborn HEAD symref-target:${head}\n`));
  }
  for (const ref of listRefs(target, false)) {
    if (!matches(ref.name)) continue;
    const sym = symrefs && ref.symref ? ` symref-target:${ref.symref}` : '';
    const peeled = peel && ref.peeled ? ` peeled:${ref.peeled}` : '';
    parts.push(packet(`${ref.oid} ${ref.name}${sym}${peeled}\n`));
  }
  return Buffer.concat([...parts, FLUSH]);
}

type Fetch = { wants: string[]; haves: string[]; shallow: Set<string>; depth?: number; relative: boolean; includeTag: boolean; done: boolean };

function fetchArgs(lines: string[]): Fetch {
  const args: Fetch = { wants: [], haves: [], shallow: new Set(), relative: false, includeTag: false, done: false };
  for (const line of lines) {
    const [key, value] = line.split(' ', 2);
    if (key === 'want' || key === 'have' || key === 'shallow') {
      if (!isOid(value)) throw new Error(`Invalid ${key} object ID`);
      if (key === 'want') args.wants.push(value);
      else if (key === 'have') args.haves.push(value);
      else args.shallow.add(value);
    }
    if (key === 'deepen') {
      const depth = Number(value);
      if (!Number.isSafeInteger(depth) || depth < 1) throw new Error('Invalid shallow depth');
      args.depth = depth;
    }
    if (key === 'deepen-relative') args.relative = true;
    if (key === 'include-tag') args.includeTag = true;
    if (key === 'done') args.done = true;
  }
  return args;
}

function selectObjects(git: GitStore, args: Fetch) {
  // Walk history separately from trees. A have only covers ancestry down to the client's shallow boundary.
  const reachable = new Set<string>();
  const walk = (tips: Iterable<string>, seen: Set<string>, parents: (oid: string) => boolean) => {
    const stack = [...tips];
    while (stack.length) {
      const oid = stack.pop()!;
      if (seen.has(oid)) continue;
      const object = git.objects.get(oid);
      if (!object) continue;
      seen.add(oid);
      for (const child of references(object, object.kind !== 'commit' || parents(oid))) stack.push(child);
    }
  };
  const known = new Set<string>();
  walk(args.haves, known, oid => !args.shallow.has(oid));
  const unshallow = new Set<string>();
  const shallow = new Set<string>();
  if (args.depth !== undefined || args.shallow.size) {
    const limits = new Map<string, number>();
    // Each position is the smallest distance at which we have visited this commit.
    // Revisiting with more budget can turn a provisional shallow boundary into history.
    const queue: [string, number][] = args.wants.map(oid => [git.peel(oid), args.relative ? -1 : 1]);
    const depth = args.depth ?? Number.POSITIVE_INFINITY;
    for (let index = 0; index < queue.length; index++) {
      const [oid, inherited] = queue[index];
      const distance = inherited < 0 && args.shallow.has(oid) ? 0 : inherited;
      const best = limits.get(oid);
      if (best !== undefined && best <= distance) continue;
      limits.set(oid, distance);
      const commit = asCommit(git.objects.get(oid));
      if (!commit?.parents.length) continue;
      if (distance >= depth) { shallow.add(oid); continue; }
      shallow.delete(oid);
      if (args.shallow.has(oid)) unshallow.add(oid);
      for (const parent of commit.parents) queue.push([parent, distance < 0 ? -1 : distance + 1]);
    }
  }
  walk(args.wants, reachable, oid => !shallow.has(oid));
  for (const oid of args.shallow) shallow.delete(oid);
  if (args.includeTag) {
    for (const [ref, oid] of git.refs) {
      if (ref.startsWith('refs/tags/') && oid !== git.peel(oid) && reachable.has(git.peel(oid))) {
        walk([oid], reachable, parent => !shallow.has(parent));
      }
    }
  }
  const objects = [...reachable].filter(oid => !known.has(oid)).map(oid => git.objects.get(oid)!);
  return { shallow, unshallow, objects };
}

type CachedReply = { request: Buffer; response: Buffer };
type UploadCache = { version: number; branch: string; refs?: CachedReply; shallow?: CachedReply };
const uploadCache = new WeakMap<GitStore, UploadCache>();

export function upload(target: Target, body: Buffer, v2: boolean): Buffer {
  const { git } = target;
  let cache: UploadCache | undefined;
  if (v2 && body.length <= 16_384) {
    cache = uploadCache.get(git);
    if (!cache || cache.version !== git.refVersion || cache.branch !== target.defaultBranch) {
      cache = { version: git.refVersion, branch: target.defaultBranch };
      uploadCache.set(git, cache);
    }
    for (const entry of [cache.refs, cache.shallow]) if (entry?.request.equals(body)) return entry.response;
  }
  const first = readPackets(body);
  if (!first.ended) throw new Error('Missing flush packet');
  if (v2 && first.lines[0] === 'command=ls-refs') {
    const response = lsRefs(target, first.lines);
    if (cache && response.length < 262_144) cache.refs = { request: body, response };
    return response;
  }
  if (v2 && first.lines[0] !== 'command=fetch') throw new Error('Unknown upload-pack command');
  const lines = first.lines;
  const caps = v2 ? [] : (lines[0]?.split(' ').slice(2) ?? []);
  const rest = v2 ? [] : readPackets(body, first.offset).lines;
  const args = fetchArgs([...lines, ...rest]);
  args.includeTag ||= caps.includes('include-tag');
  args.relative ||= caps.includes('deepen-relative');
  const common = args.haves.filter(oid => git.objects.has(oid));
  if (!args.wants.every(oid => git.objects.has(oid))) return Buffer.concat([packet('ERR upload-pack: not our ref\n'), FLUSH]);
  if (!args.wants.length) return FLUSH;
  if (v2 && !args.done) {
    const acks = common.length ? common.map(oid => packet(`ACK ${oid}\n`)) : [packet('NAK\n')];
    return Buffer.concat([packet('acknowledgments\n'), ...acks, FLUSH]);
  }
  const { shallow, unshallow, objects } = selectObjects(git, args);
  const parts: Buffer[] = [];
  if (args.depth !== undefined || unshallow.size || shallow.size) {
    if (v2) parts.push(packet('shallow-info\n'));
    for (const oid of unshallow) parts.push(packet(`unshallow ${oid}\n`));
    for (const oid of shallow) parts.push(packet(`shallow ${oid}\n`));
    parts.push(v2 ? DELIMITER : FLUSH);
  }
  if (!v2 && !args.done) {
    if (rest.length || args.depth === undefined) parts.push(packet('NAK\n'));
    return Buffer.concat(parts);
  }
  parts.push(v2 ? packet('packfile\n') : packet(common.length ? `ACK ${common[0]}\n` : 'NAK\n'));
  const pack = writePack(objects);
  const banded = v2 || caps.includes('side-band-64k');
  if (banded) parts.push(...sideband(pack), FLUSH);
  else parts.push(pack);
  const response = Buffer.concat(parts);
  // Identical depth-one requests can share the framed pack across concurrent clones.
  if (cache && args.depth === 1 && !args.haves.length && !args.shallow.size && response.length <= 1_048_576) {
    cache.shallow = { request: body, response };
  }
  return response;
}

export function receive({ git }: Target, body: Buffer): Buffer {
  const { lines, offset, ended } = readPackets(body);
  if (!ended) throw new Error('Missing receive-pack flush');
  const commands = lines.filter(line => !line.startsWith('shallow '));
  const caps = commands[0]?.split('\0')[1]?.split(' ') ?? [];
  const parsed = commands.map(line => {
    const [old, next, ref, extra] = line.split('\0')[0].split(' ');
    if (!isOid(old) || !isOid(next) || !validRef(ref) || extra !== undefined) throw new Error('Invalid ref update');
    return { old, next, ref };
  });
  let quarantine: ReturnType<typeof readPack> = new Map();
  let unpacked = true;
  try {
    if (offset < body.length) quarantine = readPack(body.subarray(offset), git.objects);
  } catch {
    unpacked = false;
  }
  const lookup = (oid: string) => git.objects.get(oid) ?? quarantine.get(oid);
  const report = [packet(unpacked ? 'unpack ok\n' : 'unpack invalid pack\n')];
  for (const { old, next, ref } of parsed) {
    let error = '';
    const current = git.refs.get(ref) ?? ZERO;
    const closure = new Set<string>();
    if (!unpacked) error = 'unpacker error';
    else if (current !== old) error = 'stale ref';
    else if (next === ZERO) git.deleteRef(ref);
    else if (!lookup(next) || (ref.startsWith('refs/heads/') && lookup(next)?.kind !== 'commit')) error = 'missing or invalid target';
    else if (old !== ZERO && old !== next && !hasAncestor(git, next, old, lookup)) error = 'non-fast-forward';
    else if (old === ZERO && refCollides(git.refs, ref)) error = 'ref namespace collision';
    else {
      // Promote only objects connected to a successful command. Failed commands leave refs unchanged.
      try {
        const stack = [next];
        while (stack.length) {
          const oid = stack.pop()!;
          if (git.objects.has(oid) || closure.has(oid)) continue;
          const object = quarantine.get(oid);
          if (!object) throw new Error('Missing object');
          closure.add(oid);
          for (const child of references(object)) stack.push(child);
        }
        for (const oid of closure) git.objects.set(oid, quarantine.get(oid)!);
        git.setRef(ref, next);
      } catch {
        error = 'invalid object graph';
      }
    }
    report.push(packet(error ? `ng ${ref} ${error}\n` : `ok ${ref}\n`));
  }
  if (!caps.includes('report-status')) return Buffer.alloc(0);
  const inner = Buffer.concat([...report, FLUSH]);
  return caps.includes('side-band-64k') ? Buffer.concat([...sideband(inner), FLUSH]) : inner;
}

function refCollides(refs: ReadonlyMap<string, string>, ref: string) {
  for (const existing of refs.keys()) if (existing.startsWith(`${ref}/`) || ref.startsWith(`${existing}/`)) return true;
  return false;
}

function hasAncestor(git: GitStore, next: string, old: string, lookup: (oid: string) => ReturnType<GitStore['objects']['get']>) {
  for (const oid of git.history(next, lookup)) if (oid === old) return true;
  return false;
}
