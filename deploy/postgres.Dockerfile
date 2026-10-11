# Rebuild the upstream gosu source with maintained Go and user parsing fixes.
FROM golang:1.26.9-alpine3.23@sha256:6750308561953d451210ed1a8086d36c8c8bdcbf672d38d9d0d424b8a7a0216c AS gosu
WORKDIR /src
COPY deploy/gosu/go.mod deploy/gosu/go.sum ./
RUN go mod download && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /gosu github.com/tianon/gosu

# Preserve the PostgreSQL entry point and its privilege drop through gosu.
FROM postgres:17-alpine@sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24
RUN apk add --no-cache zlib=1.3.2-r1
COPY --from=gosu /gosu /usr/local/bin/gosu
RUN gosu nobody true
COPY deploy/postgres/roles.sh /docker-entrypoint-initdb.d/10-roles.sh
