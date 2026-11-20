package control

import (
	"io/fs"
	"path"
	"strconv"
	"strings"
)

// MigrationFiles names the migrations this writ applies, in order, as a
// database records them: a pair of engine files once, without its engine.
func MigrationFiles() []string {
	files, _ := fs.Glob(migrations, "migrations/*.sql")
	var names []string
	for _, f := range files {
		n := path.Base(f)
		for _, engine := range []string{".postgres.sql", ".sqlite.sql"} {
			if stem, ok := strings.CutSuffix(n, engine); ok {
				n = stem + ".sql"
			}
		}
		if len(names) == 0 || names[len(names)-1] != n {
			names = append(names, n)
		}
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
