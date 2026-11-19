package ui

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"

	"github.com/mr-karan/writ/internal/server"
)

// networkView is the part of writ.network/v1 that a plan's or a run's page
// shows: the run's egress mode, the env's egress list, and a row for each
// destination the run asked for.
type networkView struct {
	Mode       string
	Egress     []string
	Rows       []networkRow
	Unrecorded int
	// Refused opens the section, since a refusal may be why the run
	// failed.
	Refused bool
}

type networkRow struct {
	Dest, Verdict, Reason string
	Count                 int
}

// readNetwork reads the network document of a plan or a run, nil when it
// has none.
func readNetwork(raw json.RawMessage) (*networkView, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var doc server.NetworkDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("read the run's network record: %w", err)
	}
	v := &networkView{Mode: doc.Mode, Egress: doc.Egress, Unrecorded: doc.Unrecorded}
	for _, d := range doc.Destinations {
		v.Rows = append(v.Rows, networkRow{Dest: net.JoinHostPort(d.Host, strconv.Itoa(d.Port)), Verdict: d.Verdict, Reason: d.Reason, Count: d.Count})
		v.Refused = v.Refused || d.Verdict == "refused"
	}
	return v, nil
}
