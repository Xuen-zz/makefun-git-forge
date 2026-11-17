import { asCommit, asTree, branchRef, GitStore, isOid, validPath, validRef, type Entry } from './git-store.ts';
import { mergeText } from './merge-text.ts';

export type Body = Record<string, unknown>;
export type User = { id: number; login: string; node_id: string; type: 'User' };
export type Label = { id: number; name: string; color: string; description: string | null };
export type Comment = { id: number; node_id: string; body: string; user: User; created_at: string; updated_at: string };
type RecordState = 'open' | 'closed';
export type Issue = {
  id: number; node_id: string; number: number; title: string; body: string; state: RecordState; labels: Label[];
  user: User; created_at: string; updated_at: string; closed_at: string | null; comments: number;
};
export type Pull = {
  id: number; node_id: string; number: number; title: string; body: string; state: RecordState; user: User;
  head: string; base: string; headAtCreation: string; baseAtCreation: string;
  created_at: string; updated_at: string; closed_at: string | null; merged_at: string | null; merge_commit_sha: string | null;
};

export class HttpError extends Error {
  status: number;
  constructor(status: number, message: string) { super(message); this.status = status; }
}

export function requireValue<T>(value: T | undefined | null): T {
  if (value == null) throw new HttpError(404, 'Not Found');
  return value;
}

export function requiredString(value: unknown, field: string): string {
  if (typeof value !== 'string' || !value.trim()) throw new HttpError(422, `${field} is required`);
  return value;
}

export const optionalString = (value: unknown, fallback = '') => typeof value === 'string' ? value : fallback;
const nodeId = (kind: string, id: number) => Buffer.from(`${kind}:${id}`).toString('base64url');

export class Repository {
  id: number;
  name: string;
  private: boolean;
  description: string;
  owner: User;
  defaultBranch = 'main';
  hasIssues = true;
  nextNumber = 1;
  createdAt: string;
  readonly git = new GitStore();
  readonly issues = new Map<number, Issue>();
  readonly pulls = new Map<number, Pull>();
  readonly labels = new Map<string, Label>();
  readonly comments = new Map<number, Comment[]>();
  readonly issueJson = new Map<number, { text: string; bytes: number }>();

  constructor(id: number, name: string, owner: User, data: Body, now: string) {
    this.id = id;
    this.name = name;
    this.owner = owner;
    this.private = data.private === true;
    this.description = optionalString(data.description);
    this.hasIssues = data.has_issues !== false;
    this.createdAt = now;
  }

  get fullName() { return `${this.owner.login}/${this.name}`; }
  get apiPath() { return `/api/v3/repos/${this.owner.login}/${this.name}`; }
  get nodeId() { return nodeId('Repository', this.id); }

  view(origin = '') {
    return {
      id: this.id, node_id: this.nodeId, name: this.name, full_name: this.fullName, owner: this.owner,
      private: this.private, visibility: this.private ? 'private' : 'public', description: this.description,
      default_branch: this.defaultBranch, has_issues: this.hasIssues, fork: false, archived: false,
      created_at: this.createdAt, url: `${origin}${this.apiPath}`, clone_url: `${origin}/${this.fullName}.git`,
      permissions: { admin: true, maintain: true, push: true, triage: true, pull: true },
    };
  }
}

export class Forge {
  readonly owner: User;
  private readonly ownerKey: string;
  readonly repos = new Map<string, Repository>();
  readonly nodes = new Map<string, { repo: Repository; kind: 'Repository' | 'Issue' | 'PullRequest' | 'IssueComment'; number?: number }>();
  private globalId = 1001;
  private epoch: number;
  private ticks = 0;

  constructor(login: string, time: string) {
    this.owner = { id: 1, node_id: nodeId('User', 1), login, type: 'User' };
    this.ownerKey = login.toLowerCase();
    const parsed = Date.parse(time);
    this.epoch = Number.isFinite(parsed) ? parsed : 0;
  }

  now(): string { return new Date(this.epoch + this.ticks++ * 1000).toISOString(); }

  find(owner: string, name: string): Repository | undefined {
    return owner.toLowerCase() === this.ownerKey ? this.repos.get(name.toLowerCase()) : undefined;
  }

