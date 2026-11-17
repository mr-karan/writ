package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/server"
)

// A dev record whose process is gone may name a port another program now
// holds: the probe gives up after devProbeTimeout, not remoteTimeout. While
// the process lives, the probe waits for a slow answer.
func TestAnswersDevWaitsOnlyForALiveProcess(t *testing.T) {
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); hang.Close() })
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Skip("no true to run: ", err)
	}
	start := time.Now()
	if answersDev(context.Background(), devClient{URL: hang.URL, PID: gone.Process.Pid}) {
		t.Fatal("a listener that never answers counts as a dev control plane")
	}
	if took := time.Since(start); took > devProbeTimeout+2*time.Second {
		t.Fatalf("the probe of a gone process's port took %s, want about %s", took, devProbeTimeout)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(devProbeTimeout + time.Second)
		_ = json.NewEncoder(w).Encode(server.ServerInfo{Format: server.FormatServer, Dev: true})
	}))
	t.Cleanup(slow.Close)
	if !answersDev(context.Background(), devClient{URL: slow.URL + "/", PID: os.Getpid()}) {
		t.Fatal("a live dev control plane that answers slowly counts as gone")
	}
}
