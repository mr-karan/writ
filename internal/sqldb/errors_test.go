package sqldb_test

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mr-karan/writ/internal/sqldb"
)

func TestPostgresPermissionCause(t *testing.T) {
	for _, tc := range []struct{ code, cause string }{{"42501", "permission denied"}, {"25006", "read-only"}} {
		t.Run(tc.code, func(t *testing.T) {
			err := sqldb.Classify(true, "postgres state", &pgconn.PgError{Code: tc.code, Message: "sensitive role and database details"})
			if !strings.Contains(err.Error(), tc.cause) || strings.Contains(err.Error(), "sensitive") {
				t.Fatal(err)
			}
		})
	}
}
