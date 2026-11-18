# github-core-clone-01

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/oktamind/github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/oktamind/github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone GitHub-compatible git forge: git smart HTTP (upload-pack protocol v2, receive-pack v0), REST v3, GraphQL v4 and MCP, in one Node 22 process with its own in-memory store. No UI, no outside service, no git binary.

## Run it

```sh
MAKEFUN_ENV_URL=http://localhost:8080 node --experimental-strip-types main.ts
```

The platform runs `node /bot/main.ts` on Node 22. The program binds an ephemeral port, registers it with the env at `MAKEFUN_ENV_URL`, serves the forge, then exits 0 when the env says done.

## What it serves

| Route | What it is |
| --- | --- |
| `GET /healthz` | 200 `{"ok":true}`: the env's boot probe |
| `GET /api/v3/` | 200, `X-OAuth-Scopes: repo, read:org`, a JSON API root |
| `/api/v3/...` | GitHub REST v3: repositories, branches, commits, contents, issues, comments, labels, pull requests, files, merges |
| `POST /api/graphql` | GitHub GraphQL v4: the documents `gh` and `github-mcp-server` send, plus introspection |
| `POST /mcp` | Model Context Protocol over streamable HTTP (revision 2026-07-28): initialize, tools/list, tools/call |
| `/<owner>/<repo>.git/...` | git smart HTTP: `info/refs`, `git-upload-pack` (v2), `git-receive-pack` (v0) |
| anything else | 404 `{"message":"Not Found"}` |

Deterministic by design: fixed commit timestamps, sequential ids and no wall clock or unseeded randomness in any response, so a trial's replay leaves a byte-identical state export.

---

<sub>Entered on [make.fun](https://make.fun/?ref=rrts6br6&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
