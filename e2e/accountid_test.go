package e2e

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// awsDocsAccount is the placeholder account id the AWS documentation uses.
// The offline import fixture holds it.
const awsDocsAccount = "123456789012"

// fakeAccount is built at run time, so this file holds no 12-digit number.
var fakeAccount = strings.Repeat("1", 12)

// TestE2EHoldsNoAccountID fails on any 12-digit number under e2e/, which
// could be a real account id.
func TestE2EHoldsNoAccountID(t *testing.T) {
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		text, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, n := range twelveDigits(string(text)) {
			if n != awsDocsAccount {
				t.Errorf("%s holds the 12-digit number %s, which could be an account id; read it from WRIT_E2E_AWS_ACCOUNT", p, n)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := twelveDigits("a " + fakeAccount + ", 1" + fakeAccount + " and 1" + fakeAccount[2:] + "."); !slices.Equal(got, []string{fakeAccount}) {
		t.Errorf("twelveDigits = %q", got)
	}
}

var twelveDigitRE = regexp.MustCompile(`(?:^|[^0-9])([0-9]{12})(?:[^0-9]|$)`)

func twelveDigits(s string) []string {
	var out []string
	for _, m := range twelveDigitRE.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}
