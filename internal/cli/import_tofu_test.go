package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// OpenTofu 1.13.9 ignores X.tf when X.tofu is in the directory, and reads
// X.tf.json and X.tofu.json beside X.tf, so writ import bulk writes no
// file that another form of its name would hide or join. It refuses
// before it reads an object: the stack has no provider to read with.
func TestImportBulkTofuRefusesAnotherFormOfItsName(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		out, existing, want string
	}{
		{out: "imported.tf", existing: "imported.tofu", want: "imported.tofu exists, and tofu ignores imported.tf beside it"},
		{out: "imported.tf", existing: "imported.tf.json", want: "imported.tf.json exists, and tofu reads it with imported.tf"},
		{out: "imported.tf", existing: "imported.tofu.json", want: "imported.tofu.json exists, and tofu reads it with imported.tf"},
		{out: "net.tf", existing: "net.tofu", want: "net.tofu exists, and tofu ignores net.tf beside it"},
		{out: "imported.tf", existing: "imported.tf", want: "imported.tf exists; writ import bulk writes a new file"},
		{out: "imported.tf", existing: "imported.imports.json", want: "imported.imports.json exists; writ import bulk writes a new file"},
		{out: "override.tf", want: "tofu reads override.tf as an override"},
		{out: "net_override.tf", want: "tofu reads net_override.tf as an override"},
	} {
		t.Run(tc.out+" beside "+tc.existing, func(t *testing.T) {
			dir := t.TempDir()
			files := map[string]string{".terraform.lock.hcl": "", "main.tf": ""}
			if tc.existing != "" {
				files[tc.existing] = "# kept\n"
			}
			for name, text := range files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			err := bulkApp(dir, &out).ImportBulk(ctx, ImportBulkOptions{Type: "random_uuid", IDs: []string{"a"}, Out: tc.out})
			if _, ok := errors.AsType[*UsageError](err); !ok || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "pick another") {
				t.Fatalf("err = %v, want a usage error: %s", err, tc.want)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != len(files) {
				t.Errorf("the stack has %d files, want the %d it had", len(entries), len(files))
			}
		})
	}
}
