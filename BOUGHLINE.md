# Boughline

Independent native source control based on Anthropic's published Scion architecture.
This fork retains Forgejo's licenses and notices. Boughline is not Anthropic Scion
and is not an official Forgejo release.

## Required architecture

The source of requirements is [Anthropic's design notes](https://www.anthropic.com/engineering/redesigning-source-control).
The goal is feature and architecture parity with what is documented, not byte-level
compatibility with an unpublished protocol. No Facebook implementation is used.

| Requirement | Owner and acceptance proof |
| --- | --- |
| Online clients, asymmetric client/server | Server performs commits, merge, rebase, history, blame, ancestry and diffs; CLI has no repository history database. |
| Delta changesets | Immutable changeset contains first-parent path changes and compressed content references; no root-tree pointer in its canonical body. |
| Distributed immutable storage | S3-compatible objects and distributed SQL metadata; stateless API processes; no server materializes the repository. |
| Independent cheap commits | No branch/ref/global application lock during creation; transactions touch only the new changeset/request rows. |
| Refs are separate | Compare-and-swap advances a ref only after authorization, ancestry and review checks. |
| Recursive derived data | One engine persists immutable values derived from parent values; manifest, graph, path history and blame use that engine. |
| Sparse object negotiation | Client requests only selected directory trees and missing blobs by hash. No packfiles or have-list negotiation. |
| Optional working copy | All metadata and mutation operations are available via authenticated APIs. |
| Virtual checkout | Linux FUSE mount presents a snapshot and reads blob contents on demand. |
| Cache hierarchy | Client -> node daemon -> regional cache -> authoritative server. Identical object RPC; explicit role authentication prevents tier bypass/fallback. |
| Bounded shared caches | Disposable disk and memory caches; multiple jobs use a node daemon over a Unix socket. Rebuildable snapshots warm it. |
| Failure isolation | Cache failure is surfaced; daemon never falls back to authoritative servers. Requests and concurrency are bounded. |
| Repository/directory IAM | Metadata APIs check principals and path grants; object capabilities bind a revision/root and permitted paths, with bounded expiry. Hash knowledge alone grants no access. |
| Review/landing constraints | Reviews bind immutable revisions and reviewer identities; changed-path rules gate landing. No global merge queue. |
| Operational proof | Unit/fuzz tests, distributed-store integration, multiple server/cache/daemon processes, fault injection, backup/restore and repeatable load commands. |

## Explicit implementation choices

CockroachDB replaces Spanner so the system can be self-hosted; S3-compatible storage
replaces the unpublished cloud blob backend. Directories use bounded hash-trie pages, so a changed name copies only its route through the directory index.
Go is used for the server, cache,
daemon and CLI. The public article specifies none of those implementation details.
`git merge-file` is used only as a text merge algorithm, without a Git repository.

The only intentionally mutable repository state is refs, IAM and review records.
All source objects and derived values are immutable and content addressed.
Database consensus and S3 durability are delegated to those systems; “lock-free”
means no application-level shared commit lock or ref update, not an assertion that
the underlying database uses no synchronization.

## Invariants

1. Publication follows durable object storage; retries cannot create duplicate logical requests.
2. A ref update validates the exact expected head, the immutable reviewed work, the actual landing delta, and current explicit reviewer grants in one CAS transaction.
3. Tree, history and blame updates depend on the delta and parent derived values.
4. Every object reply validates content identity and authorization independently of cache hits.
5. An object capability is valid only for its repository, revision root, path rights and lifetime.
6. Daemons contact only caches; caches contact only servers. A failed tier is never bypassed.
7. Checkout never overwrites dirty or unknown files; all paths and filesystem types are validated.
8. Native writes do not invoke Forgejo's Git commit/push path or create a ref for each change.
9. Review requirements and path IAM are enforced server-side on the actual content being landed.
10. Claimed capabilities are marked tested only after their relevant proof runs successfully.

11. Introducing a blob reference requires a durable repository/principal upload claim or an authorized server-side merge result; knowing a global hash never authorizes copying bytes across repositories or path grants.
12. Metadata comparisons reject unauthorized changes without revealing their private path names.


Progress and measured limitations belong in `native/VALIDATION.md`. The earlier
Python prototype and its benchmarks are not proof for this implementation.

## Verification findings

| Finding | Invariant and regression proof |
| --- | --- |
| Flat directories made one-file updates scale with directory size. | Paged canonical trees share unrelated pages; TestPagedTreesCanonicalAndIncremental. |
| Removing every changed path could delete newly appeared untracked files. | Only old tracked inodes are displaced; no-overwrite installs and recovery snapshots preserve content; TestDisplacementPreservesRacingEditsAndUnknownFiles. |
| Recovery could mutate an early path before detecting a later corrupt snapshot. | All backups are verified before mutation; TestRecoveryChecksAllBackupsBeforeMutation. |
| History's encoded components could exceed legal source-path limits. | Internal derived keys are separate from validated source paths; TestLegalLongPathsRetainHistory. |
| Filesystem aliases produced different monitor identities. | Workspace roots are canonicalized; TestWorkspaceAliasesShareMonitorIdentity. |
| A shared client cache contaminated cache-failure tests across runs. | Integration clients own isolated disposable directories; a known uncached object is used for failure isolation. |
| Directory-wide disk-cache accounting ran on every admission. | One bounded index per owner; generation plus file locking coordinate independent writers; TestDiskCacheBoundAcrossIndependentWriters. |

Directory pruning uses the platform's directory-only unlink operation, so a
racing regular file is preserved (`TestDirectoryPruningCannotUnlinkRacingFile`).

The corresponding regression proofs are `TestHashKnowledgeCannotPublishPrivateBlobs`
and `TestRestrictedDiffDoesNotRevealPrivateNames`. Claims are stored after durable
blob upload and are included in native backups.

Review attestations record the approver's read scope. Landing counts an approval
only for paths in that scope, caps it by current explicit native grants, and
atomically guards those grant hashes. Stacking another author's commit above a
protected edit does not enable self-approval of that earlier path edit
(`TestStackedReviewsRespectScopeAuthorshipAndRevocation`). Merge blame preserves
line origins from both parents (`TestGraphMergeRebaseAndConflicts`).

A sustained burst exposed an ancestry-search scaling failure: one unlanded change
against 1,024 unrelated main-history commits required 4,114 object reads. The
versioned graph/v2 derivation adds a persistent paged ancestor set. Negative
reachability is indexed, and merge-base search walks only the ancestry outside
an indexed common frontier. The same regression now uses 25 reads. Independent
DAG closure and merge-base comparisons verify the index against a separate model
(`TestMergeBaseDoesNotWalkUnrelatedMainHistory`,
`TestReachabilityAndMergeBasesMatchIndependentDAG`). This adds bounded immutable
index pages per commit in exchange for removing history-sized landing searches.
