package mailru

import (
	"testing"
	"universal-bypass-tool/transport"
)

func TestSyntheticEditsRejectedBeforeStart(t *testing.T) {
	tr := NewMailruDocsTransport("unused/doc", transport.DefaultConfig())
	defer tr.Stop()
	tr.SetEditorActivity(true)
	if err := tr.Start(); err == nil || tr.IsRunning() {
		t.Fatal("synthetic edits must fail before any connection is opened")
	}
}
