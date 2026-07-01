package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

func (s *Server) AttachAdapter() (string, error) {
	id, err := newAdapterID()
	if err != nil {
		return "", err
	}
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if s.shutdownRequested {
		return "", fmt.Errorf("owner is shutting down")
	}
	if s.adapterLeases == nil {
		s.adapterLeases = make(map[string]struct{})
	}
	s.adapterLeases[id] = struct{}{}
	s.adapterCount = len(s.adapterLeases)
	return id, nil
}

func (s *Server) DetachAdapter(id string) bool {
	id = strings.TrimSpace(id)
	s.lifeMu.Lock()
	if id == "" {
		s.lifeMu.Unlock()
		return false
	}
	if _, ok := s.adapterLeases[id]; !ok {
		s.lifeMu.Unlock()
		return false
	}
	delete(s.adapterLeases, id)
	s.adapterCount = len(s.adapterLeases)
	shouldShutdown := len(s.adapterLeases) == 0 && s.agent.LiveSessionCount() == 0
	s.lifeMu.Unlock()
	if shouldShutdown {
		s.RequestShutdown()
	}
	return true
}

func (s *Server) RequestShutdown() {
	s.lifeMu.Lock()
	if s.shutdownRequested {
		s.lifeMu.Unlock()
		return
	}
	s.shutdownRequested = true
	cancel := s.cancel
	s.lifeMu.Unlock()
	s.agent.ShutdownOwner()
	if cancel != nil {
		cancel()
	}
}

func (s *Server) handleOwnerShutdown(w http.ResponseWriter, r *http.Request) {
	jsonResp(w, http.StatusOK, map[string]any{"ok": true})
	go s.RequestShutdown()
}

func newAdapterID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func WaitForOwnerExit(home string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return WaitForOwnerExitContext(ctx, home)
}

func WaitForOwnerExitContext(ctx context.Context, home string) error {
	for {
		current, err := Read(home)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			if removeErr := Remove(home); removeErr == nil {
				return nil
			}
		} else if IsStale(current) {
			_ = Remove(home)
			return nil
		}
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("timed out waiting for owner to stop")
			}
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
