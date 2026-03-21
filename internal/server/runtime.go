package server

import "github.com/dotnode/gatelm/internal/config"

func (s *Server) CurrentConfig() config.Config {
	return s.snapshot().cfg
}

func (s *Server) AllBackendsDown() bool {
	snap := s.snapshot()
	if snap.health == nil {
		return false
	}
	return snap.health.AllBackendsDown()
}

func (s *Server) Close() error {
	s.stopCleanup()
	snap := s.snapshot()
	if snap.health != nil {
		snap.health.Stop()
	}
	if snap.tokenLog != nil {
		snap.tokenLog.Close()
	}
	return nil
}
