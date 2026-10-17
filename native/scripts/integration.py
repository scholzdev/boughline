"""Real, disposable three-node SQL / S3 / two-server / cache / daemon proof.

Requires official CockroachDB and MinIO binaries plus a built bough executable.
No production repository or credentials are used.
"""
import argparse
import concurrent.futures
import collections
import threading
import hashlib
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request


def port():
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return listener.getsockname()[1]


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--cockroach", required=True)
    p.add_argument("--minio", required=True)
    p.add_argument("--bough", required=True)
    p.add_argument("--fuse", action="store_true")
    p.add_argument("--forgejo")
    p.add_argument("--forgejo-source")
    p.add_argument("--count", type=int, default=200)
    p.add_argument("--output")
    args = p.parse_args()
    processes, logs, checks = [], [], []
    conflicts = collections.Counter()
    progress_lock = threading.Lock()
    completed = {"count": 0}
    with tempfile.TemporaryDirectory(prefix="boughline-cluster-") as directory:
        root = Path(directory)
        executable = str(Path(args.bough).resolve())
        cockroach, minio = str(Path(args.cockroach).resolve()), str(Path(args.minio).resolve())
        env = dict(os.environ)
        generated = subprocess.check_output([executable, "keygen"], text=True)
        env.update(dict(line.split("=", 1) for line in generated.splitlines()))
        env.update(BOUGH_OWNER="scholzdev", BOUGH_S3_TLS="false", BOUGH_S3_BUCKET="boughline-test",
                   BOUGH_S3_ACCESS_KEY="integration-user", BOUGH_S3_SECRET_KEY=secrets.token_urlsafe(40),
                   COCKROACH_SKIP_ENABLING_DIAGNOSTIC_REPORTING="true")
        env["BOUGH_TOKEN"] = env["BOUGH_ADMIN_TOKEN"]
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

        def start(name, command, extra=None, cwd=None):
            log = (root / (name + ".log")).open("w+")
            logs.append((name, log))
            process = subprocess.Popen(command, cwd=cwd or root, env={**env, **(extra or {})}, stdout=log, stderr=subprocess.STDOUT)
            processes.append(process)
            return process

        def wait_http(url, timeout=40):
            end = time.monotonic() + timeout
            while time.monotonic() < end:
                try:
                    with opener.open(url, timeout=2) as response:
                        if response.status == 200:
                            return
                except (urllib.error.URLError, TimeoutError):
                    time.sleep(.1)
            raise RuntimeError("service did not become ready: " + url)

        def cli(*command, cwd=None, extra=None):
            result = subprocess.run([executable, *command], cwd=cwd, env={**env, **(extra or {})},
                                    capture_output=True, text=True, timeout=180)
            if result.returncode:
                raise RuntimeError(f"bough {command[0]} failed: {result.stderr.strip()}")
            return result.stdout.strip()

        def api(server, resource, body=None, token=None, method=None):
            route = "/v1/repos/test/" + resource
            if resource == "CREATE":
                route = "/v1/repos"
            raw = body if isinstance(body, bytes) else json.dumps(body).encode() if body is not None else None
            request = urllib.request.Request(server + route, data=raw, method=method or ("POST" if body is not None else "GET"),
                      headers={"Authorization": "Bearer " + (token or env["BOUGH_TOKEN"]),
                               "Content-Type": "application/octet-stream" if isinstance(body, bytes) else "application/json"})
            with opener.open(request, timeout=120) as response:
                return json.load(response)

        def commit(server, parent, files, key=None, token=None):
            changes = {}
            for path, data in files.items():
                if data is None:
                    changes[path] = None
                else:
                    raw = data.encode()
                    blob = api(server, "blobs", raw, token=token)["id"]
                    assert blob == hashlib.sha256(raw).hexdigest()
                    changes[path] = {"id": blob, "mode": 0o100644, "size": len(raw)}
            return api(server, "commits", {"parents": [parent], "changes": changes,
                       "message": "integration change", "request_id": key or secrets.token_hex(20)}, token=token)["id"]

        def land(server, revision):
            last_fault = None
            for attempt in range(150):
                head = api(server, "refs/main")["target"]
                try:
                    return api(server, "land", {"ref": "main", "expected": head, "revision": revision,
                               "request_id": hashlib.sha256((head + revision).encode()).hexdigest()})["id"]
                except urllib.error.HTTPError as error:
                    if error.code != 409:
                        raise
                    last_fault = json.load(error)
                    error.close()
                    reason = last_fault.get("error", "unknown conflict")
                    with progress_lock:
                        conflicts[reason] += 1
                    if reason not in ("ref changed", "review policy changed; retry landing", "reviews changed; retry landing", "reviewer permissions changed; retry landing"):
                        raise RuntimeError("non-retryable landing conflict: " + json.dumps(last_fault))
                    if attempt in (31, 63, 127):
                        print(f"landing {revision[:12]}: {attempt+1} retries; {reason}", flush=True)
                    time.sleep(.005)
            raise RuntimeError("landing exhausted 150 retries: " + json.dumps(last_fault))

        try:
            sql_processes = []
            sql_ports, web_ports = [port() for _ in range(3)], [port() for _ in range(3)]
            joined = ",".join(f"127.0.0.1:{n}" for n in sql_ports)
            for i in range(3):
                sql_processes.append(start(f"sql-{i}", [cockroach, "start", "--insecure", f"--listen-addr=127.0.0.1:{sql_ports[i]}",
                      f"--http-addr=127.0.0.1:{web_ports[i]}", f"--join={joined}", f"--store={root / ('sql-' + str(i))}",
                      "--cache=64MiB", "--max-sql-memory=64MiB"]))
            for n in web_ports:
                wait_http(f"http://127.0.0.1:{n}/health", timeout=60)
            subprocess.run([cockroach, "init", "--insecure", f"--host=127.0.0.1:{sql_ports[0]}"],
                           check=True, capture_output=True, env=env, timeout=40)
            subprocess.run([cockroach, "sql", "--insecure", f"--host=127.0.0.1:{sql_ports[0]}",
                            "--execute=CREATE DATABASE boughline;"],
                           check=True, capture_output=True, env=env, timeout=40)
            subprocess.run([cockroach, "sql", "--insecure", f"--host=127.0.0.1:{sql_ports[0]}",
                            "--execute=SET CLUSTER SETTING diagnostics.reporting.enabled = false;"],
                           check=True, capture_output=True, env=env, timeout=40)
            env["BOUGH_SQL_URL"] = f"postgresql://root@127.0.0.1:{sql_ports[0]}/boughline?sslmode=disable"
            s3_port, console_port = port(), port()
            env["BOUGH_S3_ENDPOINT"] = f"127.0.0.1:{s3_port}"
            start("s3", [minio, "server", str(root / "s3"), "--address", f"127.0.0.1:{s3_port}",
                  "--console-address", f"127.0.0.1:{console_port}"],
                  {"MINIO_ROOT_USER": env["BOUGH_S3_ACCESS_KEY"], "MINIO_ROOT_PASSWORD": env["BOUGH_S3_SECRET_KEY"]})
            wait_http(f"http://127.0.0.1:{s3_port}/minio/health/live")
            cli("migrate")
            cli("init-store")
            servers, caches, daemon_sockets, cache_processes = [], [], [], []
            for i in range(2):
                api_port, cache_port = port(), port()
                server, cache = f"http://127.0.0.1:{api_port}", f"http://127.0.0.1:{cache_port}"
                start(f"api-{i}", [executable, "server", "--listen", f"127.0.0.1:{api_port}"],
                      {"BOUGH_SQL_URL": f"postgresql://root@127.0.0.1:{sql_ports[i]}/boughline?sslmode=disable"})
                wait_http(server + "/healthz")
                cache_processes.append(start(f"cache-{i}", [executable, "cache", "--listen", f"127.0.0.1:{cache_port}",
                     "--parent", server, "--cache", str(root / f"cache-{i}")]))
                wait_http(cache + "/healthz")
                daemon_socket = str(root / f"daemon-{i}.sock")
                start(f"daemon-{i}", [executable, "daemon", "--socket", daemon_socket,
                      "--parent", cache, "--cache", str(root / f"daemon-{i}")])
                end = time.monotonic() + 15
                while not Path(daemon_socket).exists() and time.monotonic() < end:
                    time.sleep(.05)
                assert Path(daemon_socket).exists()
                servers.append(server); caches.append(cache); daemon_sockets.append(daemon_socket)
            env.update(BOUGH_SERVER=servers[0], BOUGH_REPO="test", BOUGH_DAEMON_SOCKET=daemon_sockets[0])
            genesis = api(servers[0], "CREATE", {"id": "test", "owner": "scholzdev", "policies": []})["genesis"]
            initial = commit(servers[0], genesis, {"src/main.go": "package main\n// original\nfunc F() int { return 1 }\n",
                             "private/secret": "private source", "README": "initial"})
            assert api(servers[1], "changes/" + initial)["id"] == initial
            land(servers[1], initial)
            checks.append("three-node distributed SQL; two stateless servers share newly published changes immediately")
            copy = root / "copy"
            cli("clone", "--sparse", "src", str(copy))
            assert not (copy / "private").exists()
            (copy / "src/main.go").write_text("package main\n// original\nfunc F() int { return 2 }\n")
            (copy / "src/new.go").write_text("package main\n")
            cli("add", "src/new.go", cwd=copy)
            work = cli("commit", "-m", "CLI edit", cwd=copy)
            cli("land", cwd=copy)
            cli("update", cwd=copy)
            checks.append("real CLI: sparse clone, add/edit, native commit, land, update")
            watcher = start("workspace-watch", [executable, "watch"], cwd=copy)
            watch_socket = Path(tempfile.gettempdir()) / ("bough-watch-" + hashlib.sha256(str(copy.resolve()).encode()).hexdigest()[:24] + ".sock")
            until = time.monotonic() + 15
            while not watch_socket.exists() and time.monotonic() < until:
                time.sleep(.05)
            assert watch_socket.exists(), "workspace watcher did not start"
            tracked = copy / "src/main.go"
            clean = tracked.read_bytes()
            cli("status", cwd=copy)
            tracked.write_bytes(clean + b"// watched edit\n")
            assert "M src/main.go" in cli("status", cwd=copy), "watcher missed a tracked edit"
            tracked.write_bytes(clean)
            assert "M src/main.go" not in cli("status", cwd=copy), "watcher missed content restoration"
            (copy / "src/unknown.go").write_text("untracked")
            assert "? src/unknown.go" in cli("status", cwd=copy), "watcher missed an unknown file"
            (copy / "src/unknown.go").unlink()
            watcher.terminate(); watcher.wait(timeout=10)
            assert "M src/main.go" not in cli("status", cwd=copy)
            checks.append("workspace monitor observes edits, restorations and new files; stopped monitor falls back to full scanning")
            snapshot = root / "snapshot.tar"
            snapshot_revision = api(servers[0], "refs/main")["target"]
            cli("snapshot-create", "--revision", snapshot_revision, "--out", str(snapshot))
            restricted = api(servers[0], "tokens", {"name": "agent", "admin": False,
                "rules": [{"prefix": "src", "read": True, "write": True}]})["token"]
            restricted_copy = root / "restricted"
            cli("clone", str(restricted_copy), extra={"BOUGH_TOKEN": restricted})
            assert not (restricted_copy / "private").exists() and not (restricted_copy / "README").exists()
            assert (restricted_copy / "src/main.go").exists()
            checks.append("directory IAM filters names/content even on warm shared cache hits")
            try:
                api(servers[0], "commits", {"parents": [snapshot_revision], "changes": {"src/forbidden-copy": {
                    "id": hashlib.sha256(b"private source").hexdigest(), "mode": 0o100644, "size": len(b"private source")}},
                    "message": "must be rejected", "request_id": "forbidden-copy"}, token=restricted)
                raise AssertionError("known private hash authorized a source copy")
            except urllib.error.HTTPError as error:
                assert error.code == 403; error.close()
            checks.append("known blob hash cannot bypass repository/principal upload ownership")
            api(servers[0], "policies", {"policies": [{"prefix": "protected", "approvals": 1,
                  "reviewers": ["reviewer"], "allow_self": False, "comment_only_exempt": False}]})
            protected = commit(servers[0], api(servers[0], "refs/main")["target"], {"protected/f": "sensitive logic"})
            try:
                land(servers[0], protected)
                raise AssertionError("unreviewed protected change landed")
            except urllib.error.HTTPError as error:
                assert error.code == 403
                error.close()
            reviewer = api(servers[0], "tokens", {"name": "reviewer", "admin": False,
                       "rules": [{"prefix": "", "read": True, "write": False}]})["token"]
            api(servers[1], "review", {"revision": protected, "approve": True}, token=reviewer)
            land(servers[0], protected)
            checks.append("protected landing requires approval of immutable revision by another authorized principal")
            newest = commit(servers[0], api(servers[0], "refs/main")["target"], {"new/uncached": "must traverse a cache"})
            cache_processes[0].terminate(); cache_processes[0].wait(timeout=20)
            failed = subprocess.run([executable, "clone", "--revision", newest, str(root / "must-fail")], cwd=root, env=env,
                                    capture_output=True, text=True, timeout=30)
            assert failed.returncode != 0, "daemon bypassed failed cache"
            cli("clone", "--revision", newest, str(root / "other-region"),
                extra={"BOUGH_SERVER": servers[1], "BOUGH_DAEMON_SOCKET": daemon_sockets[1]})
            checks.append("failed regional cache is not bypassed; independent region remains available")
            warm_socket = str(root / "warm.sock")
            start("warm-daemon", [executable, "daemon", "--socket", warm_socket, "--parent", caches[0],
                  "--cache", str(root / "warm-cache"), "--warm-snapshot", str(snapshot)])
            end = time.monotonic() + 20
            while not Path(warm_socket).exists() and time.monotonic() < end:
                time.sleep(.05)
            cli("clone", "--revision", snapshot_revision, str(root / "from-snapshot"),
                extra={"BOUGH_DAEMON_SOCKET": warm_socket})
            checks.append("node daemon starts from a verified snapshot and serves it while its parent cache is down")
            warmed = json.loads(cli("derive", "--repo", "test", "--kind", "all"))
            assert warmed["path-search/v1"] > 0 and warmed["copy-paths/v1"] > 0
            again = json.loads(cli("derive", "--repo", "test", "--kind", "all"))
            assert all(number == 0 for number in again.values()), "derivation job repeated completed work"
            checks.append("administrator prewarming persists lazy indices and resumes without repeating completed work")
            matches = api(servers[0], "search?revision=main&q=main.go")["paths"]
            assert matches == ["src/main.go"]
            api(servers[0], "copies?revision=main&path=src/main.go")
            backup = root / "independent-backup.tar"
            cli("backup", "--file", str(backup))
            subprocess.run([cockroach, "sql", "--insecure", f"--host=127.0.0.1:{sql_ports[0]}",
                            "--execute=CREATE DATABASE boughline_restored;"],
                           check=True, capture_output=True, env=env, timeout=40)
            restored_env = {"BOUGH_SQL_URL": f"postgresql://root@127.0.0.1:{sql_ports[0]}/boughline_restored?sslmode=disable",
                            "BOUGH_S3_BUCKET": "boughline-restored"}
            cli("migrate", extra=restored_env); cli("init-store", extra=restored_env)
            cli("restore", "--file", str(backup), extra=restored_env)
            verified = json.loads(cli("verify", extra=restored_env))
            assert verified["verified_objects"] > 0
            restored_port = port(); restored_server = f"http://127.0.0.1:{restored_port}"
            start("restored-api", [executable, "server", "--listen", f"127.0.0.1:{restored_port}"], restored_env)
            wait_http(restored_server + "/healthz")
            restored_head = api(restored_server, "refs/main")["target"]
            assert restored_head == api(servers[1], "refs/main")["target"]
            assert api(restored_server, "browse?revision=main&path=src/main.go")["file"]["id"] == hashlib.sha256(clean).hexdigest()
            assert api(restored_server, "search?revision=main&q=main.go")["paths"] == matches
            checks.append("consistent SQL plus immutable objects restore into an independent database and bucket; source and derived integrity verified")
            if args.forgejo:
                forgejo = str(Path(args.forgejo).resolve())
                source = Path(args.forgejo_source).resolve()
                forge_port = port(); forge_origin = f"http://127.0.0.1:{forge_port}"
                forge_dir = root / "forgejo"; forge_dir.mkdir()
                config = forge_dir / "app.ini"
                config.write_text(f"""APP_NAME = Boughline
RUN_MODE = prod
[server]
HTTP_ADDR = 127.0.0.1
INTERNAL_LISTENER_PATH = {root / "forge.sock"}
HTTP_PORT = {forge_port}
ROOT_URL = {forge_origin}/
APP_DATA_PATH = {forge_dir / 'data'}
STATIC_ROOT_PATH = {source}
DISABLE_SSH = true
[database]
DB_TYPE = sqlite3
PATH = {forge_dir / 'forgejo.db'}
[repository]
ROOT = {forge_dir / 'repos'}
BOUGHLINE_URL = {servers[0]}
BOUGHLINE_SECRET = {env['BOUGH_FORGEJO_SECRET']}
[security]
INSTALL_LOCK = true
SECRET_KEY = {secrets.token_hex(32)}
INTERNAL_TOKEN = {secrets.token_hex(40)}
[service]
DISABLE_REGISTRATION = true
[session]
PROVIDER = file
PROVIDER_CONFIG = {forge_dir / 'sessions'}
[log]
MODE = console
LEVEL = warn
[indexer]
ISSUE_INDEXER_TYPE = db
""")
                config.chmod(0o600)
                def forge_cli(*command):
                    completed = subprocess.run([forgejo, *command, "--config", str(config), "--work-path", str(forge_dir)], env=env, capture_output=True, text=True, timeout=120)
                    if completed.returncode:
                        raise RuntimeError("Forgejo setup failed: " + completed.stderr[-2000:] + completed.stdout[-2000:])
                    return completed.stdout.strip()
                forge_cli("migrate")
                forge_cli("admin", "user", "create", "--username", "owner", "--password", secrets.token_urlsafe(40),
                          "--email", "owner@example.invalid", "--admin", "--must-change-password=false")
                forge_token = forge_cli("admin", "user", "generate-access-token", "--username", "owner", "--token-name", "integration", "--scopes", "all", "--raw").splitlines()[-1]
                read_token = forge_cli("admin", "user", "generate-access-token", "--username", "owner", "--token-name", "read", "--scopes", "read:repository", "--raw").splitlines()[-1]
                start("forgejo", [forgejo, "web", "--config", str(config), "--work-path", str(forge_dir)])
                wait_http(forge_origin + "/api/healthz", timeout=60)
                def forge_api(route, body=None, token=None, method=None):
                    raw = body if isinstance(body, bytes) else json.dumps(body).encode() if body is not None else None
                    request = urllib.request.Request(forge_origin + "/api/v1/" + route, data=raw, method=method,
                              headers={"Authorization": "token " + (token or forge_token), "Content-Type": "application/octet-stream" if isinstance(body, bytes) else "application/json"})
                    try:
                        with opener.open(request, timeout=60) as response:
                            return json.load(response)
                    except urllib.error.HTTPError as error:
                        detail = error.read(2000).decode(errors="replace")
                        raise RuntimeError(f"Forgejo {route}: HTTP {error.code}: {detail}") from error
                created = forge_api("user/repos", {"name": "native-project", "boughline": True, "private": False})
                info = forge_api("repos/owner/native-project/bough")
                gateway = f"repos/owner/native-project/bough/v1/repos/{info['repo']}/"
                native_head = forge_api(gateway + "refs/main")["target"]
                blob = forge_api(gateway + "blobs", b"native source through Forgejo\n")["id"]
                revision = forge_api(gateway + "commits", {"parents": [native_head], "changes": {"hello.txt": {"id": blob, "mode": 0o100644, "size": len(b"native source through Forgejo\n")}}, "message": "through Forgejo", "request_id": "forge-integration"})["id"]
                forge_api(gateway + "land", {"ref": "main", "expected": native_head, "revision": revision, "request_id": "forge-land"})
                forge_copy = root / "forge-copy"
                cli("clone", "--forgejo", forge_origin, "--owner", "owner", "--repo", "native-project", str(forge_copy), extra={"BOUGH_TOKEN": read_token, "BOUGH_DAEMON_SOCKET": daemon_sockets[1]})
                assert (forge_copy / "hello.txt").read_text() == "native source through Forgejo\n"
                with opener.open(forge_origin + "/owner/native-project") as response:
                    html = response.read().decode()
                    assert "hello.txt" in html and "Boughline" in html
                with opener.open(forge_origin + "/owner/native-project?path=hello.txt") as response:
                    html = response.read().decode()
                    assert "native source through Forgejo" in html
                try:
                    with opener.open(forge_origin + "/owner/native-project.git/info/refs?service=git-upload-pack"):
                        raise AssertionError("Git transport served native source")
                except urllib.error.HTTPError as error:
                    assert error.code == 409; error.close()
                checks.append("real Forgejo binary: native project creation, permission gateway, read-scoped PAT clone, source UI, blocked Git transport")
            if args.fuse:
                if not Path("/dev/fuse").exists():
                    raise RuntimeError("--fuse requires a Linux runner with /dev/fuse")
                lazy_content = "lazy FUSE content " + secrets.token_hex(30)
                lazy_revision = commit(servers[1], api(servers[1], "refs/main")["target"], {"virtual/lazy.txt": lazy_content})
                lazy_id = hashlib.sha256(lazy_content.encode()).hexdigest()
                mountpoint = root / "mount"; mountpoint.mkdir()
                mount = start("fuse-mount", [executable, "mount", "--server", servers[1], "--socket", daemon_sockets[1],
                              "--repo", "test", "--revision", lazy_revision, str(mountpoint)])
                until = time.monotonic() + 20
                while not os.path.ismount(mountpoint) and time.monotonic() < until:
                    if mount.poll() is not None:
                        raise RuntimeError("virtual checkout exited before mounting")
                    time.sleep(.1)
                assert os.path.ismount(mountpoint), "virtual checkout did not mount"
                file = mountpoint / "virtual/lazy.txt"
                assert file.stat().st_size == len(lazy_content)
                cached_blob = root / "daemon-1/blob" / lazy_id[:2] / lazy_id[2:]
                assert not cached_blob.exists(), "virtual lookup eagerly fetched file contents"
                assert file.read_text() == lazy_content
                assert cached_blob.exists(), "virtual file body did not pass through node daemon"
                try:
                    file.write_text("must fail")
                    raise AssertionError("snapshot mount allowed writes")
                except OSError:
                    pass
                mount.terminate(); mount.wait(timeout=20)
                assert not os.path.ismount(mountpoint)
                checks.append("real Linux FUSE mount: metadata-only lookup, lazy daemon fetch, immutable content, clean unmount")
            sql_processes[2].kill(); sql_processes[2].wait(timeout=15)
            failover = commit(servers[0], api(servers[0], "refs/main")["target"], {"resilience/node-loss": "quorum write"})
            land(servers[1], failover)
            assert api(servers[1], "changes/" + failover)["id"] == failover
            start("sql-2-restarted", [cockroach, "start", "--insecure", f"--listen-addr=127.0.0.1:{sql_ports[2]}",
                  f"--http-addr=127.0.0.1:{web_ports[2]}", f"--join={joined}", f"--store={root / 'sql-2'}",
                  "--cache=64MiB", "--max-sql-memory=64MiB"])
            wait_http(f"http://127.0.0.1:{web_ports[2]}/health", timeout=60)
            subprocess.run([cockroach, "sql", "--insecure", f"--host=127.0.0.1:{sql_ports[2]}",
                            "--execute=SELECT count(*) FROM boughline.bough_changes;"], check=True, capture_output=True, env=env, timeout=40)
            checks.append("one SQL node killed: quorum publication/landing continues, restarted node rejoins with persisted history")
            print("distributed integration checks passed; running landing workload", flush=True)
            started = time.monotonic()
            def worker(number):
                last = None
                for sequence in range(number, args.count, 8):
                    server = servers[number % 2]
                    head = api(server, "refs/main")["target"]
                    last = commit(server, head, {f"agents/{number}": f"sequence={sequence}\n"}, key=f"load-{sequence}")
                    land(server, last)
                    with progress_lock:
                        completed["count"] += 1
                        if completed["count"] % 250 == 0:
                            print(f"landed {completed['count']}/{args.count}; conflicts={dict(conflicts)}", flush=True)
                return last
            with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
                last = list(pool.map(worker, range(8)))
            elapsed = time.monotonic() - started

            final_head = api(servers[1], "refs/main")["target"]
            for number, revision in enumerate(last):
                if revision is None:
                    continue
                assert api(servers[1], "merge-base", {"left": revision, "right": final_head})["bases"] == [revision], "lost a landed agent revision"
                encoded = api(servers[1], f"browse?revision={final_head}&path=agents/{number}")["content"]
                import base64
                expected_sequence = max(range(number, args.count, 8))
                assert base64.b64decode(encoded).decode() == f"sequence={expected_sequence}\n", "lost latest agent content"
            checks.append(f"{args.count} submissions landed across eight concurrent agents and two API servers")
            report = {"checks": checks, "landings": args.count, "workers": 8, "seconds": round(elapsed, 3),
                      "landings_per_second": round(args.count / elapsed, 3),
                      "conflicts": dict(conflicts),
                      "topology": "3 CockroachDB nodes, real MinIO, 2 API servers, 2 caches, 3 Unix-socket daemons",
                      "limits": "Small synthetic repository on one Mac; not a production-scale or cross-machine failure proof."}
            if args.output:
                Path(args.output).write_text(json.dumps(report, indent=2) + "\n")
            print(json.dumps(report, indent=2))
        except Exception as failure:
            print(f"integration failed: {type(failure).__name__}: {failure}", flush=True)
            if args.output:
                failure_path = Path(args.output).with_name(Path(args.output).stem + "-failure.json")
                failure_path.write_text(json.dumps({"status": "failed", "error": str(failure), "completed_landings": completed["count"], "conflicts": dict(conflicts), "checks": checks}, indent=2) + "\n")
            if isinstance(failure, subprocess.CalledProcessError):
                print(f"subprocess failed: {failure.stderr.decode() if isinstance(failure.stderr, bytes) else failure.stderr}")
            secrets_to_hide = [v for k, v in env.items() if any(s in k for s in ("TOKEN", "KEY", "SECRET", "PASSWORD", "SEED")) and len(v) > 12]
            for name, log in logs:
                log.flush(); log.seek(0)
                tail = log.read()[-1200:]
                for secret in secrets_to_hide:
                    tail = tail.replace(secret, "<redacted>")
                print(f"{name}: {tail}", flush=True)
            raise
        finally:
            for process in reversed(processes):
                if process.poll() is None:
                    process.terminate()
            for process in reversed(processes):
                try:
                    process.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    process.kill(); process.wait()
            for _, log in logs:
                log.close()


if __name__ == "__main__":
    main()
