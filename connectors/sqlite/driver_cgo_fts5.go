//go:build sqlite_cgo && !sqlite_fts5

package sqlite

// mattn/go-sqlite3 only includes FTS5 with the sqlite_fts5 build tag.
// Build with: go build -tags "sqlite_cgo sqlite_fts5"
var _ = buildTagSqliteFts5IsRequiredWithSqliteCgo
