package control

import (
	"io/fs"
	"path"
	"strconv"
	"strings"
)

// MigrationFiles names the migrations this writ applies, in order.
func MigrationFiles() []string {
	names, _ := fs.Glob(migrations, "migrations/*.sql")
	for i, n := range names {
		names[i] = path.Base(n)
	}
	return names
}

// Migrations is the schema version a database this writ migrated is at:
// the number of its newest migration. Numbers may skip, so it is not the
// count of files.
func Migrations() int {
	names := MigrationFiles()
	if len(names) == 0 {
		return 0
	}
	v, _ := strconv.Atoi(strings.SplitN(names[len(names)-1], "_", 2)[0])
	return v
}
