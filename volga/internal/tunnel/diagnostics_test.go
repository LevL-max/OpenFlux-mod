package tunnel

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hashicorp/yamux"
	"openflux-volga-lab/internal/recordconn"
)

func TestMuxDiagnosisFirstFailureAndPrivacy(t *testing.T) {
	d := &muxDiagnosis{}
	d.Printf("[DEBUG] private-document=%s", "private-cookie")
	d.Printf("[ERR] yamux: keepalive failed: %v", yamux.ErrKeepAliveTimeout)
	d.Printf("[ERR] yamux: Failed to read header: %v", errors.New("private-token"))
	d.Print("private-cookie")
	d.Println("private-cookie")
	r := d.snapshot(recordconn.ErrPeerRestart)
	if r["operation"] != "keepalive" || r["cause"] != "keepalive_timeout" || r["result"] != "peer_restart" {
		t.Fatalf("wrong first cause: %v", r)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "private") {
		t.Fatal("private data in diagnostic")
	}
	d = &muxDiagnosis{}
	d.Printf("[ERR] yamux: aborted stream open (destination=%s): %v", "private-host", errors.New("private-error"))
	r = d.snapshot(nil)
	if r["operation"] != "open" || r["cause"] != "other" {
		t.Fatalf("wrong open classification: %v", r)
	}
	b, _ = json.Marshal(r)
	if strings.Contains(string(b), "private") {
		t.Fatal("destination or error leaked")
	}
}
