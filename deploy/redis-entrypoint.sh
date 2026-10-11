#!/bin/sh
set -eu
umask 077
REDIS_PASSWORD=${REDIS_PASSWORD:-}
if [ "${#REDIS_PASSWORD}" -lt 32 ]; then
    echo 'REDIS_PASSWORD must contain at least 32 bytes' >&2
    exit 1
fi
digest=$(printf '%s' "$REDIS_PASSWORD" | sha256sum | cut -d ' ' -f 1)
printf 'user default off\nuser ledger on #%s ~report:v2:* -@all +ping +get +set +incr +select +hello +client|setinfo +client|setname\n' "$digest" > /tmp/finance-redis.acl
unset REDIS_PASSWORD digest
exec redis-server --aclfile /tmp/finance-redis.acl --save '' --appendonly no --maxmemory 64mb --maxmemory-policy volatile-lru
