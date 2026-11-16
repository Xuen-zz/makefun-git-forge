import { Forge, HttpError, pullView, requireValue, type Body } from './forge.ts';
import { filterState } from './rest.ts';

const revisions = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];
const serverInfo = { name: 'makefun-git-forge', version: '1.0.0' };
const capabilities = { tools: { listChanged: false } };
const string = { type: 'string' };
const boolean = { type: 'boolean' };
const positiveInteger = { type: 'integer', minimum: 1 };
const location = { owner: string, repo: string };
const address = ['owner', 'repo'];

function tool(name: string, description: string, required: string[], properties: Record<string, object>) {
  return { name, description, inputSchema: { type: 'object', properties, required } };
}

const tools = [
  tool('create_repo', 'Create a repository owned by the authenticated user.', ['name'], { name: string, description: string, private: boolean, auto_init: boolean }),
  tool('get_repo', 'Read the repository metadata for an owner and name.', address, location),
  tool('create_issue', 'Open an issue in a repository.', [...address, 'title'], { ...location, title: string, body: string }),
  tool('list_issues', 'List the repository issues, optionally filtered by state.', address, { ...location, state: { type: 'string', enum: ['open', 'closed', 'all', 'OPEN', 'CLOSED', 'ALL'] }, page: positiveInteger, per_page: positiveInteger }),
  tool('create_pull_request', 'Open a pull request from an existing head branch into the base branch.', [...address, 'title', 'head', 'base'], { ...location, title: string, body: string, head: string, base: string }),
  tool('merge_pull_request', 'Squash or merge a pull request into its base branch.', [...address, 'pull_number'], { ...location, pull_number: positiveInteger, merge_method: { type: 'string', enum: ['squash', 'merge'] }, commit_title: string, commit_message: string }),
];
const toolIndex = new Map(tools.map(value => [value.name, value]));
const record = (data: unknown): data is Body => data !== null && typeof data === 'object' && !Array.isArray(data);

function validArgument(value: unknown, schema: { type: string; minimum?: number; enum?: unknown[] } | undefined): boolean {
  if (!schema) return true;
  if (schema.type === 'integer') return typeof value === 'number' && Number.isSafeInteger(value) && value >= (schema.minimum ?? 0);
  if (typeof value !== schema.type) return false;
  return !schema.enum || schema.enum.includes(value);
}

function runTool(forge: Forge, name: string, args: Body, origin: string): unknown {
  if (name === 'create_repo') return forge.createRepo(args).view(origin);
  const repo = requireValue(forge.find(args.owner as string, args.repo as string));
  switch (name) {
    case 'get_repo': return repo.view(origin);
    case 'create_issue': return forge.createIssue(repo, args);
    case 'list_issues': {
      const state = (args.state as string | undefined)?.toLowerCase() ?? 'open';
      const size = Math.min(100, (args.per_page as number | undefined) ?? 30);
      const page = (args.page as number | undefined) ?? 1;
      return filterState(repo.issues.values(), state).reverse().slice((page - 1) * size, page * size);
    }
    case 'create_pull_request': return pullView(repo, forge.createPull(repo, args));
    case 'merge_pull_request': return forge.merge(repo, requireValue(repo.pulls.get(args.pull_number as number)), args);
  }
}

export function rpcError(id: unknown, code: number, message: string) { return { jsonrpc: '2.0', id: id ?? null, error: { code, message } }; }

export function handleMcp(forge: Forge, input: unknown, origin: string): unknown {
  if (!record(input) || input.jsonrpc !== '2.0' || typeof input.method !== 'string' ||
      (input.id !== undefined && input.id !== null && typeof input.id !== 'string' && typeof input.id !== 'number')) {
    return rpcError(null, -32600, 'Invalid Request');
  }
  const { id, method } = input;
  if (id === undefined) return undefined;
  const params = record(input.params) ? input.params : {};
  const result = (value: unknown) => ({ jsonrpc: '2.0', id, result: value });

  switch (method) {
    case 'server/discover':
      return result({ supportedVersions: revisions, serverInfo, capabilities, _meta: { 'io.modelcontextprotocol/serverInfo': serverInfo } });
    case 'initialize': {
      const offered = params.protocolVersion;
      return result({ protocolVersion: typeof offered === 'string' && revisions.includes(offered) ? offered : revisions[1], serverInfo, capabilities });
    }
    case 'ping': return result({});
    case 'tools/list': return result({ tools });
    case 'tools/call': {
      const name = params.name;
      const spec = typeof name === 'string' ? toolIndex.get(name) : undefined;
      if (!spec) return rpcError(id, -32602, 'Unknown tool');
      const args = params.arguments;
      if (!record(args) || spec.inputSchema.required.some(key => !Object.hasOwn(args, key)) ||
          Object.entries(args).some(([key, value]) => !validArgument(value, spec.inputSchema.properties[key] as { type: string } | undefined))) {
        return rpcError(id, -32602, 'Invalid tool arguments');
      }
      try {
        const value = runTool(forge, spec.name, args, origin);
        return result({ content: [{ type: 'text', text: JSON.stringify(value) }], isError: false });
      } catch (error) {
        if (!(error instanceof HttpError)) throw error;
        return result({ isError: true, content: [{ type: 'text', text: error.message }] });
      }
    }
    default: return rpcError(id, -32601, 'Method not found');
  }
}
