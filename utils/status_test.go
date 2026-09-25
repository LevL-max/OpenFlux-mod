package utils

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestStatusEventsDoNotRequireDebug(t *testing.T) {
	var output bytes.Buffer
	oldOutput, oldStatus, oldVerbose := log.Writer(), statusEvents, verbose
	defer func() { log.SetOutput(oldOutput); statusEvents = oldStatus; verbose = oldVerbose }()
	log.SetOutput(&output)
	verbose = false
	EnableStatusEvents(true)
	Statusf("[YDOCS] AUTH_BLOCKED: waiting for cookie refresh")
	if !strings.Contains(output.String(), "AUTH_BLOCKED") {
		t.Fatal("authentication status must be visible without --debug")
	}
	output.Reset()
	EnableStatusEvents(false)
	Statusf("disabled event")
	if output.Len() != 0 {
		t.Fatal("disabled status events must be silent")
	}
}
