# Update vulnerable OS libraries until the vendor refreshes its image.
FROM redis:7.4.11-alpine@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499
RUN apk add --no-cache libcrypto3=3.3.7-r2 libssl3=3.3.7-r2 zlib=1.3.2-r1
COPY --chmod=755 deploy/redis-entrypoint.sh /usr/local/bin/finance-redis
USER redis
ENTRYPOINT ["/usr/local/bin/finance-redis"]
