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
  `services/boughline`, `modules/gitrepo`, `routers/api/v1/repo`,
  `routers/private`, and `routers/web/repo`, selecting `^TestBough`.
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
  file contents were preserved. A 3,000-landing run is pending completion.

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
self-host container topology. These CI jobs have not yet run. Locally, deployment
YAML and Python/shell syntax were inspected; there is no available Docker daemon
or Linux kernel here. No container or FUSE runtime success is claimed yet.

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
separate report recording completed landings and conflict reasons. Earlier
failed load attempts remain failures and are not counted as completed runs.

For self-host installation, see [the deployment guide](../deploy/boughline/README.md).
Keep your existing source history and independently verified backups until a
staged migration has validated your own workload.
