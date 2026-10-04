// Package migrations embeds the SQL schema migrations of both services.
// Apply them through cmd/migrate, which also registers the Ledger Go migration
// for category normalization. The standalone goose CLI would omit that step.
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed ledger/*.sql auth/*.sql
var files embed.FS

// Ledger returns the migrations of the ledger service.
func Ledger() fs.FS { return dir("ledger") }

// Auth returns the migrations of the auth service.
func Auth() fs.FS { return dir("auth") }

func dir(name string) fs.FS {
	sub, err := fs.Sub(files, name)
	if err != nil {
		panic(err) // the directories are embedded above, so this cannot happen
	}
	return sub
}
