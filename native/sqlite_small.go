package main

import (
	"database/sql"
	sqlite3 "github.com/mattn/go-sqlite3"
)

// Keep the driver name used by existing data/migration code. The compact
// plugin statically links SQLite C, rather than its larger Go translation.
func init() { sql.Register("sqlite", &sqlite3.SQLiteDriver{}) }
