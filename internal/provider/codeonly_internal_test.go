package provider

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestCodeOnly covers the errors of the Shutdown and StopProvider calls,
// which Writ logs with no object to judge them by: a status keeps its code
// and loses the provider's message, and any other error stays.
func TestCodeOnly(t *testing.T) {
	quoted := status.Errorf(codes.Internal, "bad value %q", `synthetic"secret\key`)
	got := codeOnly(quoted)
	if status.Code(got) != codes.Internal || strings.Contains(got.Error(), "synthetic") {
		t.Fatalf("codeOnly(%v) = %v, want code Internal and no provider text", quoted, got)
	}
	if codeOnly(nil) != nil {
		t.Fatal("codeOnly(nil) is not nil")
	}
	plain := errors.New("connection closed")
	if got := codeOnly(plain); got != plain {
		t.Fatalf("codeOnly(%v) = %v, want it unchanged", plain, got)
	}
}