  createRepo(body: Body): Repository {
    const name = requiredString(body.name, 'name');
    if (!/^[a-zA-Z0-9_.-]{1,100}$/.test(name) || name === '.' || name === '..' || name.toLowerCase().endsWith('.git')) {
      throw new HttpError(422, 'Invalid repository name');
    }
    const key = name.toLowerCase();
    if (this.repos.has(key)) throw new HttpError(422, 'Repository already exists');
    const repo = new Repository(this.globalId++, name, this.owner, body, this.now());
    if (body.auto_init === true) {
      const files = new Map([['README.md', { oid: repo.git.put('blob', Buffer.from(`# ${name}\n`)), mode: '100644' }]]);
      this.commit(repo, repo.defaultBranch, files, [], 'Initial commit');
    }
    this.repos.set(key, repo);
    this.nodes.set(repo.nodeId, { repo, kind: 'Repository' });
    return repo;
  }

  deleteRepo(repo: Repository) {
    this.repos.delete(repo.name.toLowerCase());
    for (const [id, value] of this.nodes) if (value.repo === repo) this.nodes.delete(id);
  }

  commit(repo: Repository, branch: string, files: Map<string, Entry>, parents: string[], message: string): string {
    const epoch = Math.floor(Date.parse(this.now()) / 1000);
    const sha = repo.git.writeCommit(files, parents, message, `${this.owner.login} <${this.owner.login}@users.noreply.github.com> ${epoch} +0000`);
    repo.git.setRef(branchRef(branch), sha);
    return sha;
  }

  createLabel(repo: Repository, body: Body): Label {
    const name = requiredString(body.name, 'name');
    const color = optionalString(body.color, 'ededed');
    if (!/^[0-9a-fA-F]{6}$/.test(color) || repo.labels.has(name.toLowerCase())) throw new HttpError(422, 'Invalid or duplicate label');
    const label = { id: this.globalId++, name, color, description: typeof body.description === 'string' ? body.description : null };
    repo.labels.set(name.toLowerCase(), label);
    return label;
  }

  issueLabels(repo: Repository, input: unknown): Label[] {
    if (input === undefined) return [];
    if (!Array.isArray(input)) throw new HttpError(422, 'labels must be a list');
    return [...new Set(input.map(name => {
      const label = typeof name === 'string' ? repo.labels.get(name.toLowerCase()) : undefined;
      if (!label) throw new HttpError(422, `Label does not exist: ${String(name)}`);
      return label;
    }))];
  }

  createIssue(repo: Repository, body: Body): Issue {
    if (!repo.hasIssues) throw new HttpError(410, 'Issues are disabled');
    const title = requiredString(body.title, 'title');
    const labels = this.issueLabels(repo, body.labels);
    const date = this.now();
    const id = this.globalId++;
    const issue: Issue = {
      id, node_id: nodeId('Issue', id), number: repo.nextNumber++, title, body: optionalString(body.body), state: 'open', labels,
      user: this.owner, created_at: date, updated_at: date, closed_at: null, comments: 0,
    };
    repo.issues.set(issue.number, issue);
    this.nodes.set(issue.node_id, { repo, kind: 'Issue', number: issue.number });
    return issue;
  }

  editIssue(repo: Repository, issue: Issue, body: Body): Issue {
    const title = body.title === undefined ? issue.title : requiredString(body.title, 'title');
    const labels = body.labels === undefined ? issue.labels : this.issueLabels(repo, body.labels);
    if (body.state !== undefined && body.state !== 'open' && body.state !== 'closed') throw new HttpError(422, 'Unknown issue state');
    const date = this.now();
    if (body.state === 'open' || body.state === 'closed') {
      if (body.state !== issue.state) issue.closed_at = body.state === 'closed' ? date : null;
      issue.state = body.state;
    }
    Object.assign(issue, { title, labels, body: optionalString(body.body, issue.body), updated_at: date });
    repo.issueJson.delete(issue.number);
    return issue;
  }

