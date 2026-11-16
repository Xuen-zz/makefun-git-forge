import { buildSchema, graphqlSync } from './vendor/graphql.mjs';
import { branchRef } from './git-store.ts';
import { buildMerge, Forge, HttpError, pullFiles, pullTips, requireValue, type Body, type Comment, type Issue, type Pull, type Repository, type User } from './forge.ts';

const schema = buildSchema(`
  scalar DateTime
  interface Node { id: ID! }
  interface RepositoryOwner { id: ID!, login: String! }
  enum IssueState { OPEN CLOSED }
  enum IssueStateReason { COMPLETED NOT_PLANNED REOPENED DUPLICATE }
  enum PullRequestState { OPEN CLOSED MERGED }
  enum MergeableState { MERGEABLE CONFLICTING UNKNOWN }
  enum PullRequestMergeMethod { MERGE SQUASH REBASE }
  enum FileChangeType { ADDED DELETED MODIFIED RENAMED COPIED CHANGED }
  enum RepositoryPermission { ADMIN MAINTAIN WRITE TRIAGE READ }
  enum OrderDirection { ASC DESC }
  enum IssueOrderField { COMMENTS CREATED_AT UPDATED_AT }
  enum SearchType { ISSUE REPOSITORY USER DISCUSSION }
  input IssueOrder { field: IssueOrderField!, direction: OrderDirection! }
  type User implements Node & RepositoryOwner { id: ID!, login: String!, databaseId: Int }
  type Ref { name: String!, prefix: String!, target: Commit }
  type Commit { oid: ID!, message: String, messageHeadline: String }
  type PageInfo { hasNextPage: Boolean!, hasPreviousPage: Boolean!, endCursor: String, startCursor: String }
  type Repository implements Node {
    id: ID!, databaseId: Int, name: String!, nameWithOwner: String!, owner: RepositoryOwner!, description: String,
    defaultBranchRef: Ref, viewerPermission: RepositoryPermission!, hasIssuesEnabled: Boolean!, isPrivate: Boolean!,
    issue(number: Int!): Issue, issues(first: Int, after: String, last: Int, before: String, states: [IssueState!], orderBy: IssueOrder, labels: [String!]): IssueConnection!,
    pullRequest(number: Int!): PullRequest, pullRequests(first: Int, after: String, last: Int, before: String, states: [PullRequestState!], orderBy: IssueOrder, headRefName: String, baseRefName: String): PullRequestConnection!,
    ref(qualifiedName: String!): Ref
  }
  type Issue implements Node {
    id: ID!, databaseId: Int, number: Int!, title: String!, state: IssueState!, body: String!,
    closed: Boolean!, createdAt: DateTime!, updatedAt: DateTime!, closedAt: DateTime, author: User, repository: Repository!,
    comments(first: Int, after: String, last: Int, before: String): IssueCommentConnection!,
    labels(first: Int, after: String, last: Int, before: String): LabelConnection!
  }
  type PullRequest implements Node {
    id: ID!, databaseId: Int, number: Int!, title: String!, body: String!, state: PullRequestState!,
    headRefName: String!, baseRefName: String!, headRefOid: ID!, baseRefOid: ID!, mergeable: MergeableState!,
    merged: Boolean!, closed: Boolean!, createdAt: DateTime!, updatedAt: DateTime!, closedAt: DateTime, mergedAt: DateTime,
    mergeCommit: Commit, repository: Repository!, author: User,
    files(first: Int, after: String, last: Int, before: String): PullRequestChangedFileConnection!,
    comments(first: Int, after: String, last: Int, before: String): IssueCommentConnection!
  }
  type IssueComment implements Node { id: ID!, databaseId: Int, body: String!, author: User, createdAt: DateTime!, updatedAt: DateTime! }
  type Label { name: String!, color: String!, description: String }
  type PullRequestChangedFile { path: String!, additions: Int!, deletions: Int!, changeType: FileChangeType! }
  type IssueEdge { cursor: String!, node: Issue }
  type PullRequestEdge { cursor: String!, node: PullRequest }
  type IssueCommentEdge { cursor: String!, node: IssueComment }
  type PullRequestChangedFileEdge { cursor: String!, node: PullRequestChangedFile }
  type LabelEdge { cursor: String!, node: Label }
  type IssueConnection { totalCount: Int!, pageInfo: PageInfo!, nodes: [Issue!]!, edges: [IssueEdge!]! }
  type PullRequestConnection { totalCount: Int!, pageInfo: PageInfo!, nodes: [PullRequest!]!, edges: [PullRequestEdge!]! }
  type IssueCommentConnection { totalCount: Int!, pageInfo: PageInfo!, nodes: [IssueComment!]!, edges: [IssueCommentEdge!]! }
  type PullRequestChangedFileConnection { totalCount: Int!, pageInfo: PageInfo!, nodes: [PullRequestChangedFile!]!, edges: [PullRequestChangedFileEdge!]! }
  type LabelConnection { totalCount: Int!, pageInfo: PageInfo!, nodes: [Label!]!, edges: [LabelEdge!]! }
  union SearchResultItem = Repository | Issue | PullRequest | User
  type SearchResultItemEdge { cursor: String!, node: SearchResultItem }
  type SearchResultItemConnection { issueCount: Int!, repositoryCount: Int!, userCount: Int!, pageInfo: PageInfo!, nodes: [SearchResultItem!]!, edges: [SearchResultItemEdge!]! }
  type Query {
    viewer: User!, repository(owner: String!, name: String!): Repository, node(id: ID!): Node, nodes(ids: [ID!]!): [Node]!,
    search(type: SearchType!, query: String!, first: Int, after: String, last: Int, before: String): SearchResultItemConnection!
  }
  input CreateIssueInput { repositoryId: ID!, title: String!, body: String, clientMutationId: String }
  input CloseIssueInput { issueId: ID!, stateReason: IssueStateReason, clientMutationId: String }
  input ReopenIssueInput { issueId: ID!, clientMutationId: String }
  input CreatePullRequestInput { repositoryId: ID!, baseRefName: String!, headRefName: String!, title: String!, body: String, draft: Boolean, maintainerCanModify: Boolean, clientMutationId: String }
  input MergePullRequestInput { pullRequestId: ID!, mergeMethod: PullRequestMergeMethod, commitHeadline: String, commitBody: String, expectedHeadOid: GitObjectID, clientMutationId: String }
  scalar GitObjectID
  input AddCommentInput { subjectId: ID!, body: String!, clientMutationId: String }
  type CreateIssuePayload { issue: Issue, clientMutationId: String }
  type CloseIssuePayload { issue: Issue, clientMutationId: String }
  type ReopenIssuePayload { issue: Issue, clientMutationId: String }
  type CreatePullRequestPayload { pullRequest: PullRequest, clientMutationId: String }
  type MergePullRequestPayload { pullRequest: PullRequest, clientMutationId: String }
  type AddCommentPayload { commentEdge: IssueCommentEdge, clientMutationId: String }
  type Mutation {
    createIssue(input: CreateIssueInput!): CreateIssuePayload, closeIssue(input: CloseIssueInput!): CloseIssuePayload,
    reopenIssue(input: ReopenIssueInput!): ReopenIssuePayload, createPullRequest(input: CreatePullRequestInput!): CreatePullRequestPayload,
    mergePullRequest(input: MergePullRequestInput!): MergePullRequestPayload, addComment(input: AddCommentInput!): AddCommentPayload
  }
`);

