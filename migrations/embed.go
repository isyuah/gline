package migrations

import "embed"

// FS contains the immutable, forward-only PostgreSQL migrations.
//
//go:embed *.up.sql
var FS embed.FS
