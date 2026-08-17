// Package migrations exposes the versioned SQL migrations embedded in the
// application binary so startup does not depend on writable or mounted files.
package migrations

import "embed"

// FS contains every Goose migration in this directory.
//
//go:embed *.sql
var FS embed.FS