type Page = { first?: number; last?: number; after?: string; before?: string };
type Order = { field: 'COMMENTS' | 'CREATED_AT' | 'UPDATED_AT'; direction: 'ASC' | 'DESC' };
type ListArgs = Page & { states?: string[]; orderBy?: Order; labels?: string[]; headRefName?: string; baseRefName?: string };

const userNode = (user: User) => ({ __typename: 'User', id: user.node_id, databaseId: user.id, login: user.login });
const commentNode = (comment: Comment) => ({
  __typename: 'IssueComment', id: comment.node_id, databaseId: comment.id, body: comment.body, author: userNode(comment.user),
  createdAt: comment.created_at, updatedAt: comment.updated_at,
});
function cursor(key: string | number) { return Buffer.from(`cursor:${key}`).toString('base64url'); }

function connection<T, U>(all: T[], args: Page, key: (value: T) => string | number, view: (value: T) => U) {
  const count = args.first ?? args.last ?? 30;
  if (count < 0 || count > 100 || (args.first != null && args.last != null)) throw new HttpError(422, 'Invalid page size');
  const indexOf = (encoded: string) => {
    const index = all.findIndex(value => cursor(key(value)) === encoded);
    if (index === -1) throw new HttpError(422, 'Invalid cursor');
    return index;
  };
  let start = args.after ? indexOf(args.after) + 1 : 0;
  let end = args.before ? indexOf(args.before) : all.length;
  end = Math.max(start, end);
  if (args.last != null) start = Math.max(start, end - count);
  else end = Math.min(end, start + count);
  const edges = all.slice(start, end).map(value => ({ cursor: cursor(key(value)), node: view(value) }));
  return {
    totalCount: all.length, nodes: edges.map(edge => edge.node), edges,
    pageInfo: { hasPreviousPage: start > 0, hasNextPage: end < all.length, startCursor: edges[0]?.cursor ?? null, endCursor: edges.at(-1)?.cursor ?? null },
  };
}

const ordered = <T extends Issue | Pull>(values: T[], by?: Order) => {
  if (by?.field === 'UPDATED_AT') values.sort((a, b) => a.updated_at.localeCompare(b.updated_at) || a.number - b.number);
  if (by?.field === 'COMMENTS') values.sort((a, b) => ('comments' in a ? a.comments : 0) - ('comments' in b ? b.comments : 0) || a.number - b.number);
  if (by?.direction !== 'ASC') values.reverse();
  return values;
};

function issueNode(repo: Repository, issue: Issue) {
  return {
    __typename: 'Issue', id: issue.node_id, databaseId: issue.id, number: issue.number, title: issue.title,
    body: issue.body, state: issue.state.toUpperCase(), closed: issue.state === 'closed', createdAt: issue.created_at,
    updatedAt: issue.updated_at, closedAt: issue.closed_at, author: userNode(issue.user), repository: () => repoNode(repo),
    comments: (args: Page) => connection(repo.comments.get(issue.number) ?? [], args, item => item.id, commentNode),
    labels: (args: Page) => connection(issue.labels, args, item => item.id, item => item),
  };
}

const pullState = (pull: Pull) => pull.merged_at ? 'MERGED' : pull.state.toUpperCase();
function pullNode(repo: Repository, pull: Pull) {
  const tips = pullTips(repo, pull);
  return {
    __typename: 'PullRequest', id: pull.node_id, databaseId: pull.id, number: pull.number, title: pull.title, body: pull.body,
    state: pullState(pull), headRefName: pull.head, baseRefName: pull.base, headRefOid: tips.head, baseRefOid: tips.base,
    merged: !!pull.merged_at, closed: pull.state === 'closed', createdAt: pull.created_at, updatedAt: pull.updated_at,
    closedAt: pull.closed_at, mergedAt: pull.merged_at, mergeCommit: pull.merge_commit_sha && { oid: pull.merge_commit_sha },
    repository: () => repoNode(repo), author: userNode(pull.user),
    mergeable: () => pull.state !== 'open' ? 'UNKNOWN' : buildMerge(repo, pull).conflict ? 'CONFLICTING' : 'MERGEABLE',
    files: (args: Page) => connection(pullFiles(repo, pull), args, f => f.filename, f => ({
      path: f.filename, additions: f.additions, deletions: f.deletions,
      changeType: f.status === 'removed' ? 'DELETED' : f.status.toUpperCase(),
    })),
    comments: (args: Page) => connection(repo.comments.get(pull.number) ?? [], args, item => item.id, commentNode),
  };
}

function repoNode(repo: Repository) {
  const ref = (qualifiedName: string) => {
    const full = qualifiedName.startsWith('refs/') ? qualifiedName : branchRef(qualifiedName);
    const oid = repo.git.refs.get(full);
    if (!oid) return null;
    const [, category, ...name] = full.split('/');
    return { name: name.join('/'), prefix: `refs/${category}/`, target: { oid } };
  };
  return {
    __typename: 'Repository', id: repo.nodeId, databaseId: repo.id, name: repo.name, nameWithOwner: repo.fullName,
    owner: userNode(repo.owner), description: repo.description, defaultBranchRef: () => ref(repo.defaultBranch),
    viewerPermission: 'ADMIN', hasIssuesEnabled: repo.hasIssues, isPrivate: repo.private,
    ref: ({ qualifiedName }: { qualifiedName: string }) => ref(qualifiedName),
    issue: ({ number }: { number: number }) => { const issue = repo.issues.get(number); return issue && issueNode(repo, issue); },
    pullRequest: ({ number }: { number: number }) => { const pull = repo.pulls.get(number); return pull && pullNode(repo, pull); },
    issues: (args: ListArgs) => {
      const issues = [...repo.issues.values()].filter(issue => (!args.states || args.states.includes(issue.state.toUpperCase())) &&
        (!args.labels || args.labels.every(label => issue.labels.some(item => item.name.toLowerCase() === label.toLowerCase()))));
      return connection(ordered(issues, args.orderBy), args, issue => issue.number, issue => issueNode(repo, issue));
    },
    pullRequests: (args: ListArgs) => {
      const pulls = [...repo.pulls.values()].filter(pull => (!args.states || args.states.includes(pullState(pull))) &&
        (!args.headRefName || args.headRefName === pull.head) && (!args.baseRefName || args.baseRefName === pull.base));
      return connection(ordered(pulls, args.orderBy), args, pull => pull.number, pull => pullNode(repo, pull));
    },
  };
}

