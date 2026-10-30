//go:build sqlite_cgo

package sqlite

import (
	"database/sql"
	"errors"
	"strings"
	"sync"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// With -tags "sqlite_cgo sqlite_fts5" the connector uses mattn/go-sqlite3
// (CGO, usually faster). The sqlite_fts5 tag is required for full-text search.
const driverName = "niromem_sqlite3"

var registerOnce sync.Once

// register installs a driver whose connections define go_lower, a
// Unicode-aware lower() used by icontains.
func register() error {
	registerOnce.Do(func() {
		sql.Register(driverName, &sqlite3.SQLiteDriver{
			ConnectHook: func(c *sqlite3.SQLiteConn) error {
				return c.RegisterFunc("go_lower", strings.ToLower, true)
			},
		})
	})
	return nil
}

func dsn(path string, readOnly bool) string {
	if readOnly {
		return "file:" + path + "?mode=ro&_busy_timeout=5000&_query_only=1"
	}
	return "file:" + path + "?_busy_timeout=5000&_journal_mode=WAL&_synchronous=NORMAL&_txlock=immediate"
}

func isUnique(err error) bool {
	var e sqlite3.Error
	if !errors.As(err, &e) {
		return false
	}
	return e.ExtendedCode == sqlite3.ErrConstraintUnique || e.ExtendedCode == sqlite3.ErrConstraintPrimaryKey
}
