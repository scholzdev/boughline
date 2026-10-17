#!/usr/bin/env python3
"""Generate a new Linux self-host configuration. Never replaces existing state."""
import argparse
import os
from pathlib import Path
import secrets
import shutil
import subprocess


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--domain', required=True)
    parser.add_argument('--cache-domain', required=True)
    parser.add_argument('--email', required=True)
    args = parser.parse_args()
    for value in (args.domain, args.cache_domain, args.email):
        if any(c in value for c in '\r\n =/{}'):
            parser.error('domain and email must be plain hostnames/email, without schemes or whitespace')
    if os.getuid() == 0:
        raise SystemExit("Run initialization as the non-root account that will own the instance state, with Docker access.")
    os.umask(0o077)
    directory = Path(__file__).resolve().parent
    state = directory / 'state'
    if state.exists() or (directory / '.env').exists():
        raise SystemExit('State already exists. Preserve its keys; do not regenerate it.')
    subprocess.run(['docker', 'build', '-t', 'boughline-native:local', str(directory.parent.parent / 'native')], check=True)
    generated = subprocess.check_output(['docker', 'run', '--rm', 'boughline-native:local', 'keygen'], text=True)
    values = dict(line.split('=', 1) for line in generated.splitlines())
    values.update(BOUGH_DOMAIN=args.domain, BOUGH_CACHE_DOMAIN=args.cache_domain, ACME_EMAIL=args.email,
                  BOUGH_UID=str(os.getuid()), BOUGH_GID=str(os.getgid()),
                  S3_ACCESS_KEY='boughline-' + secrets.token_hex(8), S3_SECRET_KEY=secrets.token_urlsafe(40))
    state.mkdir(mode=0o700)
    for name in ('sql1', 'sql2', 'sql3', 'objects', 'cache1', 'cache2', 'forgejo', 'caddy', 'caddy-config', 'certs/root', 'certs/nodes', 'certs/client'):
        (state / name).mkdir(mode=0o700, parents=True, exist_ok=True)
    image = 'cockroachdb/cockroach:v26.2.7'
    def cert(*command):
        subprocess.run(['docker', 'run', '--rm', '--user', f'{os.getuid()}:{os.getgid()}', '-v', f'{state}:/state', image, 'cert', *command], check=True)
    cert('create-ca', '--certs-dir=/state/certs/root', '--ca-key=/state/certs/ca.key')
    cert('create-client', 'root', '--certs-dir=/state/certs/root', '--ca-key=/state/certs/ca.key')
    for name in ('nodes', 'client'):
        shutil.copyfile(state / 'certs/root/ca.crt', state / f'certs/{name}/ca.crt')
    cert('create-node', 'localhost', '127.0.0.1', '--certs-dir=/state/certs/nodes', '--ca-key=/state/certs/ca.key')
    cert('create-client', 'bough', '--certs-dir=/state/certs/client', '--ca-key=/state/certs/ca.key')
    (directory / '.env').write_text(''.join(f'{key}={value}\n' for key, value in values.items()))
    conf = state / 'forgejo/gitea/conf'
    conf.mkdir(mode=0o700, parents=True)
    (conf / 'app.ini').write_text(f'''APP_NAME = Boughline
RUN_USER = git
RUN_MODE = prod
[server]
HTTP_ADDR = 127.0.0.1
HTTP_PORT = 3000
ROOT_URL = https://{args.domain}/
INTERNAL_LISTENER_PATH = /run/forgejo/native.sock
DISABLE_SSH = true
[database]
DB_TYPE = sqlite3
PATH = /data/gitea/forgejo.db
[repository]
ROOT = /data/git/repositories
BOUGHLINE_URL = http://127.0.0.1:8712
BOUGHLINE_SECRET = {values['BOUGH_FORGEJO_SECRET']}
[security]
INSTALL_LOCK = true
SECRET_KEY = {secrets.token_hex(40)}
INTERNAL_TOKEN = {secrets.token_hex(40)}
[service]
DISABLE_REGISTRATION = true
[session]
PROVIDER = file
PROVIDER_CONFIG = /data/gitea/sessions
[indexer]
ISSUE_INDEXER_TYPE = db
''')
    (state / 'daemon.env').write_text(f'''BOUGH_PUBLIC_KEY={values['BOUGH_PUBLIC_KEY']}
BOUGH_DAEMON_KEY={values['BOUGH_DAEMON_KEY']}
BOUGH_PARENT_URL=https://{args.cache_domain}
''')
    print('Configuration generated. Keep state/certs/ca.key and .env private and backed up.')
    print('Run docker compose up -d --build from deploy/boughline, then create the Forgejo admin account as documented.')


if __name__ == '__main__':
    main()
