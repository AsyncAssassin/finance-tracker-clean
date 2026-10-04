// Package requestid carries the ID of a request through a context and
// between services, so the log lines of the gateway and the ledger for one
// request can be found by a single ID.
package requestid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

// Header is the HTTP header with the request ID.
const Header = "X-Request-ID"

// MetadataKey is the gRPC metadata key with the request ID; gRPC metadata
// keys are lowercase.
const MetadataKey = "x-request-id"

// MaxLen is the longest ID accepted from a client; longer ones are
// replaced, so a client cannot fill the logs with a huge header.
const MaxLen = 64

type ctxKey struct{}

// With returns a copy of ctx carrying id.
func With(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// From returns the request ID in ctx, or an empty string.
func From(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// Valid reports whether id can be used as received: 1 to MaxLen visible
// ASCII characters. gRPC metadata rejects other bytes, so an ID with them
// would fail the call to the ledger.
func Valid(id string) bool {
	if id == "" || len(id) > MaxLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < '!' || id[i] > '~' {
			return false
		}
	}
	return true
}

// New returns a random ID of 16 hex digits.
func New() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}
