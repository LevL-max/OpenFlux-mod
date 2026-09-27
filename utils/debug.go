package utils

import (
	"fmt"
	"log"
	"os"
)

var (
	debugLog     *log.Logger
	verbose      bool
	packetTrace  bool
	statusEvents = true
)

// EnablePacketTrace adds a debug line for every tunnel packet. It is separate
// from --debug because those lines cost CPU on every packet and grow the log
// without bound.
func EnablePacketTrace() { packetTrace = true }

// IsPacketTrace reports whether per-packet debug lines are enabled.
func IsPacketTrace() bool { return verbose && packetTrace }

// EnableStatusEvents controls the safe operational events used by installers
// and Router Panel. These never contain browser cookies or session tokens.
func EnableStatusEvents(enabled bool) { statusEvents = enabled }

func Statusf(format string, args ...interface{}) {
	if statusEvents {
		log.Printf(format, args...)
	}
}

func EnableDebug() {
	verbose = true
	debugLog = log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)
}

func Debugf(format string, args ...interface{}) {
	if verbose {
		debugLog.Output(2, fmt.Sprintf(format, args...))
	}
}

func IsVerbose() bool {
	return verbose
}

// SafeGo runs fn in a new goroutine and contains any panic to that worker.
func SafeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				Debugf("[PANIC] recovered in %s: %v", name, r)
			}
		}()
		fn()
	}()
}