  comment(repo: Repository, number: number, data: Body): Comment {
    const issue = repo.issues.get(number);
    const pull = repo.pulls.get(number);
    if (!issue && !pull) throw new HttpError(404, 'Not Found');
    const body = requiredString(data.body, 'body');
    const id = this.globalId++;
    const date = this.now();
    const comment = { id, node_id: nodeId('IssueComment', id), user: this.owner, body, created_at: date, updated_at: date };
    const comments = repo.comments.get(number) ?? [];
    comments.push(comment);
    repo.comments.set(number, comments);
    this.nodes.set(comment.node_id, { repo, kind: 'IssueComment', number });
    if (issue) { issue.comments++; issue.updated_at = date; repo.issueJson.delete(number); }
    return comment;
  }

  createPull(repo: Repository, body: Body): Pull {
    const title = requiredString(body.title, 'title');
    let head = requiredString(body.head, 'head');
    const base = requiredString(body.base, 'base');
    const separator = head.indexOf(':');
    if (separator !== -1 && head.slice(0, separator).toLowerCase() === repo.owner.login.toLowerCase()) head = head.slice(separator + 1);
    const headAtCreation = repo.git.refs.get(branchRef(head));
    const baseAtCreation = repo.git.refs.get(branchRef(base));
    if (!headAtCreation || !baseAtCreation || head === base || headAtCreation === baseAtCreation) throw new HttpError(422, 'Invalid pull request branches or no commits between them');
    for (const pull of repo.pulls.values()) {
      if (pull.state === 'open' && pull.head === head && pull.base === base) throw new HttpError(422, 'Pull request already exists');
    }
    const id = this.globalId++;
    const date = this.now();
    const pull: Pull = {
      id, node_id: nodeId('PullRequest', id), number: repo.nextNumber++, title, body: optionalString(body.body), state: 'open', user: this.owner,
      head, base, headAtCreation, baseAtCreation, created_at: date, updated_at: date, closed_at: null, merged_at: null, merge_commit_sha: null,
    };
    repo.pulls.set(pull.number, pull);
    this.nodes.set(pull.node_id, { repo, kind: 'PullRequest', number: pull.number });
    return pull;
  }

  merge(repo: Repository, pull: Pull, input: Body) {
    if (pull.state !== 'open') throw new HttpError(405, 'Pull request is closed');
    const { head, base, files, mergedFiles, conflict } = buildMerge(repo, pull);
    if (input.sha !== undefined && input.sha !== head) throw new HttpError(409, 'Head branch was modified');
    if (conflict || !base || !head) throw new HttpError(405, 'Pull request is not mergeable');
    const method = optionalString(input.merge_method, 'squash');
    if (method !== 'squash' && method !== 'merge') throw new HttpError(422, 'Supported merge methods are squash and merge');
    const title = optionalString(input.commit_title, `${pull.title} (#${pull.number})`);
    const message = optionalString(input.commit_message, pull.body);
    for (const [path, content] of mergedFiles) files.get(path)!.oid = repo.git.put('blob', content);
    const sha = this.commit(repo, pull.base, files, method === 'merge' ? [base, head] : [base], message ? `${title}\n\n${message}` : title);
    const date = this.now();
    Object.assign(pull, { state: 'closed', merged_at: date, closed_at: date, updated_at: date, merge_commit_sha: sha, headAtCreation: head, baseAtCreation: base });
    return { sha, merged: true, message: 'Pull Request successfully merged' };
  }
}

export function pullTips(repo: Repository, pull: Pull) {
  return {
    head: (pull.merged_at ? null : repo.git.refs.get(branchRef(pull.head))) ?? pull.headAtCreation,
    base: (pull.merged_at ? null : repo.git.refs.get(branchRef(pull.base))) ?? pull.baseAtCreation,
  };
}

function commonBase(git: GitStore, base: string, head: string): string | undefined {
  const ancestors = new Set(git.history(base));
  for (const oid of git.history(head)) if (ancestors.has(oid)) return oid;
}

const sameEntry = (a?: Entry, b?: Entry) => a?.oid === b?.oid && a?.mode === b?.mode;

function pullTrees(repo: Repository, pull: Pull) {
  const { base, head } = pullTips(repo, pull);
  const ancestor = commonBase(repo.git, base, head);
  return { base, head, before: repo.git.files(ancestor), after: repo.git.files(head) };
}

function* differences(before: Map<string, Entry>, after: Map<string, Entry>) {
  for (const path of new Set([...before.keys(), ...after.keys()])) {
    const old = before.get(path), next = after.get(path);
    if (!sameEntry(old, next)) yield { path, old, next };
  }
}

