package server

import (
	"context"
	"sync"

	"github.com/MMinasyan/lightcode/internal/agent"
)

type LocalService struct {
	*agent.Agent
	server *Server
	done   <-chan error
	lifeMu sync.Mutex
	lease  string
}

func NewLocalService(a *agent.Agent, srv *Server, done <-chan error) *LocalService {
	return &LocalService{Agent: a, server: srv, done: done}
}

func (s *LocalService) AttachAdapter(context.Context) error {
	id, err := s.server.AttachAdapter()
	if err != nil {
		return err
	}
	s.lifeMu.Lock()
	s.lease = id
	s.lifeMu.Unlock()
	return nil
}

func (s *LocalService) DetachAdapter(context.Context) error {
	s.lifeMu.Lock()
	id := s.lease
	s.lifeMu.Unlock()
	if id == "" {
		return nil
	}
	_ = s.server.DetachAdapter(id)
	s.lifeMu.Lock()
	if s.lease == id {
		s.lease = ""
	}
	s.lifeMu.Unlock()
	return nil
}

func (s *LocalService) WaitOwner() error {
	if s.done == nil {
		return nil
	}
	return <-s.done
}
