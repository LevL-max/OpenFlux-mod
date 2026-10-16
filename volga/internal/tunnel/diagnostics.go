package tunnel

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"

	"github.com/hashicorp/yamux"
	"openflux-volga-lab/internal/recordconn"
)

// Classify failures without logging destinations, authenticated records, or
// arbitrary error strings. Yamux otherwise discards the initiating reason.
func failureKind(err error) string {
	var ne net.Error
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, recordconn.ErrPeerRestart):
		return "peer_restart"
	case errors.Is(err, recordconn.ErrProtocol):
		return "record_protocol"
	case errors.Is(err, context.Canceled), errors.Is(err, net.ErrClosed):
		return "closed"
	case errors.Is(err, yamux.ErrKeepAliveTimeout):
		return "keepalive_timeout"
	case errors.Is(err, yamux.ErrConnectionWriteTimeout):
		return "write_timeout"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	default:
		return "other"
	}
}

type muxDiagnosis struct {
	mu               sync.Mutex
	operation, cause string
}

// Yamux currently emits formatted diagnostics. Do not retain an unstructured
// Print/Println argument if a future library version uses those methods.
func (d *muxDiagnosis) Print(...interface{})   {}
func (d *muxDiagnosis) Println(...interface{}) {}

func (d *muxDiagnosis) Printf(format string, args ...interface{}) {
	if !strings.Contains(format, "[ERR]") {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.operation != "" {
		return
	}
	d.operation = "mux_error"
	for _, item := range []struct{ match, name string }{
		{"keepalive failed", "keepalive"}, {"Failed to read header", "read"},
		{"Failed to write", "write"}, {"aborted stream open", "open"},
		{"protocol", "protocol"},
	} {
		if strings.Contains(format, item.match) {
			d.operation = item.name
			break
		}
	}
	for _, arg := range args {
		if err, ok := arg.(error); ok {
			d.cause = failureKind(err)
			break
		}
	}
}

func (d *muxDiagnosis) snapshot(err error) map[string]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return map[string]string{"operation": d.operation, "cause": d.cause, "result": failureKind(err)}
}
