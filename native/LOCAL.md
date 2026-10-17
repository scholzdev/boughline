# Test locally without Docker

`scripts/local.py` starts a persistent development instance on macOS or Linux:
one CockroachDB node, MinIO, native API, regional cache, node daemon and the
Forgejo fork. Everything listens on loopback. This is an insecure local development
topology; use [the Linux deployment guide](../deploy/boughline/README.md) for a
server accessible over the network.

Prerequisites: Python 3, Git, built `bough` and Forgejo binaries, built Forgejo
frontend, and CockroachDB/MinIO binaries for your platform. Build Boughline as
described in [the root README](../README.md). The tested dependency versions are
CockroachDB 26.2.7 and MinIO source commit `7aac2a2c5b7c`.

From the repository root, with your executable paths:

```sh
python3 native/scripts/local.py \
  --bough /path/to/bough --forgejo /path/to/boughforge \
  --cockroach /path/to/cockroach --minio /path/to/minio
```

Leave this terminal open. Browse to
[http://127.0.0.1:3000/owner/playground](http://127.0.0.1:3000/owner/playground).
The launcher creates an `owner` account and native `playground` project. In another
terminal, from the same repository root:

```sh
cat local/boughline/login.txt        # web username/password
source local/boughline/client.env    # private local PAT and daemon socket
export PATH="/path/to/directory-containing-bough:$PATH"

bough clone --forgejo http://127.0.0.1:3000 --owner owner --repo playground local/playground
cd local/playground
printf 'My first native change\n' > demo.txt
bough add demo.txt
bough commit -m 'Try Boughline'
bough land
bough update
bough status
```

Reload the source page to see the landed file. For a second change, edit it,
commit, then open `?revision=COMMIT_ID&view=diff` on the project home before landing
to inspect its changes against `main`.

Ctrl-C in the launcher terminal stops all its services. Run the same command to
restart; repository data, accounts, credentials and daemon socket identity are
retained. The private `local/` directory is excluded from Git and Docker build
contexts. Keep its credentials private. Use `--state /absolute/private/directory`
to store state elsewhere; use that directory for the `source` and login commands.
Backend ports are 26257, 8081, 9000, 9001, 8712 and 8713; the launcher refuses busy
ports. `--web-port PORT` changes only the web port, and existing workspaces should
keep their original web origin.

Add `--check` to exercise actual clone/add/commit/land/update commands and then
stop the services. This check writes a small test change to `playground` and
retains the state. It was run successfully on macOS, including a restart.
