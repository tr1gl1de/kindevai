package db

import (
	"errors"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func code(err error) int {
	var e *sqlite.Error
	if errors.As(err, &e) {
		return e.Code()
	}
	return 0
}

// IsUniqueViolation reports a UNIQUE or PRIMARY KEY constraint failure.
func IsUniqueViolation(err error) bool {
	c := code(err)
	return c == sqlite3.SQLITE_CONSTRAINT_UNIQUE || c == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
}

// IsForeignKeyViolation reports a FOREIGN KEY constraint failure.
func IsForeignKeyViolation(err error) bool { return code(err) == sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY }
