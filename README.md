# Git Forge

A standalone Git forge for the [make.fun GitHub Core bounty](https://make.fun/bounty/github-core-clone-01). One Node.js process serves Git smart HTTP, REST v3, GraphQL and MCP from a shared in-memory store.

## Run locally

Use Node.js 22.18 or later in the Node 22 series. Start the forge with `node main.ts`. It prints its loopback port. Set `FORGE_LOGIN` to choose the repository owner and `FORGE_TOKEN` to require authentication in standalone mode. The default login is `adaiversen`, the account used by the competition kit. Standalone mode with an empty token allows unauthenticated requests on loopback.

```sh
export FORGE_LOGIN=forge
export FORGE_TOKEN=local-token
node main.ts
```

In another shell, replace `PORT` with the printed port.

```sh
curl -H 'Authorization: Bearer local-token' -H 'Content-Type: application/json' \
  -d '{"name":"example","auto_init":true}' http://127.0.0.1:PORT/api/v3/user/repos
git -c http.extraHeader='Authorization: Bearer local-token' \
  clone http://127.0.0.1:PORT/forge/example.git
```

## Endpoints

- Git smart HTTP at `/{owner}/{repo}.git`, including upload-pack v2 and v0, shallow history, incremental fetch, tags, receive-pack and thin-pack decoding.
- REST v3 at `/api/v3`, including repository metadata, branches, commits, contents, labels, issues, comments and pull requests.
- GraphQL at `/api/graphql`, including introspection, global node IDs, cursor pagination and issue, comment and pull request mutations.
- Streamable HTTP MCP at `/mcp`, with six repository, issue and pull request tools.

The store has one configured owner and lives for the lifetime of the process. Pull requests support branches in the same repository, a squash or merge commit, and conflict detection.

## Run the competition suite

Run from the unpacked official kit. In competition mode `main.ts` binds an ephemeral port, registers with `MAKEFUN_ENV_URL` and exits when the suite reports completion. The kit supplies the bearer token and deterministic clock origin.

```sh
node cli/makefun.mjs grade-local --kit . --bot /absolute/path/to/forge --driver docker --instances 3
```

Use `--driver process` to run the same practice checks and replay on the host when Docker is unavailable. Timings depend on the host and isolation mode.

## License

MIT. GraphQL execution uses a vendored build of [graphql-js 16.15.0](https://github.com/graphql/graphql-js) under its [MIT license](vendor/graphql.LICENSE). The vendored ES module exports `buildSchema` and `graphqlSync`. Starting the forge requires no package installation.
