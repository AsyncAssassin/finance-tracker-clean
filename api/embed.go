// Package api holds the API descriptions of the project: the OpenAPI
// document of the HTTP API served by the gateway and the protobuf
// definitions of the gRPC services.
package api

import _ "embed"

// OpenAPI is the OpenAPI 3 document of the HTTP API.
//
//go:embed openapi.yaml
var OpenAPI []byte

// Docs is the Swagger UI page that renders OpenAPI.
//
//go:embed docs.html
var Docs []byte
