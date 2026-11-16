# Rebuild the upstream gosu source with maintained Go and user parsing fixes.
FROM golang:1.26.11-alpine3.23@sha256:23f198931d4bd83b4f5a613bd75656257ae8bf832dcbc2db80e1050b5219ca2a AS gosu
WORKDIR /src
COPY deploy/gosu/go.mod deploy/gosu/go.sum ./
RUN go mod download && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /gosu github.com/tianon/gosu

# Preserve the PostgreSQL entry point and its privilege drop through gosu.
FROM postgres:17-alpine@sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24
RUN apk add --no-cache zlib=1.4-r0 musl=1.2.6-r3 musl-utils=1.2.6-r3 krb5-libs=1.22.3-r0 libxslt=1.1.43-r4
COPY --from=gosu /gosu /usr/local/bin/gosu
RUN gosu nobody true
COPY deploy/postgres/roles.sh /docker-entrypoint-initdb.d/10-roles.sh
