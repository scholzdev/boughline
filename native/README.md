# Native client and services

`bough` is the client, API server, regional cache and node daemon in one binary.
Install the client with `go install github.com/scholzdev/boughline/native/cmd/bough@main`
or build it from this checkout.
There is no local source-history database and no local Git repository in a native
workspace. Edits are normal files; publication, ancestry, merges, rebases, history,
blame and derived indices run on the server.

## What lives on a worker

Anthropic's article says its client maintains a bounded object cache and lays out
working-copy files on disk. Boughline uses the same distinction: repository
history, changesets and source-control computations live on the server; cached
objects and checked-out or edited files can occupy the worker's SSD.

- Metadata/API-only jobs need no source checkout.
- An editable sparse checkout stores the selected files locally. `commit` uploads
  changed/new bytes and asks the server to create an immutable changeset. `update`
  fetches changes explicitly and protects local edits.
- A Linux virtual snapshot fetches file contents only when read, through bounded
  client/node caches. It is currently read-only and pins its revision. It is not
  an editable virtual filesystem, and does not promise zero worker disk use.

No worker maintains a full repository/history database. There is no Git push:
creating a changeset already publishes it, and `land` separately advances `main`.

## Workspace commands

Flags precede positional arguments. `BOUGH_TOKEN` is read from the environment
and is never saved in workspace state. Through the Forgejo gateway it is a
Forgejo PAT; on a standalone native API it is a native token.

```sh
bough clone --forgejo https://forge.example.com --owner scholzdev --repo project --sparse src checkout
cd checkout
bough status
bough watch
bough add src/new.go
bough commit -m 'A change'
bough land
bough update --revision main
bough recover
```

`commit` includes modifications and deletions of tracked files, plus files named
by `add`. `add` tracks names, rather than staging a separate byte snapshot.
`.boughignore` supports shell globs matched against complete relative paths or
individual path components. Blank lines and `#` comments are ignored. It does not
implement Git's negation or `**` rules. Tracked files remain tracked when ignored.

A failed publication retains a stable request ID in `.bough/state.json`; retry
`commit` to resume. An update first downloads and verifies all replacements,
writes a recovery journal, and preserves old tracked inodes under `.bough/backups`.
It refuses dirty files, untracked collisions and symlink ancestors. Interrupted
updates require `recover`. Recovery refuses later edits or damaged backups before
restoring anything. Backups remain available for inspection; remove old backups
only after checking their contents, particularly when an editor held a file open.

`watch` is an optional per-workspace filesystem monitor. It places a barrier in
the event queue before returning cached status, rechecks dirty paths, and performs
a full scan after state changes, unfamiliar paths or event loss. Ordinary status
falls back to a full scan if the watcher is absent, unavailable or slow.

## Operations without a checkout

Set `BOUGH_SERVER`, `BOUGH_REPO`, and `BOUGH_TOKEN`, or pass
`--forgejo URL --owner OWNER --repo NAME` on any remote client command.

```sh
bough show --revision REVISION
bough diff --base BASE --revision TARGET
bough merge --base LEFT --revision RIGHT -m 'Merge'
bough rebase --revision WORK --onto main
bough history --revision main src/main.go
bough blame --revision main src/main.go
bough search --revision main main.go
bough copies --revision main src/main.go
bough review --revision REVISION --approve=true
bough land --revision REVISION
```

Remote results are JSON. Copy lookup returns other current paths with identical
content; it does not infer partially edited copies. Path search uses a persistent
trigram index, accepts at least three characters, and filters results by path IAM.
Blame, search and copy indices are lazy recursive derivations; their first query
can take longer on a large imported history. Prewarm a large import on the server
with `bough derive --repo NATIVE_ID --kind all` before opening latency-sensitive
queries. This administrator job uses SQL/S3 credentials, persists each completed
value, and skips completed work when restarted. It runs outside API timeouts.

`commit-api --file request.json` publishes changes from uploaded blob IDs, without
a working copy. Authenticated JSON endpoints are under `/v1/repos/ID/`:
`info`, `refs/NAME`, `changes/ID`, `checkout`, `browse`, `blobs`, `commits`, `diff`,
`merge-base`, `merge`, `rebase`, `land`, `history`, `blame`, `search`, `copies`,
`review`, `reviews/ID`, `tokens`, `principals`, and `policies`.

Changesets support up to 64 parents, 10,000 changed paths, 16 KiB messages, and
256 MiB files. Paths are UTF-8, case collisions are refused at checkout, and `.git`,
`.bough`, colon, backslash, control bytes and traversal components are reserved.
These are explicit Boughline protocol limits, not claims about unpublished Scion
limits. Web file previews and individual textual diffs are limited to 1 MiB;
combined web patches are limited to 4 MiB.

## Import Git history

Create an empty native project. Use a repository administrator's PAT and retain
the checkpoint file:

```sh
bough import-git --forgejo https://forge.example.com --owner scholzdev --repo project \
  --source /path/to/existing/git-repo --checkpoint import.json --git-ref refs/heads/main
```

The importer reads all local commits and preserves the parent DAG, parent order,
file bytes, executable/symlink modes, author and committer metadata, and original
Git commit objects. Each source commit has a deterministic idempotency key. The
checkpoint records native commit IDs and original ref names. The selected branch
maps to `main`; other branches and tags map to `git-<SHA256-of-original-ref-name>`.
Annotated tags map to their target commit; their annotation/signature objects are
not migrated. The source repository is not changed. Missing partial-clone objects,
submodules, unsupported paths, or objects exceeding native limits fail explicitly.
An import cannot overwrite an existing nonempty `main`; importing is a cutover,
not a bidirectional Git synchronization service.

## Read-only virtual snapshots and warming

On Linux with `/dev/fuse` available:

```sh
bough mount --revision REVISION --sparse src /mnt/snapshot
bough snapshot-create --revision REVISION --out snapshot.tar
bough daemon --parent https://cache.example.com --socket /run/boughline/daemon.sock \
  --cache /var/cache/boughline --warm-snapshot snapshot.tar
```

Mounting fetches directory metadata as accessed; file bodies are fetched on open
and read through the node daemon. A mount pins its revision. Snapshot archives
contain verified packed objects, not authority to read them: current signed
checkout capabilities still govern every shared-cache request. Refresh snapshots
by writing a new archive to a new path and atomically replacing the configured
file; the daemon checks for changes each minute.

## Policies and grants

`policy --file policies.json` requires a repository administrator. Example:

```json
{"policies":[{"prefix":"security","approvals":1,"reviewers":["forge-user-2"],"allow_self":false,"comment_only_exempt":false}]}
```

Forgejo identities are stable `forge-user-<numeric user ID>` names. A native
`grant --file grant.json` can narrow an identity's directory rights. Example:

```json
{"name":"forge-user-2","admin":false,"rules":[{"prefix":"","read":true,"write":false},{"prefix":"private","read":false,"write":false}]}
```

Approvals attest the reviewer's read scope at approval time. Landing checks that
scope and current explicit native grants for each protected path, and checks
unlanded path-history authors for self-approval. Reviewer-grant changes during
landing cause the CAS to retry. Review attestations are private metadata.

Longest-prefix rules win. A write grant must also allow reading. An explicit
native grant replaces the default repository-wide Forgejo grant, and the gateway
caps it by current Forgejo write/admin permissions. Native tokens cannot bypass
the gateway for Forgejo-owned native projects. Signed object leases expire within
one minute; bytes already downloaded to a user's own cache cannot be recalled.

The comment-only exemption is conservative and currently recognizes Go source
comments with unchanged parsed code, excluding compiler/build directives. Other
languages require ordinary approvals.
