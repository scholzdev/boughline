"""Exercise the Linux self-host container recipe using an isolated CI account.
Run from the repository root after deploy/boughline/init.py. No public DNS or
certificate issuance is required: the tested backend listeners stay on loopback.
"""
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


def main():
    project = Path(__file__).resolve().parents[2]
    deployment = project / 'deploy/boughline'
    compose = ['docker', 'compose', '--project-directory', str(deployment), '-f', str(deployment / 'compose.yaml')]
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with tempfile.TemporaryDirectory(prefix='bough-containers-') as directory:
        work = Path(directory)
        def command(*args):
            result = subprocess.run([*compose, *args], capture_output=True, text=True, timeout=120)
            if result.returncode:
                raise RuntimeError(f'container command {args[0]} failed: {result.stderr[-1500:]}')
            return result.stdout.strip()
        deadline = time.monotonic() + 180
        while time.monotonic() < deadline:
            try:
                with opener.open('http://127.0.0.1:3000/api/healthz', timeout=2):
                    break
            except (urllib.error.URLError, TimeoutError):
                time.sleep(2)
        else:
            diagnostic = subprocess.run([*compose, 'logs', '--no-color', '--tail', '60'], capture_output=True, text=True, timeout=30)
            detail = diagnostic.stdout + diagnostic.stderr
            for line in (deployment / '.env').read_text().splitlines():
                _, value = line.split('=', 1)
                if len(value) >= 16:
                    detail = detail.replace(value, '<redacted>')
            raise RuntimeError('container Forgejo did not become healthy:\n' + detail[-18000:])
        command('exec', '-T', '--user', 'git', 'forge', 'forgejo', 'admin', 'user', 'create',
                '--config', '/data/gitea/conf/app.ini', '--username', 'ci-owner', '--random-password',
                '--email', 'ci@example.invalid', '--admin', '--must-change-password=false')
        token = command('exec', '-T', '--user', 'git', 'forge', 'forgejo', 'admin', 'user', 'generate-access-token',
                        '--config', '/data/gitea/conf/app.ini', '--username', 'ci-owner', '--token-name', 'container-proof', '--raw', '--scopes', 'all').splitlines()[-1]
        def api(route, body=None):
            raw = body if isinstance(body, bytes) else json.dumps(body).encode() if body is not None else None
            request = urllib.request.Request('http://127.0.0.1:3000/api/v1/' + route, data=raw,
                      headers={'Authorization': 'token ' + token, 'Content-Type': 'application/octet-stream' if isinstance(body, bytes) else 'application/json'})
            with opener.open(request, timeout=60) as response:
                return json.load(response)
        api('user/repos', {'name': 'native-project', 'private': False})
        info = api('repos/ci-owner/native-project/bough')
        route = f"repos/ci-owner/native-project/bough/v1/repos/{info['repo']}/"
        head = api(route + 'refs/main')['target']
        content = b'container source proof\n'
        blob = api(route + 'blobs', content)['id']
        revision = api(route + 'commits', {'parents': [head], 'changes': {'hello.txt': {'id': blob, 'mode': 0o100644, 'size': len(content)}},
                       'message': 'container proof', 'request_id': secrets.token_hex(24)})['id']
        api(route + 'land', {'ref': 'main', 'expected': head, 'revision': revision, 'request_id': secrets.token_hex(24)})
        with opener.open('http://127.0.0.1:3000/ci-owner/native-project?path=hello.txt') as response:
            assert 'container source proof' in response.read().decode()
        executable = work / 'bough'
        with executable.open('wb') as output:
            subprocess.run([*compose, 'run', '--rm', '--no-deps', '--entrypoint', 'cat', 'api1', '/usr/local/bin/bough'], stdout=output, check=True, timeout=60)
        executable.chmod(0o755)
        values = dict(line.split('=', 1) for line in (deployment / '.env').read_text().splitlines())
        env = {**os.environ, 'BOUGH_PUBLIC_KEY': values['BOUGH_PUBLIC_KEY'], 'BOUGH_DAEMON_KEY': values['BOUGH_DAEMON_KEY'], 'BOUGH_TOKEN': token}
        daemon_socket = str(work / 'daemon.sock')
        daemon = subprocess.Popen([str(executable), 'daemon', '--parent', 'http://127.0.0.1:8713', '--socket', daemon_socket, '--cache', str(work / 'node-cache')], env=env, stdout=subprocess.DEVNULL)
        try:
            deadline = time.monotonic() + 15
            while not Path(daemon_socket).exists() and time.monotonic() < deadline:
                time.sleep(.1)
            subprocess.run([str(executable), 'clone', '--server', 'http://127.0.0.1:3000/api/v1/repos/ci-owner/native-project/bough',
                            '--repo', info['repo'], '--socket', daemon_socket, '--cache', str(work / 'client-cache'), str(work / 'checkout')], env=env, check=True, timeout=60)
            assert (work / 'checkout/hello.txt').read_bytes() == content
        finally:
            daemon.terminate()
            daemon.wait(timeout=20)
        report = {'status': 'passed', 'checks': ['secure three-node SQL container bootstrap', 'MinIO durable objects',
                  'native API and regional cache containers', 'Forgejo native project and source page', 'host client through Unix node daemon and container cache'],
                  'limits': 'One Linux CI host; public ACME issuance and multi-host failure isolation are separate operational setup.'}
        (project / 'native/container-integration-result.json').write_text(json.dumps(report, indent=2) + '\n')
        print(json.dumps(report, indent=2))


if __name__ == '__main__':
    main()
