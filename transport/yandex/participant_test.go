package yandex

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"universal-bypass-tool/transport"
)

func TestParticipantsShareSerializedCookieStoreAndKeepIndependentSessions(t *testing.T) {
	first := NewYandexDocsTransport("document", transport.DefaultConfig())
	path := filepath.Join(t.TempDir(), "cookies.json")
	if err := first.SetCookieStore(path); err != nil {
		t.Fatal(err)
	}
	first.SetCursorSafety(2600, 100*time.Millisecond)
	second := first.NewParticipant()
	if first.cookieStore != second.cookieStore || first.BaseTransport == second.BaseTransport || first.baseUserID == second.baseUserID {
		t.Fatal("participants must share storage but have independent connection state")
	}
	if second.cursorMaxBytes != 2600 || second.writeEvery != 100*time.Millisecond {
		t.Fatal("lost safety configuration")
	}
	var wg sync.WaitGroup
	for _, peer := range []*YandexDocsTransport{first, second} {
		wg.Add(1)
		go func(peer *YandexDocsTransport) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				if err := peer.cookieStore.Save("document", map[string]string{"test": "value"}); err != nil {
					t.Error(err)
				}
			}
		}(peer)
	}
	wg.Wait()
	if _, err := transport.NewCookieStore(path); err != nil {
		t.Fatal(err)
	}
}
