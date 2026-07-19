package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// HTTPInfo is the handshake the daemon publishes so stream consumers and the
// Vite dev proxy can find the HTTP/SSE plane's ephemeral port. ExtraAddrs are
// the extra listeners' bound addresses, so a pairing CLI can tell whether the
// running daemon actually serves a leg before advertising it.
type HTTPInfo struct {
	Port       int      `json:"port"`
	Bind       string   `json:"bind,omitempty"`
	ExtraAddrs []string `json:"extra_addrs,omitempty"`
}

// readHTTPInfo returns the last published handshake, or the zero value when the
// file is absent, unreadable, or corrupt. The prior port is only a reuse hint
// for listenHTTP, so this is the one read where silent-zero is correct.
func (s *Server) readHTTPInfo() HTTPInfo {
	b, err := os.ReadFile(s.paths.HTTPInfoPath())
	if err != nil {
		return HTTPInfo{}
	}
	var info HTTPInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return HTTPInfo{}
	}
	return info
}

// writeHTTPInfo publishes the handshake atomically: a same-directory temp file
// (0600 via os.CreateTemp) renamed over the target, so a concurrent reader never
// sees a half-written file and a runtime republish never truncates the old one.
func (s *Server) writeHTTPInfo(info HTTPInfo) error {
	b, err := json.Marshal(info)
	if err != nil {
		return err
	}
	path := s.paths.HTTPInfoPath()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".http-*.json")
	if err != nil {
		return fmt.Errorf("write http info: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write http info: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write http info: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write http info: %w", err)
	}
	return nil
}
