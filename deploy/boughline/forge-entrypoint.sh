#!/bin/sh
set -eu
# Keep the configured host owner able to read private bind-mounted state. There
# is no SSH daemon, Git push hook or process supervisor in this native image.
if [ "$(id -u)" = 0 ]; then
  bough_uid=${USER_UID:-1000}
  bough_gid=${USER_GID:-1000}
  case "$bough_uid:$bough_gid" in *[!0-9:]*|:*|*:) echo 'USER_UID/USER_GID must be numeric' >&2; exit 1;; esac
  sed -i "s/^git:[^:]*:[0-9]*:/git:x:$bough_gid:/" /etc/group
  sed -i "s/^git:[^:]*:[0-9]*:[0-9]*:/git:x:$bough_uid:$bough_gid:/" /etc/passwd
  mkdir -p /data/gitea /data/git /run/forgejo
  chown git:git /data/gitea /data/git /run/forgejo
  exec su-exec git:git /usr/local/bin/forgejo "$@"
fi
exec /usr/local/bin/forgejo "$@"
