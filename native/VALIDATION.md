# Validation record

Boughline is an independent implementation of Anthropic's published design,
not a Scion-compatible binary or a proven replacement for its production service.
This record describes the native Go implementation, not the earlier Python
prototype. Validation date: 2026-10-17.

## Completed locally

- `go test -race ./...` in `native`: passed. Includes immutable publication,
  idempotency, CAS, merge/rebase/conflicts, recursive indices, Git import/resume,
  directory IAM, review scope/revocation/authorship, blob ownership, cache tier
  authentication, bounded disk caches, and workspace recovery/race regressions.
- Forgejo boundary tests passed with `-tags 'sqlite sqlite_unlock_notify'` in
  `services/boughline`, `services/repository`, `modules/setting`,
  `modules/gitrepo`, `routers/api/v1/repo`,
  `routers/private`, and `routers/web/repo`, selecting `^TestBough`.
- The native-only local launcher passed web login, native web/API creation,
  settings/collaborators, absence of Git clone links/storage, denied Git routes,
  and real client clone/add/commit/land/update. Persistent restart also passed.
- Native macOS executable, Linux amd64 executable, and complete Forgejo backend
  built successfully. Forgejo frontend built successfully with Node 24.
- A disposable cluster used three real CockroachDB 26.2.7 processes, real MinIO,
  two native API servers, two regional caches and three Unix-socket daemons.
  It passed sparse CLI editing/publication/landing/update, watcher/fallback,
  permissions on warm caches, private-blob hash rejection, review gating, cache
  failure isolation, snapshot warming, derivation/resume, independent SQL/object
  backup/restore/integrity, and SQL node kill/quorum writes/rejoin.
- That cluster ran the actual Forgejo binary: native project creation, signed
  permission gateway, clone with a read-scoped PAT, native source pages, and
  rejection of Git transport all passed.
- The graph/v2 cluster completed 128 landings with eight concurrent workers:
  30.914 seconds, 4.140 landings/second, and 401 retried ref conflicts. Every
  worker's final revision remained an ancestor of the final head and its latest
  file contents were preserved.
  The subsequent 3,000-landing fixed-delay driver failed after 1,514 landings.
  The real CLI run also failed after 1,260 landings when its bounded retry budget
  expired. Neither is a passing 3,000-landing result.

The load uses eight agents changing separate small files in one small repository,
on one Mac. It measures publication and optimistic landing under contention; it
does not simulate a large monorepo, complex conflicts, production review traffic,
wide-area latency, or independent host failures. It is not a capacity guarantee
for 3,000 daily production merges.

## Scaling regression

The original graph/v1 search needed 4,114 object reads to find the merge base of
one unlanded change against 1,024 unrelated main-history commits. Graph/v2 uses a
persistent paged ancestor index and needs 25 reads for the same test. A separate
DAG model verifies reachability and maximal merge bases. This addresses a measured
ancestry problem; it does not by itself prove workload fairness or throughput.

## Linux CI and deployment

The GitHub workflow builds the client and fork, repeats the distributed test,
exercises an actual Linux FUSE mount, and separately builds and starts the
self-host container topology. Both jobs passed on commit `5103d10211` in
[the final CI run](https://github.com/scholzdev/boughline/actions/runs/39038680049).

The native job passed race and boundary tests, frontend/backend builds, real
Linux FUSE, distributed storage/recovery checks, native web/API/client operations,
and immediate restart against retained state. Its eight-worker workload completed
128 landings in 20.562 seconds (6.225/second), with final ancestry and content
checks. These are small synthetic single-host results, not production capacity.

The container job built and started the full recipe: encrypted three-node SQL,
MinIO, native API/cache and Forgejo. The real host client cloned through a Unix
node daemon and container cache. Caddy configuration validation passed. Public
ACME issuance was not exercised. The archived report's "one Mac" limit label was
a hard-coded reporting string; that job ran on Ubuntu Linux. Future reports record
the actual OS/architecture and say "one host". Linux runtime evidence comes from
CI; the development Mac has no local Docker daemon or Linux kernel.


The FUSE view is a read-only snapshot with lazy hydration. Editable workspaces
use physical sparse files. The single-host Compose example uses TLS-authenticated
SQL and loopback backends behind Caddy; three SQL nodes on one machine do not
provide host-loss tolerance. Public ACME issuance, disaster recovery on separate
hosts, sustained large-history performance, and independent security review
remain operator validation before production cutover.

## Repeat the distributed proof

Build the current native and Forgejo binaries and provide CockroachDB and MinIO:

```sh
python3 native/scripts/integration.py \
  --cockroach /path/to/cockroach --minio /path/to/minio \
  --bough /path/to/bough --forgejo /path/to/boughforge \
  --forgejo-source "$PWD" --count 3000 --output native/integration-result.json
```

Add `--fuse` on Linux with a usable `/dev/fuse`. All services, credentials and
data are disposable; the harness cleans up its processes. A failure produces a
separate report recording completed landings and any reported failure reason. Earlier
failed load attempts remain failures and are not counted as completed runs.

For self-host installation, see [the deployment guide](../deploy/boughline/README.md).
Keep your existing source history and independently verified backups until a
staged migration has validated your own workload.

## Sustained contention limit

The 3,000-submission burst exposed starvation at a rapidly moving shared ref.
The initial fixed-delay test driver lost 150 ref races; replacing it with the
actual CLI also exhausted that client's 50-attempt randomized retry budget.
The failure reports are retained alongside this document. Saved changesets are
immutable and safe to retry; no successful throughput figure is reported for
those failed runs. The current implementation does not guarantee fair progress
for every submission during an uninterrupted maximum-rate burst. Production
capacity for the user's workload remains unproven.
