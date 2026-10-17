# Self-host Boughline

This deployment runs the Forgejo fork, two native API processes, two regional
cache processes, MinIO, three CockroachDB nodes, and Caddy on one Linux server.
Only Caddy's HTTP/HTTPS ports are public. Backend HTTP stays on loopback; SQL
uses certificate authentication and encryption. Node daemons run on client/CI
machines and connect to the cache hostname over HTTPS.

Use a Linux server with Docker Engine, the Compose plugin, Python 3, persistent
disk, and DNS records for two hostnames pointing to the server. Start with 8 GiB
RAM and size disk for repository history plus independent backups. Three SQL
processes on one server do not survive loss of that server; place consensus nodes
and object-store replicas on separate hosts when that is required.

CockroachDB and MinIO are separate software with their own licenses. Supply a
CockroachDB license if its current terms require one for your use. Boughline does
not obtain or accept a commercial license for you. This binary serves only native projects. Import existing Git history into a new
native project; existing Git data is never automatically converted or deleted.

## Initialize and start

From a checkout of this repository:

```sh
python3 deploy/boughline/init.py --domain forge.example.com \
  --cache-domain cache.example.com --email you@example.com
cd deploy/boughline
docker compose up -d --build
docker compose logs bootstrap forge
```

Initialization generates fresh per-role secrets, a SQL CA and client certificates,
and an instance configuration under `state/`. It refuses to replace existing
state. Keep `.env`, the signing seed, the SQL CA private key, and Forgejo's secret
configuration backed up. They are excluded from Git and Docker build contexts.
Caches receive the public signing key and tier keys, without the signing seed,
S3 credentials or SQL credentials.

Caddy obtains HTTPS certificates for the two hostnames. Allow inbound 80/443 and
keep backend, SQL, and MinIO ports inaccessible from outside the host. Do not
publish those container ports separately. This Compose file uses Linux host
networking; the loopback topology is intentional.

Create the first account, then sign in and change its generated password:

```sh
docker compose exec --user git forge forgejo admin user create \
  --config /data/gitea/conf/app.ini --username scholzdev \
  --email you@example.com --admin --random-password --must-change-password=true
```

Create a Forgejo PAT in account settings. Read-only use needs `read:repository`;
commits, reviews and landings need `write:repository`. Project creation also needs
the corresponding user/organization scope. Use that PAT to create a native project:

```sh
curl --fail-with-body -X POST https://forge.example.com/api/v1/user/repos \
  -H "Authorization: token $BOUGH_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"my-project","private":true}'
```

The organization repository-creation API also creates native projects automatically.
Projects start empty on `main`; Git auto-initialization options are
rejected. If the backend was temporarily unavailable during creation, the project
remains native and blocks Git writes. Retry
`POST /api/v1/repos/OWNER/NAME/bough/initialize` as its administrator.

## Install a node daemon and client

Build `native/cmd/bough` for each client platform, or use your own verified CI
artifact. Install the binary at `/usr/local/bin/bough` on a Linux worker. Use one
unprivileged service identity distinct from job identities, with a client socket
group:

```sh
sudo groupadd --system bough-clients
sudo useradd --system --no-create-home --gid bough-clients bough
sudo install -d -m 0700 /etc/boughline
sudo install -m 0600 /secure/path/to/daemon.env /etc/boughline/daemon.env
sudo install -m 0644 bough-daemon.service /etc/systemd/system/bough-daemon.service
sudo usermod -aG bough-clients YOUR_LOGIN
sudo systemctl daemon-reload
sudo systemctl enable --now bough-daemon
```

Copy only the generated `state/daemon.env` to the worker over your administrative
channel. It contains the public signing key, the daemon-to-cache key and the cache
HTTPS origin. Treat tier keys as service credentials: the daemon is trusted to
retrieve objects, while jobs receive only repository credentials and Unix-socket
access. Log in again after changing group membership.

Then:

```sh
export BOUGH_TOKEN=YOUR_FORGEJO_PAT
export BOUGH_DAEMON_SOCKET=/run/boughline/daemon.sock
bough clone --forgejo https://forge.example.com --owner scholzdev --repo my-project project
```

For a personal macOS client, run the daemon as your own account with a short,
private Unix-socket path and a writable cache directory. Linux FUSE mounting is
separate; physical sparse workspaces work on macOS as well.

## Back up and restore

Native backup creates a single independent archive containing one consistent SQL
snapshot and all verified immutable source/derived objects. It does not depend on
the source bucket being available during restore.

```sh
# From deploy/boughline on the server:
docker compose run --rm -v "$PWD/backups:/backup" api1 backup --file /backup/native-2026-10-17.tar
```

Create `backups/` owned by your configured UID first. Treat the archive as private
source data and copy it to independent storage. Also back up `.env`, certificates,
and the Forgejo data/configuration. To capture the Forgejo SQLite database and
repository identity mappings coherently, stop the `forge` service, copy its data
and configuration, and restart it. A native-only restore does not recreate Forgejo
accounts or project IDs. For a coordinated instance snapshot, stop both API
services and Forgejo while taking the two backups; node/cache services may remain.

Restore into a newly initialized, empty native database and an empty independent
S3 bucket, with API services stopped:

```sh
bough migrate
bough init-store
bough restore --file /backup/native-2026-10-17.tar
bough verify
```

These administrator commands read `BOUGH_SQL_URL` and the `BOUGH_S3_*` environment
variables; they do not use a user PAT. Restore verifies every referenced source
and derived object before publishing metadata. It refuses existing native rows
or a nonempty bucket. A failed restore may leave unreferenced objects in its new
bucket; use a fresh target when retrying. Restore the matching Forgejo metadata
and signing keys before reopening the gateway.

## Operate and extend

`/healthz` identifies each native tier. Authenticated native `info` and `metrics`
endpoints expose server status and object counts; regional cache `/metrics`
requires its incoming tier key. Counters include requests, cache hits/misses,
upstream failures, and rejected requests. Cache content is disposable and bounded
by bytes and object count. APIs are stateless and can be restarted independently.

For multiple hosts, use TLS on every non-loopback native/cache listener, distribute
only the secrets needed by each role, and use a CA trusted by clients. Pass
`--tls-cert`, `--tls-key`, and `BOUGH_CA_FILE`; use normal S3 TLS credentials and
SQL `sslmode=verify-full`. Point caches at API load-balancers and daemons at cache
load-balancers. There is no automatic fallback across cache tiers. Same-tier
replicas can be balanced by your proxy.

The native runtime and the real multi-process topology are validated separately
from these container recipes. Check [the validation record](../../native/VALIDATION.md)
for exactly which deployment, failure and load checks have run.
