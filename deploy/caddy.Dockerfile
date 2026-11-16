# Rebuild the standard Caddy modules with patched Go and HTTP/2 dependencies.
FROM golang:1.26.11-alpine3.23@sha256:23f198931d4bd83b4f5a613bd75656257ae8bf832dcbc2db80e1050b5219ca2a AS build
WORKDIR /src
COPY deploy/caddy/go.mod deploy/caddy/go.sum ./
RUN go mod download
COPY deploy/caddy/main.go ./
RUN CGO_ENABLED=0 go build -mod=readonly -tags=nobadger,nomysql,nopgx -trimpath -ldflags="-s -w" -o /out/caddy .

FROM caddy:2.11.9-alpine@sha256:003a97808e781aa9f7e75cc2f43d34d461099031faf5e20faefb9f61126836f0
COPY --from=build /out/caddy /usr/bin/caddy
RUN setcap cap_net_bind_service=+ep /usr/bin/caddy
