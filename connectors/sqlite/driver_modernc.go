//go:build !sqlite_cgo

package sqlite

import (
	"database/sql/driver"
	"errors"
	"strings"
	"sync"

	msqlite "modernc.org/sqlite"
	lib "modernc.org/sqlite/lib"
)

// The default driver is modernc.org/sqlite: pure Go, no CGO, cross-compiles
// anywhere (Lambda, scratch images). Build with -tags "sqlite_cgo sqlite_fts5"
// to use mattn/go-sqlite3 instead.
const driverName = "sqlite"

var registerOnce sync.Once
var registerErr error

// register installs go_lower, a Unicode-aware lower() used by icontains.
func register() error {
	registerOnce.Do(func() {
		registerErr = msqlite.RegisterDeterministicScalarFunction("go_lower", 1,
			func(_ *msqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
				switch v := args[0].(type) {
				case string:
					return strings.ToLower(v), nil
				case []byte:
					return strings.ToLower(string(v)), nil
				}
				return args[0], nil
			})
	})
	return registerErr
}

func dsn(path string, readOnly bool) string {
	if readOnly {
		return "file:" + path + "?mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)"
	}
	return "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate"
}

func isUnique(err error) bool {
	var e *msqlite.Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Code() == lib.SQLITE_CONSTRAINT_UNIQUE || e.Code() == lib.SQLITE_CONSTRAINT_PRIMARYKEY
}
