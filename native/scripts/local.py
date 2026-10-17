#!/usr/bin/env python3
"""Persistent loopback-only development instance for macOS/Linux, without Docker.

Requires built bough/Forgejo binaries, built Forgejo frontend, CockroachDB and
MinIO. Ctrl-C stops children; all repository data stays in --state.
"""
import argparse
import fcntl
import hashlib
import http.cookiejar
import json
import os
from pathlib import Path
import secrets
import shlex
import signal
import socket
import stat
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for binary in ('bough', 'forgejo', 'cockroach', 'minio'):
        parser.add_argument('--' + binary, required=True)
    parser.add_argument('--state', default='local/boughline')
    parser.add_argument('--web-port', type=int, default=3000)
    parser.add_argument('--check', action='store_true', help='exercise the real CLI, then stop; retain state')
    args = parser.parse_args()
    project = Path(__file__).resolve().parents[2]
    binaries = {name: str(Path(getattr(args, name)).resolve()) for name in ('bough', 'forgejo', 'cockroach', 'minio')}
    for path in binaries.values():
        if not os.access(path, os.X_OK):
            parser.error('executable missing: ' + path)
    if not 1 <= args.web_port <= 65535:
        parser.error('invalid web port')
    os.umask(0o077)
    root = Path(args.state).resolve()
    if any(char in str(root) for char in '\r\n'):
        parser.error('--state cannot contain newlines')
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    if root.stat().st_uid != os.getuid() or root.stat().st_mode & 0o077:
        parser.error('--state must be private (chmod 700) and owned by your account')
    lock = (root / 'launcher.lock').open('a')
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        parser.error('this local instance is already running')
    ports = (args.web_port, 26257, 8081, 9000, 9001, 8712, 8713)
    if len(set(ports)) != len(ports):
        parser.error('--web-port overlaps a backend port')
    for port in ports:
        try:
            with socket.socket() as listener:
                listener.bind(('127.0.0.1', port))
        except OSError:
            parser.error(f'port {port} is busy; stop the other service before starting this instance')
    # Stable short paths keep existing workspaces usable after launcher restart.
    sockets = Path(tempfile.gettempdir()) / ('bough-local-' + hashlib.sha256(str(root).encode()).hexdigest()[:16])
    sockets.mkdir(mode=0o700, exist_ok=True)
    info = sockets.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
        parser.error('socket directory is not a private owned directory')
    base = {key: value for key, value in os.environ.items() if not key.startswith(('BOUGH_', 'MINIO_'))}
    base['COCKROACH_SKIP_ENABLING_DIAGNOSTIC_REPORTING'] = 'true'
    for name in ('logs', 'forgejo', 'sql', 'objects', 'cache', 'daemon'):
        (root / name).mkdir(mode=0o700, exist_ok=True)
    config_path = root / 'secrets.json'
    if config_path.exists():
        values = json.loads(config_path.read_text())
    else:
        generated = subprocess.check_output([binaries['bough'], 'keygen'], env=base, text=True)
        values = dict(line.split('=', 1) for line in generated.splitlines())
        values.update(BOUGH_S3_ACCESS_KEY='local-' + secrets.token_hex(8), BOUGH_S3_SECRET_KEY=secrets.token_urlsafe(40),
                      password=secrets.token_urlsafe(30), forge_secret=secrets.token_hex(40), forge_internal=secrets.token_hex(40))
        config_path.write_text(json.dumps(values))
    origin = f'http://127.0.0.1:{args.web_port}'
    env = {**base, **{k: v for k, v in values.items() if k.startswith('BOUGH_')},
           'BOUGH_OWNER': 'operator', 'BOUGH_SQL_URL': 'postgresql://root@127.0.0.1:26257/boughline?sslmode=disable',
           'BOUGH_S3_ENDPOINT': '127.0.0.1:9000', 'BOUGH_S3_TLS': 'false', 'BOUGH_S3_BUCKET': 'boughline-local'}
    forge_dir = root / 'forgejo'
    config = forge_dir / 'app.ini'
    config.write_text(f'''APP_NAME = Boughline Local
RUN_MODE = prod
[server]
HTTP_ADDR = 127.0.0.1
HTTP_PORT = {args.web_port}
ROOT_URL = {origin}/
INTERNAL_LISTENER_PATH = {sockets / 'forge.sock'}
APP_DATA_PATH = {forge_dir / 'data'}
STATIC_ROOT_PATH = {project}
DISABLE_SSH = true
[database]
DB_TYPE = sqlite3
PATH = {forge_dir / 'forgejo.db'}
[repository]
ROOT = {forge_dir / 'repos'}
BOUGHLINE_URL = http://127.0.0.1:8712
BOUGHLINE_SECRET = {values['BOUGH_FORGEJO_SECRET']}
[security]
INSTALL_LOCK = true
SECRET_KEY = {values['forge_secret']}
INTERNAL_TOKEN = {values['forge_internal']}
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
''')
    children, logs = [], []
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def run(command, environment=base, cwd=root):
        result = subprocess.run(command, env=environment, cwd=cwd, capture_output=True, text=True, timeout=180)
        if result.returncode:
            raise RuntimeError(f'{Path(command[0]).name} {command[1]} failed; see {root / "logs"}: ' + result.stderr[-1000:])
        return result.stdout.strip()

    def start(name, command, environment=base):
        log = (root / 'logs' / (name + '.log')).open('a')
        logs.append(log)
        child = subprocess.Popen(command, env=environment, cwd=root, stdout=log, stderr=subprocess.STDOUT)
        children.append((name, child))

    def ready(url):
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            for name, child in children:
                if child.poll() is not None:
                    raise RuntimeError(f'{name} exited; see {root / "logs" / (name + ".log")}')
            try:
                with opener.open(url, timeout=2) as response:
                    if response.status == 200:
                        return
            except (urllib.error.URLError, TimeoutError):
                time.sleep(.2)
        raise RuntimeError('service did not become ready: ' + url)

    def forge_cli(*command):
        return run([binaries['forgejo'], *command, '--config', str(config), '--work-path', str(forge_dir)])

    def api(route, token, body=None):
        request = urllib.request.Request(origin + '/api/v1/' + route, data=json.dumps(body).encode() if body is not None else None,
                  headers={'Authorization': 'token ' + token, 'Content-Type': 'application/json'})
        with opener.open(request, timeout=60) as response:
            return json.load(response)

    def stop(_signum, _frame):
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, stop)
    try:
        start('sql', [binaries['cockroach'], 'start-single-node', '--insecure', '--listen-addr=127.0.0.1:26257',
              '--http-addr=127.0.0.1:8081', '--store=' + str(root / 'sql'), '--cache=128MiB', '--max-sql-memory=128MiB'])
        ready('http://127.0.0.1:8081/health')
        run([binaries['cockroach'], 'sql', '--insecure', '--host=127.0.0.1:26257', '--execute=CREATE DATABASE IF NOT EXISTS boughline;'])
        start('objects', [binaries['minio'], 'server', str(root / 'objects'), '--address', '127.0.0.1:9000', '--console-address', '127.0.0.1:9001'],
              {**base, 'MINIO_ROOT_USER': values['BOUGH_S3_ACCESS_KEY'], 'MINIO_ROOT_PASSWORD': values['BOUGH_S3_SECRET_KEY']})
        ready('http://127.0.0.1:9000/minio/health/live')
        run([binaries['bough'], 'migrate'], env)
        run([binaries['bough'], 'init-store'], env)
        start('api', [binaries['bough'], 'server', '--listen', '127.0.0.1:8712'], env)
        ready('http://127.0.0.1:8712/healthz')
        start('cache', [binaries['bough'], 'cache', '--listen', '127.0.0.1:8713', '--parent', 'http://127.0.0.1:8712', '--cache', str(root / 'cache')],
              {**base, **{k: values[k] for k in ('BOUGH_PUBLIC_KEY', 'BOUGH_CACHE_KEY', 'BOUGH_DAEMON_KEY')}})
        ready('http://127.0.0.1:8713/healthz')
        daemon_socket = str(sockets / 'daemon.sock')
        start('daemon', [binaries['bough'], 'daemon', '--socket', daemon_socket, '--parent', 'http://127.0.0.1:8713', '--cache', str(root / 'daemon')],
              {**base, **{k: values[k] for k in ('BOUGH_PUBLIC_KEY', 'BOUGH_DAEMON_KEY')}})
        forge_cli('migrate')
        if 'token' not in values:
            users = forge_cli('admin', 'user', 'list').splitlines()
            if not any('owner' in line.split() for line in users):
                forge_cli('admin', 'user', 'create', '--username', 'owner', '--password', values['password'],
                          '--email', 'owner@example.invalid', '--admin', '--must-change-password=false')
            values['token'] = forge_cli('admin', 'user', 'generate-access-token', '--username', 'owner',
                                       '--token-name', 'local-' + secrets.token_hex(8), '--scopes', 'all', '--raw').splitlines()[-1]
            config_path.write_text(json.dumps(values))
        start('forgejo', [binaries['forgejo'], 'web', '--config', str(config), '--work-path', str(forge_dir)])
        ready(origin + '/api/healthz')
        try:
            api('repos/owner/playground', values['token'])
        except urllib.error.HTTPError as error:
            if error.code != 404:
                raise
            error.close()
            api('user/repos', values['token'], {'name': 'playground', 'private': False})
        client_env = {**base, 'BOUGH_TOKEN': values['token'], 'BOUGH_DAEMON_SOCKET': daemon_socket}
        (root / 'client.env').write_text(''.join('export ' + k + '=' + shlex.quote(v) + '\n' for k, v in client_env.items() if k.startswith('BOUGH_')))
        (root / 'login.txt').write_text('Username: owner\nPassword: ' + values['password'] + '\n')
        if args.check:
            metadata = api('repos/owner/playground', values['token'])
            assert metadata['clone_url'] == '' and metadata['ssh_url'] == '', 'Git clone links remain visible'
            assert not any((forge_dir / 'repos').rglob('*.git')), 'native project created Git storage'
            web = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
            def page(path, form=None):
                body = urllib.parse.urlencode(form).encode() if form is not None else None
                request = urllib.request.Request(origin + path, data=body, headers={'Origin': origin})
                with web.open(request, timeout=60) as response:
                    return response.read().decode(), response.geturl()
            page('/user/login')
            _, location = page('/user/login', {'user_name': 'owner', 'password': values['password']})
            assert '/user/login' not in location, 'web login failed'
            form, _ = page('/repo/create')
            assert 'name="repo_name"' in form and 'name="auto_init"' not in form and 'name="repo_template"' not in form
            name = 'web-proof-' + secrets.token_hex(6)
            content, location = page('/repo/create', {'uid': metadata['owner']['id'], 'repo_name': name})
            assert location.endswith('/owner/' + name) and 'bough clone' in content, 'native web creation failed'
            assert api('repos/owner/' + name + '/bough', values['token'])['repo'].startswith('repo-')
            settings, _ = page('/owner/' + name + '/settings')
            assert 'Native repository' in settings and 'Git hooks' not in settings
            page('/owner/' + name + '/settings/collaboration')
            try:
                page('/repo/migrate')
                raise AssertionError('Git migration page remained available')
            except urllib.error.HTTPError as error:
                assert error.code == 409
                error.close()
            for route, body in (
                ('user/repos', {'name': 'must-not-create', 'auto_init': True}),
                ('repos/migrate', {'clone_addr': 'https://example.invalid/repo.git', 'repo_name': 'must-not-migrate'}),
                ('repos/owner/playground/branches', None),
            ):
                try:
                    api(route, values['token'], body)
                    raise AssertionError('Git operation remained available: ' + route)
                except urllib.error.HTTPError as error:
                    assert error.code in (400, 409), (route, error.code)
                    error.close()
            for route in ('/owner/playground.git/info/refs?service=git-upload-pack',):
                try:
                    with opener.open(origin + route):
                        raise AssertionError('Git web route remained available: ' + route)
                except urllib.error.HTTPError as error:
                    # The migration page requires login before its rejection.
                    assert error.code in (409, 401), (route, error.code)
                    error.close()
            with tempfile.TemporaryDirectory(prefix='check-', dir=root) as temporary:
                checkout = Path(temporary) / 'checkout'
                run([binaries['bough'], 'clone', '--forgejo', origin, '--owner', 'owner', '--repo', 'playground',
                     '--cache', str(Path(temporary) / 'client-cache'), str(checkout)], client_env)
                (checkout / 'hello.txt').write_text('Local Boughline proof ' + secrets.token_hex(16) + '\n')
                run([binaries['bough'], 'add', 'hello.txt'], client_env, checkout)
                run([binaries['bough'], 'commit', '-m', 'Local test'], client_env, checkout)
                run([binaries['bough'], 'land'], client_env, checkout)
                run([binaries['bough'], 'update'], client_env, checkout)
                assert (checkout / 'hello.txt').read_text().startswith('Local Boughline proof ')
            print('Local test passed: native web/API creation, settings, no Git transport/storage, daemon checkout, commit, land and update.', flush=True)
            return
        print(f'Boughline: {origin}/owner/playground\nWeb login: {root / "login.txt"}\nCLI credentials: source {shlex.quote(str(root / "client.env"))}\nCtrl-C stops services; state is preserved at {root}', flush=True)
        while True:
            for name, child in children:
                if child.poll() is not None:
                    raise RuntimeError(f'{name} exited; see {root / "logs" / (name + ".log")}')
            time.sleep(1)
    except KeyboardInterrupt:
        pass
    finally:
        for _, child in reversed(children):
            if child.poll() is None:
                child.terminate()
        for _, child in reversed(children):
            try:
                child.wait(timeout=15)
            except subprocess.TimeoutExpired:
                child.kill()
                child.wait()
        for log in logs:
            log.close()
        lock.close()


if __name__ == '__main__':
    main()
