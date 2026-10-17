# Boughline

Self-hosted, server-centric source control with a thin `bough` client and a Forgejo
fork for accounts, project management and native source review.

Boughline implements the architecture described in [Anthropic's Scion design
notes](https://www.anthropic.com/engineering/redesigning-source-control): delta
changesets, independently published commits, recursive derived data, sparse and
virtual checkouts, distributed immutable storage, and a strict cache hierarchy.
It is an independent implementation. The article does not publish Scion's wire
protocol or implementation, so Boughline does not claim binary compatibility.
No Facebook implementation code is used.

Source history lives in Boughline's native storage. Forgejo's Git database is not
used for native projects. Ordinary Git projects continue to use Forgejo normally.

```text
bough metadata commands ──HTTPS──> Forgejo identity/permission gateway ──> native API
bough source objects ──Unix socket──> node daemon ──> regional cache ──> native API
                                                                    ├── CockroachDB
                                                                    └── S3-compatible objects
```

## Use it

Follow [self-hosting](deploy/boughline/README.md) to run your instance, then create
a native project through Forgejo's API by adding `"boughline": true` to the
ordinary repository-creation request.

```sh
export BOUGH_TOKEN=YOUR_FORGEJO_PERSONAL_ACCESS_TOKEN
export BOUGH_DAEMON_SOCKET=/run/boughline/daemon.sock

bough clone --forgejo https://forge.example.com --owner scholzdev --repo my-project project
cd project
bough watch                 # optional; leave running in another terminal
# Edit files as usual.
bough add new-file.go       # tracked edits and deletions are included automatically
bough commit -m 'Implement the change'
bough land
bough update
```

Commits are published immutably without moving a shared ref. `land` separately
merges with the current head, checks directory permissions and review policies,
then advances the ref using compare-and-swap. Conflicts preserve the saved work
and are reported to the client. Review approvals bind exact immutable revisions.

The native source page provides browsing, textual diffs, path history, blame,
path search, approval/retraction, and landing. Native projects reject Git HTTP
and SSH source operations. Their reviews use native policies; Forgejo's Git pull
request engine, Actions runner, branch protections and Git release machinery do
not apply to native source.

## Native operations

See [client and API usage](native/README.md), [architecture and invariants](BOUGHLINE.md),
and [validation and limits](native/VALIDATION.md). The Linux FUSE view is a read-only,
lazily fetched snapshot; editable sparse workspaces work on Linux and macOS.

## Build

The native module requires Go 1.25 or later:

```sh
cd native
go test -race ./...
go build -o bough ./cmd/bough
```

The Forgejo fork uses the toolchain declared in the root `go.mod`, Node from
`.node-version`, and the existing Forgejo build:

```sh
npm ci
make frontend
TAGS='bindata sqlite sqlite_unlock_notify' make backend
```

The source tree retains Forgejo's history, licenses and attribution. Boughline
changes are GPL-3.0-or-later; existing file notices retain their original terms.
See [LICENSE](LICENSE) and [the upstream README](README.forgejo.md). This fork is
not an official Forgejo or Anthropic product.
