import type { ServerResponse } from 'node:http';
import { asCommit, branchRef } from './git-store.ts';
import { contents, createRef, Forge, HttpError, optionalString, pullFiles, pullView, requireValue, requiredString, updateContents, type Body } from './forge.ts';

export const JSON_TYPE = 'application/json; charset=utf-8';

export function sendJson(res: ServerResponse, status: number, value: unknown, headers?: Record<string, string>) {
  sendJsonText(res, status, JSON.stringify(value), headers);
}

function sendJsonText(res: ServerResponse, status: number, text: string, headers?: Record<string, string>) {
  res.writeHead(status, { 'Content-Type': JSON_TYPE, 'Content-Length': String(Buffer.byteLength(text)), ...headers });
  res.end(text);
}

export function pathParts(path: string): string[] {
  try { return path.replace(/^\//, '').replace(/\/$/, '').split('/').map(decodeURIComponent); }
  catch { throw new HttpError(400, 'Invalid URL encoding'); }
}

function positive(value: string | null, fallback: number) {
  const number = Number(value);
  return Number.isSafeInteger(number) && number > 0 ? number : fallback;
}

function paginate<T>(res: ServerResponse, url: URL, items: readonly T[], transform: (item: T) => unknown = item => item) {
  const size = Math.min(100, positive(url.searchParams.get('per_page'), 30));
  const page = positive(url.searchParams.get('page'), 1);
  const last = Math.ceil(items.length / size);
  const links: string[] = [];
  const link = (n: number, rel: string) => {
    const target = new URL(url);
    target.searchParams.set('page', String(n));
    links.push(`<${target.href}>; rel="${rel}"`);
  };
  if (page < last) { link(page + 1, 'next'); link(last, 'last'); }
  if (page > 1 && last > 0) { link(1, 'first'); link(Math.min(page - 1, last), 'prev'); }
  sendJson(res, 200, items.slice((page - 1) * size, page * size).map(transform), links.length ? { Link: links.join(', ') } : undefined);
}

export function filterState<T extends { state: string }>(records: Iterable<T>, state: string) {
  return [...records].filter(record => state === 'all' || record.state === state);
}

const bodyObject = (data: unknown): Body => {
  if (data === null || typeof data !== 'object' || Array.isArray(data)) throw new HttpError(400, 'Expected a JSON object');
  return data as Body;
};

function identity(value: string) {
  const parsed = /^(.*?) <([^>]*)> (-?\d+) [+-]\d{4}$/.exec(value);
  if (!parsed) return { name: value, email: '', date: null };
  const date = new Date(Number(parsed[3]) * 1000);
  return { name: parsed[1], email: parsed[2], date: Number.isFinite(date.getTime()) ? date.toISOString() : null };
}

export function rest(forge: Forge, method: string, url: URL, res: ServerResponse, data: unknown) {
  const path = url.pathname;
  const input = bodyObject(data);
  const [group, owner, name, section, ...tail] = pathParts(path.slice(7));
  const is = (m: string) => method === m;
  const respond = (status: number, value: unknown) => sendJson(res, status, value);
  if (!group && is('GET')) return sendJson(res, 200, {
    current_user_url: `${url.origin}/api/v3/user`, current_user_repositories_url: `${url.origin}/api/v3/user/repos`,
    repository_url: `${url.origin}/api/v3/repos/{owner}/{repo}`,
  }, { 'X-OAuth-Scopes': 'repo, read:org' });
  if (group === 'meta' && !owner && is('GET')) return respond(200, { installed_version: '1.0.0' });
  if (group === 'user' && !owner && is('GET')) return respond(200, forge.owner);
  if (group === 'user' && owner === 'repos' && !name) {
    if (is('GET')) return paginate(res, url, [...forge.repos.values()], repo => repo.view(url.origin));
    if (is('POST')) return respond(201, forge.createRepo(input).view(url.origin));
  }
  if (group !== 'repos' || !owner || !name) throw new HttpError(404, 'Not Found');
  const repo = requireValue(forge.find(owner, name));
  const query = url.searchParams;
  if (!section) {
    if (is('GET')) return respond(200, repo.view(url.origin));
    if (is('DELETE')) {
      forge.deleteRepo(repo);
      res.writeHead(204, { 'Content-Type': JSON_TYPE });
      return res.end();
    }
    if (is('PATCH')) {
      if (typeof input.default_branch === 'string' && !repo.git.refs.has(branchRef(input.default_branch))) throw new HttpError(422, 'Invalid default branch');
      repo.description = optionalString(input.description, repo.description);
      if (typeof input.private === 'boolean') repo.private = input.private;
      if (typeof input.has_issues === 'boolean') repo.hasIssues = input.has_issues;
      if (typeof input.default_branch === 'string') repo.defaultBranch = input.default_branch;
      return respond(200, repo.view(url.origin));
    }
  }
  if (section === 'branches' && is('GET')) {
    const view = ([ref, sha]: [string, string]) => ({ name: ref.slice(11), commit: { sha }, protected: false });
    if (tail.length) {
      const ref = branchRef(tail.join('/'));
      return respond(200, view([ref, requireValue(repo.git.refs.get(ref))]));
    }
    return paginate(res, url, [...repo.git.refs].filter(([ref]) => ref.startsWith('refs/heads/')), view);
  }
  if (section === 'commits' && is('GET')) {
    const ref = tail.length ? tail.join('/') : query.get('sha') ?? repo.defaultBranch;
    const tip = repo.git.resolve(ref);
    const view = (sha: string) => {
      const commit = asCommit(repo.git.objects.get(sha))!;
      return { sha, commit: { message: commit.message.trimEnd(), tree: { sha: commit.tree }, author: identity(commit.author), committer: identity(commit.committer) }, parents: commit.parents.map(sha => ({ sha })) };
    };
    if (tail.length) return respond(200, view(requireValue(tip)));
    if (!tip && query.has('sha')) throw new HttpError(404, 'Not Found');
    return paginate(res, url, tip ? [...repo.git.history(tip)] : [], view);
  }
  if (section === 'git' && tail[0] === 'refs' && tail.length === 1 && is('POST')) {
    return respond(201, createRef(repo, input));
  }
  if (section === 'git' && tail[0] === 'ref' && tail.length > 1 && is('GET')) {
    const ref = `refs/${tail.slice(1).join('/')}`;
    const sha = requireValue(repo.git.refs.get(ref));
    return respond(200, { ref, object: { sha, type: repo.git.objects.get(sha)?.kind } });
  }
  if (section === 'contents') {
    const file = tail.join('/');
    if (is('GET')) return respond(200, contents(repo, file, query.get('ref') ?? repo.defaultBranch));
    if (is('PUT') || is('DELETE')) {
      const result = updateContents(forge, repo, file, input, is('DELETE'));
      return respond(result.status, result.data);
    }
  }
  if (section === 'labels' && !tail.length) {
    if (is('POST')) return respond(201, forge.createLabel(repo, input));
    if (is('GET')) return paginate(res, url, [...repo.labels.values()]);
  }
  if (section === 'issues') {
    if (!tail.length) {
      if (is('POST')) return respond(201, forge.createIssue(repo, input));
      if (is('GET')) {
        let issues = filterState(repo.issues.values(), query.get('state') ?? 'open');
        if (query.has('labels')) {
          const labels = query.get('labels')!.split(',').map(label => label.toLowerCase());
          issues = issues.filter(issue => labels.every(label => issue.labels.some(current => current.name.toLowerCase() === label)));
        }
        if (query.get('sort') === 'updated') issues.sort((a, b) => a.updated_at.localeCompare(b.updated_at) || a.number - b.number);
        if (query.get('direction') !== 'asc') issues.reverse();
        return paginate(res, url, issues);
      }
    } else {
      const number = positive(tail[0], 0);
      const issue = repo.issues.get(number);
      if (tail.length === 1 && is('GET')) {
        if (!issue) {
          const pull = requireValue(repo.pulls.get(number));
          return respond(200, { ...pullView(repo, pull), pull_request: { url: `${repo.apiPath}/pulls/${number}` } });
        }
        let encoded = repo.issueJson.get(number);
        if (encoded === undefined) { encoded = JSON.stringify(issue); repo.issueJson.set(number, encoded); }
        return sendJsonText(res, 200, encoded);
      }
      if (tail.length === 1 && is('PATCH')) return respond(200, forge.editIssue(repo, requireValue(issue), input));
      if (tail.length === 2 && tail[1] === 'comments') {
        if (is('POST')) return respond(201, forge.comment(repo, number, input));
        if (is('GET') && (issue || repo.pulls.has(number))) return paginate(res, url, repo.comments.get(number) ?? []);
      }
    }
  }
  if (section === 'pulls') {
    if (!tail.length) {
      if (is('POST')) return respond(201, pullView(repo, forge.createPull(repo, input)));
      if (is('GET')) {
        const filtered = filterState(repo.pulls.values(), query.get('state') ?? 'open').filter(pull => {
          const head = query.get('head'), base = query.get('base');
          return (!head || pull.head === head || `${owner}:${pull.head}` === head) && (!base || pull.base === base);
        });
        if (query.get('direction') !== 'asc') filtered.reverse();
        return paginate(res, url, filtered, pull => pullView(repo, pull));
      }
    } else {
      const pull = requireValue(repo.pulls.get(positive(tail[0], 0)));
      if (tail.length === 1 && is('GET')) return respond(200, pullView(repo, pull));
      if (tail.length === 1 && is('PATCH')) {
        if (input.state !== undefined && input.state !== 'open' && input.state !== 'closed') throw new HttpError(422, 'Invalid pull request state');
        if (pull.merged_at && input.state === 'open') throw new HttpError(422, 'Pull request is merged');
        const title = input.title === undefined ? pull.title : requiredString(input.title, 'title');
        const date = forge.now();
        if (input.state === 'open' || input.state === 'closed') {
          if (pull.state !== input.state) pull.closed_at = input.state === 'closed' ? date : null;
          pull.state = input.state;
        }
        Object.assign(pull, { title, body: optionalString(input.body, pull.body), updated_at: date });
        return respond(200, pullView(repo, pull));
      }
      if (tail.length === 2 && tail[1] === 'files' && is('GET')) return paginate(res, url, pullFiles(repo, pull));
      if (tail.length === 2 && tail[1] === 'merge') {
        if (is('PUT')) return respond(200, forge.merge(repo, pull, input));
        if (is('GET') && pull.merged_at) return res.writeHead(204, { 'Content-Type': JSON_TYPE }).end();
      }
    }
  }
  throw new HttpError(404, 'Not Found');
}
