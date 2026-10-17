#!/bin/sh
set -eu
attempt=0
until cockroach sql --host=127.0.0.1:26257 --certs-dir=/certs --execute='SELECT 1' >/dev/null 2>&1; do
  attempt=$((attempt+1))
  if [ "$attempt" -gt 60 ]; then echo 'SQL cluster did not initialize' >&2; exit 1; fi
  cockroach init --host=127.0.0.1:26257 --certs-dir=/certs >/dev/null 2>&1 || true
  sleep 2
done
cockroach sql --host=127.0.0.1:26257 --certs-dir=/certs --execute='CREATE DATABASE IF NOT EXISTS boughline; CREATE USER IF NOT EXISTS bough; GRANT ALL ON DATABASE boughline TO bough; GRANT ALL ON SCHEMA boughline.public TO bough;'