export function buildMerge(repo: Repository, pull: Pull) {
  const { base, head, before, after } = pullTrees(repo, pull);
  const files = repo.git.files(base);
  const mergedFiles = new Map<string, Buffer>();
  let conflict = !repo.git.refs.has(branchRef(pull.head)) || !repo.git.refs.has(branchRef(pull.base));
  for (const { path, old, next } of differences(before, after)) {
    const current = files.get(path);
    if (!sameEntry(current, old) && !sameEntry(current, next)) {
      const merged = mergeEntries(repo.git, old, current, next);
      if (!merged) { conflict = true; continue; }
      files.set(path, { mode: merged.mode, oid: merged.oid });
      if (merged.data) mergedFiles.set(path, merged.data);
      continue;
    }
    if (next) files.set(path, next);
    else files.delete(path);
  }
  for (const path of files.keys()) {
    for (let i = path.indexOf('/'); i >= 0; i = path.indexOf('/', i + 1)) {
      if (files.has(path.slice(0, i))) conflict = true;
    }
  }
  return { base, head, files, mergedFiles, conflict };
}

function mergeEntries(git: GitStore, before?: Entry, ours?: Entry, theirs?: Entry): (Entry & { data?: Buffer }) | null {
  if (!before || !ours || !theirs) return null;
  const mode = ours.mode === before.mode ? theirs.mode : theirs.mode === before.mode || theirs.mode === ours.mode ? ours.mode : null;
  if (!mode) return null;
  if (ours.oid === before.oid) return { mode, oid: theirs.oid };
  if (theirs.oid === before.oid || ours.oid === theirs.oid) return { mode, oid: ours.oid };
  if (![before.mode, ours.mode, theirs.mode].every(value => value === '100644' || value === '100755')) return null;
  const data = mergeText(git.objects.get(before.oid)!.data, git.objects.get(ours.oid)!.data, git.objects.get(theirs.oid)!.data);
  return data ? { mode, oid: ours.oid, data } : null;
}

function lineChanges(before: Buffer, after: Buffer) {
  if (before.equals(after) || before.includes(0) || after.includes(0)) return { additions: 0, deletions: 0 };
  const lines = (bytes: Buffer) => bytes.toString().match(/[^\n]*\n|[^\n]+$/g) ?? [];
  const a = lines(before), b = lines(after);
  let start = 0, aEnd = a.length, bEnd = b.length;
  while (start < aEnd && start < bEnd && a[start] === b[start]) start++;
  while (start < aEnd && start < bEnd && a[aEnd - 1] === b[bEnd - 1]) { aEnd--; bEnd--; }
  const width = bEnd - start;
  let common = 0;
  if (width * (aEnd - start) <= 2_000_000) {
    const row = new Uint32Array(width + 1);
    for (let i = start; i < aEnd; i++) {
      let diagonal = 0;
      for (let j = 1; j <= width; j++) {
        const above = row[j];
        row[j] = a[i] === b[start + j - 1] ? diagonal + 1 : Math.max(above, row[j - 1]);
        diagonal = above;
      }
    }
    common = row[width];
  }
  return { additions: width - common, deletions: aEnd - start - common };
}

export function pullFiles(repo: Repository, pull: Pull) {
  const { before, after } = pullTrees(repo, pull);
  const empty = Buffer.alloc(0);
  return [...differences(before, after)].map(({ path, old, next }) => {
    const read = (entry?: Entry) => (entry && repo.git.objects.get(entry.oid)?.data) || empty;
    const { additions, deletions } = lineChanges(read(old), read(next));
    return { filename: path, sha: next?.oid ?? old!.oid, status: !old ? 'added' : !next ? 'removed' : 'modified', additions, deletions, changes: additions + deletions };
  });
}

export function pullView(repo: Repository, pull: Pull) {
  const { head, base } = pullTips(repo, pull);
  return {
    id: pull.id, node_id: pull.node_id, number: pull.number, title: pull.title, body: pull.body, state: pull.state, user: pull.user,
    created_at: pull.created_at, updated_at: pull.updated_at, closed_at: pull.closed_at, merged_at: pull.merged_at,
    merged: pull.merged_at !== null, merge_commit_sha: pull.merge_commit_sha,
    head: { ref: pull.head, sha: head, label: `${repo.owner.login}:${pull.head}` },
    base: { ref: pull.base, sha: base, label: `${repo.owner.login}:${pull.base}` },
  };
}

