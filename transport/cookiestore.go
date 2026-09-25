package transport

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// CookieStore is a small persistent store mapping a transport/session key
// (normally the document URL) to cookie name/value pairs.
type CookieStore struct {
	path string
	mu   sync.RWMutex
	jar  map[string]map[string]string
}

func NewCookieStore(path string) (*CookieStore, error) {
	if path == "" {
		return nil, errors.New("cookiestore: empty path")
	}
	s := &CookieStore{
		path: path,
		jar:  make(map[string]map[string]string),
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("cookiestore: read %s: %w", path, err)
	}
	if len(data) == 0 {
		return s, nil
	}

	var loaded map[string]map[string]string
	if err := json.Unmarshal(data, &loaded); err != nil {
		return nil, fmt.Errorf("cookiestore: invalid JSON in %s: %w", path, err)
	}
	s.jar = loaded
	return s, nil
}

func (s *CookieStore) Path() string { return s.path }

func (s *CookieStore) Load(key string) map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	src := s.jar[key]
	if src == nil {
		return nil
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func (s *CookieStore) Save(key string, jar map[string]string) error {
	if key == "" {
		return errors.New("cookiestore: empty key")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(jar) == 0 {
		delete(s.jar, key)
	} else {
		cp := make(map[string]string, len(jar))
		for k, v := range jar {
			cp[k] = v
		}
		s.jar[key] = cp
	}
	return s.persistLocked()
}

func (s *CookieStore) persistLocked() error {
	dir := filepath.Dir(s.path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("cookiestore: mkdir %s: %w", dir, err)
		}
	}

	data, err := json.MarshalIndent(s.jar, "", "  ")
	if err != nil {
		return fmt.Errorf("cookiestore: marshal: %w", err)
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("cookiestore: write %s: %w", tmp, err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("cookiestore: chmod %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("cookiestore: rename %s -> %s: %w", tmp, s.path, err)
	}
	return nil
}
