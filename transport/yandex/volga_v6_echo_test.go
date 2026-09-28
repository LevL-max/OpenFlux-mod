//go:build volga

package yandex

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestVolgaV6HandoffIgnoresOwnLogicalDATA(t *testing.T) {
	for _, kind := range []volgaV6FrameKind{volgaV6FrameData, volgaV6FrameFragment} {
		t.Run(string(rune('0'+kind)), func(t *testing.T) {
			a, b, _, _, _, gotB := newLinkedVolgaV6Pair(t, defaultVolgaV6RuntimeConfig())
			if _, err := a.Send([][]byte{[]byte("before")}); err != nil {
				t.Fatal(err)
			}
			before := b.receiver.Snapshot()
			if _, _, err := b.manager.Handoff(context.Background()); err != nil {
				t.Fatal(err)
			}
			frame := volgaV6WireFrame{Kind: kind, Session: b.session.sessionID, Seq: 99, Floor: 99, Payload: [][]byte{[]byte("self-echo")}}
			if kind == volgaV6FrameFragment {
				data := frame
				data.Kind = volgaV6FrameData
				records, _ := encodeVolgaV6Records(data)
				blob, err := volgaV6RecordBlob(records)
				if err != nil {
					t.Fatal(err)
				}
				frame.Fragment = volgaV6Fragment{Total: uint32(len(blob)), Data: blob}
			}
			b.handleIncoming(frame)
			if after := b.receiver.Snapshot(); after != before {
				t.Fatalf("self echo changed receiver: before=%+v after=%+v", before, after)
			}
			if _, err := a.Send([][]byte{[]byte("after")}); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(*gotB, []string{"before", "after"}) {
				t.Fatalf("peer data lost or self delivered: %v", *gotB)
			}
			seq, err := b.Send([][]byte{[]byte("return path")})
			if err != nil {
				t.Fatal(err)
			}
			b.handleIncoming(volgaV6WireFrame{Kind: volgaV6FrameAck, Session: b.session.sessionID, Ack: volgaV6Ack{Session: b.session.sessionID, Base: seq}})
			if b.session.Snapshot(time.Now()).ReplayDepth != 0 {
				t.Fatal("valid ACK for local session was filtered")
			}
		})
	}
}

func TestVolgaV6AutomaticSessionsAreDistinct(t *testing.T) {
	var previous uint64
	for i := 0; i < 1000; i++ {
		s := newVolgaV6ReliableSession(0, nil, defaultVolgaV6ReliableConfig())
		if s.sessionID <= previous {
			t.Fatalf("session %d did not advance past %d", s.sessionID, previous)
		}
		previous = s.sessionID
	}
}
