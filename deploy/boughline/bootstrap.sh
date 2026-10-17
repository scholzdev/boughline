#!/bin/sh
set -eu
bough migrate
attempt=0
until bough init-store; do
  attempt=$((attempt+1))
  if [ "$attempt" -gt 30 ]; then exit 1; fi
  sleep 2
done
