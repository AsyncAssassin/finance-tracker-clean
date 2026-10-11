# Rebuild the standard Caddy modules with patched Go and HTTP/2 dependencies.
FROM golang:1.26.9-alpine3.23@sha256:6750308561953d451210ed1a8086d36c8c8bdcbf672d38d9d0d424b8a7a0216c AS build
WORKDIR /src
COPY deploy/caddy/go.mod deploy/caddy/go.sum ./
RUN go mod download
COPY deploy/caddy/main.go ./
RUN CGO_ENABLED=0 go build -mod=readonly -tags=nobadger,nomysql,nopgx -trimpath -ldflags="-s -w" -o /out/caddy .

FROM caddy:2.11.7-alpine@sha256:d8542f48d34a9cf4e4c11a478865229840e87e4c96ea3f439101f31a5d35f75f
RUN apk add --no-cache zlib=1.3.2-r1
COPY --from=build /out/caddy /usr/bin/caddy
RUN setcap cap_net_bind_service=+ep /usr/bin/caddy