function queryRoot(forge: Forge) {
  const entity = (id: string) => {
    if (id === forge.owner.node_id) return userNode(forge.owner);
    const item = forge.nodes.get(id);
    if (!item) return null;
    const { repo, kind, number } = item;
    if (kind === 'Repository') return repoNode(repo);
    if (kind === 'Issue') return issueNode(repo, repo.issues.get(number!)!);
    if (kind === 'PullRequest') return pullNode(repo, repo.pulls.get(number!)!);
    return commentNode(repo.comments.get(number!)!.find(comment => comment.node_id === id)!);
  };
  const target = (id: unknown, kind: string) => {
    const node = typeof id === 'string' ? forge.nodes.get(id) : undefined;
    return requireValue(node?.kind === kind ? node : undefined);
  };
  const mutation = (run: (input: Body) => object) => ({ input }: { input: Body }) => ({ ...run(input), clientMutationId: input.clientMutationId ?? null });
  const state = (next: 'open' | 'closed') => mutation(input => {
    const { repo, number } = target(input.issueId, 'Issue');
    return { issue: issueNode(repo, forge.editIssue(repo, repo.issues.get(number!)!, { state: next })) };
  });
  return {
    viewer: () => userNode(forge.owner),
    repository: ({ owner, name }: { owner: string; name: string }) => { const repo = forge.find(owner, name); return repo ? repoNode(repo) : null; },
    node: ({ id }: { id: string }) => entity(id), nodes: ({ ids }: { ids: string[] }) => ids.map(entity),
    search: (args: Page & { type: string; query: string }) => {
      const query = args.query.toLowerCase();
      const repoName = /(?:^| )repo:([^ ]+)/.exec(query)?.[1];
      const state = /(?:^| )(?:state|is):(open|closed|merged)(?: |$)/.exec(query)?.[1];
      const terms = query.split(/\s+/).filter(term => term && !term.includes(':'));
      const matches = (text: string) => terms.every(term => text.toLowerCase().includes(term));
      const hits = [];
      for (const repo of forge.repos.values()) {
        if (repoName && repo.fullName.toLowerCase() !== repoName) continue;
        if (args.type === 'REPOSITORY' && matches(`${repo.fullName} ${repo.description}`)) hits.push(repoNode(repo));
        if (args.type !== 'ISSUE') continue;
        if (!/\bis:pr\b/.test(query)) for (const issue of repo.issues.values()) {
          if ((!state || state === issue.state) && matches(`${issue.title} ${issue.body}`)) hits.push(issueNode(repo, issue));
        }
        if (!/\bis:issue\b/.test(query)) for (const pull of repo.pulls.values()) {
          if ((!state || state.toUpperCase() === pullState(pull)) && matches(`${pull.title} ${pull.body}`)) hits.push(pullNode(repo, pull));
        }
      }
      if (args.type === 'USER' && matches(forge.owner.login)) hits.push(userNode(forge.owner));
      return { ...connection(hits, args, node => node.id, node => node), issueCount: args.type === 'ISSUE' ? hits.length : 0,
        repositoryCount: args.type === 'REPOSITORY' ? hits.length : 0, userCount: args.type === 'USER' ? hits.length : 0 };
    },
    createIssue: mutation(input => {
      const { repo } = target(input.repositoryId, 'Repository');
      return { issue: issueNode(repo, forge.createIssue(repo, input)) };
    }),
    closeIssue: state('closed'), reopenIssue: state('open'),
    createPullRequest: mutation(input => {
      const { repo } = target(input.repositoryId, 'Repository');
      return { pullRequest: pullNode(repo, forge.createPull(repo, { ...input, head: input.headRefName, base: input.baseRefName })) };
    }),
    mergePullRequest: mutation(input => {
      const { repo, number } = target(input.pullRequestId, 'PullRequest');
      const pull = repo.pulls.get(number!)!;
      forge.merge(repo, pull, { merge_method: (input.mergeMethod as string | undefined)?.toLowerCase(), sha: input.expectedHeadOid,
        commit_title: input.commitHeadline, commit_message: input.commitBody });
      return { pullRequest: pullNode(repo, pull) };
    }),
    addComment: mutation(input => {
      const item = forge.nodes.get(String(input.subjectId));
      if (!item || (item.kind !== 'Issue' && item.kind !== 'PullRequest')) throw new HttpError(404, 'Not Found');
      const comment = forge.comment(item.repo, item.number!, input);
      return { commentEdge: { cursor: cursor(comment.id), node: commentNode(comment) } };
    }),
  };
}

export function createGraphQL(forge: Forge) {
  const rootValue = queryRoot(forge);
  const execute = (payload: unknown) => {
    const request = payload as Body | null;
    if (!request || typeof request.query !== 'string') return { errors: [{ message: 'A GraphQL query is required' }] };
    const { variables, operationName } = request;
    if (variables != null && (typeof variables !== 'object' || Array.isArray(variables))) return { errors: [{ message: 'GraphQL variables must be an object' }] };
    if (operationName != null && typeof operationName !== 'string') return { errors: [{ message: 'GraphQL operationName must be a string' }] };
    return graphqlSync({ schema, rootValue, source: request.query, variableValues: variables as Body | null | undefined, operationName });
  };
  return (data: unknown) => {
    if (!Array.isArray(data)) return execute(data);
    if (data.length === 0 || data.length > 50) throw new HttpError(400, 'Expected 1 to 50 GraphQL operations');
    return data.map(execute);
  };
}