export function contents(repo: Repository, path: string, ref: string) {
  const sha = requireValue(repo.git.resolve(ref));
  let oid = asCommit(repo.git.objects.get(sha))!.tree;
  let mode = '40000';
  if (path) {
    if (!validPath(path)) throw new HttpError(404, 'Not Found');
    for (const part of path.split('/')) {
      if (mode !== '40000' && mode !== '040000') throw new HttpError(404, 'Not Found');
      const item = requireValue(asTree(repo.git.objects.get(oid)).find(entry => entry.name === part));
      ({ oid, mode } = item);
    }
  }
  const object = repo.git.objects.get(oid);
  if (object?.kind === 'tree') return asTree(object).map(entry => contentInfo(repo, path ? `${path}/${entry.name}` : entry.name, entry));
  if (object?.kind !== 'blob') throw new HttpError(404, 'Not Found');
  return { ...contentInfo(repo, path, { mode, oid }), encoding: 'base64', content: object.data.toString('base64') };
}

function contentInfo(repo: Repository, path: string, entry: Entry) {
  const type = entry.mode === '40000' || entry.mode === '040000' ? 'dir' : entry.mode === '160000' ? 'submodule' : entry.mode === '120000' ? 'symlink' : 'file';
  const url = `${repo.apiPath}/contents/${path.split('/').map(encodeURIComponent).join('/')}`;
  return { name: path.split('/').at(-1), path, sha: entry.oid, type, size: type === 'dir' ? 0 : (repo.git.objects.get(entry.oid)?.data.length ?? 0), url };
}

export function updateContents(forge: Forge, repo: Repository, path: string, input: Body, remove: boolean) {
  if (!path || !validPath(path) || path.length > 4096 || path.split('/').length > 256) throw new HttpError(422, 'Invalid path');
  const branch = optionalString(input.branch, repo.defaultBranch);
  if (!validRef(branchRef(branch))) throw new HttpError(422, 'Invalid branch');
  const parent = repo.git.refs.get(branchRef(branch));
  if (!parent && (branch !== repo.defaultBranch || repo.git.refs.size)) throw new HttpError(404, 'Branch not found');
  const files = repo.git.files(parent);
  const old = files.get(path);
  if (remove && !old) throw new HttpError(404, 'Not Found');
  if (input.sha !== old?.oid) throw new HttpError(409, 'SHA does not match current file');
  const message = requiredString(input.message, 'message');
  if (remove) files.delete(path);
  else {
    for (const existing of files.keys()) {
      if (existing.startsWith(`${path}/`) || path.startsWith(`${existing}/`)) throw new HttpError(409, 'File and directory paths overlap');
    }
    if (typeof input.content !== 'string') throw new HttpError(422, 'content is required');
    const encoded = input.content.replace(/\s/g, '');
    if (!/^[A-Za-z0-9+/]*={0,2}$/.test(encoded) || encoded.length % 4 === 1) throw new HttpError(422, 'content is not base64');
    files.set(path, { mode: old?.mode ?? '100644', oid: repo.git.put('blob', Buffer.from(encoded, 'base64')) });
  }
  const sha = forge.commit(repo, branch, files, parent ? [parent] : [], message);
  const commit = { sha, message };
  return { status: old ? 200 : 201, data: { commit, content: remove ? null : contents(repo, path, branch) } };
}

export function createRef(repo: Repository, input: Body) {
  const { ref, sha } = input;
  if (!validRef(ref) || !isOid(sha) || !repo.git.objects.has(sha) || (ref.startsWith('refs/heads/') && !asCommit(repo.git.objects.get(sha)))) {
    throw new HttpError(422, 'Invalid ref or SHA');
  }
  for (const existing of repo.git.refs.keys()) {
    if (existing === ref || existing.startsWith(`${ref}/`) || ref.startsWith(`${existing}/`)) throw new HttpError(422, 'Reference already exists');
  }
  repo.git.setRef(ref, sha);
  return { ref, object: { type: repo.git.objects.get(sha)!.kind, sha } };
}
