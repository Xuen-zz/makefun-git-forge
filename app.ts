import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { timingSafeEqual } from 'node:crypto';
import { gunzipSync } from 'node:zlib';
import { advertise, receive, upload } from './git-http.ts';
import { Forge, HttpError, requireValue } from './forge.ts';
import { createGraphQL } from './graphql.ts';
import { handleMcp, rpcError } from './mcp.ts';
import { pathParts, rest, sendJson } from './rest.ts';

const MAX_BODY = 64 * 1024 * 1024;
const MAX_JSON = 48 * 1024 * 1024;
const EMPTY = Buffer.alloc(0);

function bodyOf(req: IncomingMessage, limit: number): Promise<Buffer> {
  const declared = Number(req.headers['content-length']);
  if (declared === 0 || (!req.headers['content-length'] && !req.headers['transfer-encoding'])) return Promise.resolve(EMPTY);
  if (declared > limit) { req.resume(); return Promise.reject(new HttpError(413, 'Request is too large')); }
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let size = 0;
    req.on('data', (chunk: Buffer) => {
      size += chunk.length;
      if (size <= limit) chunks.push(chunk);
      else reject(new HttpError(413, 'Request is too large'));
    });
    req.on('end', () => resolve(chunks.length === 1 ? chunks[0] : Buffer.concat(chunks)));
    req.on('error', reject);
    req.on('aborted', () => reject(new HttpError(400, 'Request was interrupted')));
  });
}

function requestUrl(req: IncomingMessage): URL {
  try { return new URL(req.url ?? '/', `http://${req.headers.host ?? 'localhost'}`); }
  catch { throw new HttpError(400, 'Invalid URL'); }
}

function credentialMatches(expected: Buffer, header: string | undefined, git: boolean): boolean {
  if (!expected.length) return true;
  if (!header) return false;
  let key: Buffer;
  if (header.startsWith('Bearer ')) key = Buffer.from(header.slice(7));
  else if (header.startsWith('token ')) key = Buffer.from(header.slice(6));
  else if (git && header.startsWith('Basic ')) {
    const pair = Buffer.from(header.slice(6), 'base64');
    const split = pair.indexOf(58);
    if (split < 0) return false;
    key = pair.subarray(split + 1);
  } else return false;
  return key.length === expected.length && timingSafeEqual(key, expected);
}

export function createApp(options: { token?: string; login?: string; epoch?: string } = {}) {
  const token = Buffer.from(options.token ?? '');
  const forge = new Forge(options.login ?? 'adaiversen', options.epoch ?? '2000-01-01T00:00:00Z');
  const graphql = createGraphQL(forge);
  let sequence = 0;
  const server = createServer((req, res) => {
    const fail = (error: unknown) => {
      if (res.destroyed || res.writableEnded) return;
      if (res.headersSent) { res.destroy(); return; }
      if (error instanceof HttpError) sendJson(res, error.status, { message: error.message });
      else {
        console.error(error);
        sendJson(res, 500, { message: 'Internal Server Error' });
      }
    };

    const dispatch = () => {
      const method = req.method ?? 'GET';
      const url = requestUrl(req);
      const path = url.pathname;
      if (method === 'GET' && path === '/healthz') return sendJson(res, 200, { ok: true });
      const isRest = path === '/api/v3' || path.startsWith('/api/v3/');
      const isGraphQL = path === '/api/graphql';
      const isMcp = path === '/mcp';
      const git = isRest || isGraphQL || isMcp ? null : /^\/([^/]+)\/([^/]+)\.git\/(info\/refs|git-receive-pack|git-upload-pack)$/.exec(path);
      if (!isGraphQL && !isMcp && !git) res.setHeader('X-GitHub-Request-Id', (++sequence).toString(16));
      if (!isRest && !isGraphQL && !isMcp && !git) throw new HttpError(404, 'Not Found');
      if (!credentialMatches(token, req.headers.authorization, !!git)) {
        req.resume();
        if (git) res.setHeader('WWW-Authenticate', 'Basic realm="Git forge"');
        throw new HttpError(401, 'Bad credentials');
      }
      if (git) return gitResponse(req, res, url, git, forge);
      if (isRest && (method === 'GET' || method === 'HEAD')) return rest(forge, method, url, res, {});
      if (isGraphQL && method !== 'POST') throw new HttpError(405, 'Method Not Allowed');
      if (isMcp && method !== 'POST') {
        if (method === 'DELETE') return res.writeHead(204).end();
        res.setHeader('Allow', 'POST, DELETE');
        throw new HttpError(405, 'Method Not Allowed');
      }
      return bodyOf(req, MAX_JSON).then(bytes => {
        let body: unknown;
        try { body = bytes.length ? JSON.parse(bytes.toString()) : {}; }
        catch {
          if (isMcp) return sendJson(res, 400, rpcError(null, -32700, 'Parse error'));
          throw new HttpError(400, 'Malformed JSON');
        }
        if (isGraphQL) return sendJson(res, 200, graphql(body));
        if (isMcp) {
          const reply = handleMcp(forge, body, url.origin);
          if (reply === undefined) return res.writeHead(202).end();
          return sendJson(res, 200, reply);
        }
        return rest(forge, method, url, res, body);
      });
    };
    try {
      const result = dispatch();
      if (result instanceof Promise) result.catch(fail);
    } catch (error) { fail(error); }
  });
  server.keepAliveTimeout = 20_000;
  return server;
}

function gitResponse(req: IncomingMessage, res: ServerResponse, url: URL, match: RegExpExecArray, forge: Forge) {
  const [owner, name] = pathParts(`/${match[1]}/${match[2]}`);
  const repo = requireValue(forge.find(owner, name));
  const route = match[3];
  const discover = route === 'info/refs';
  const service = discover ? url.searchParams.get('service') : route;
  if ((service !== 'git-receive-pack' && service !== 'git-upload-pack') || (discover ? req.method !== 'GET' : req.method !== 'POST')) {
    throw new HttpError(404, 'Not Found');
  }
  const isReceive = service === 'git-receive-pack';
  const protocol = req.headers['git-protocol'];
  const v2 = typeof protocol === 'string' && protocol.split(':').includes('version=2');
  const answer = (bytes: Buffer) => {
    res.writeHead(200, { 'Content-Type': `application/x-${service}-${discover ? 'advertisement' : 'result'}`,
      'Cache-Control': 'no-cache', 'Content-Length': String(bytes.length) });
    res.end(bytes);
  };
  if (discover) return answer(advertise(repo, isReceive, v2));
  return bodyOf(req, MAX_BODY).then(raw => {
    try {
      const bytes = req.headers['content-encoding'] === 'gzip' ? gunzipSync(raw, { maxOutputLength: MAX_BODY }) : raw;
      return answer(isReceive ? receive(repo, bytes) : upload(repo, bytes, v2));
    } catch (error) {
      throw new HttpError(400, error instanceof Error ? error.message : 'Invalid Git request');
    }
  });
}
