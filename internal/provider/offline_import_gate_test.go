package provider_test

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/mr-karan/writ/internal/provider"
	"github.com/mr-karan/writ/internal/schema"
)

func TestOfflineAWSImportGate(t *testing.T) {
	if os.Getenv("WRIT_OFFLINE_AWS_GATE") != "1" {
		t.Skip("run with just e2e-import")
	}
	dirs, err := provider.DefaultDirs()
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Install(context.Background(), provider.InstallOptions{Dirs: dirs}, nil, []provider.Requirement{{Name: "aws", Source: schema.Source{Namespace: "hashicorp", Type: "aws"}, Version: "6.70.0"}})
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("unshare", "-rn", self, "-test.run=^(TestOfflineAWSImportPlan|TestOfflineAWSListenerStickiness|TestOfflineAWSImportIdentity)$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), "WRIT_OFFLINE_AWS=1")
	out, err := cmd.CombinedOutput()
	t.Logf("offline provider checks:\n%s", out)
	if err != nil {
		t.Fatal(err)
	}
}
