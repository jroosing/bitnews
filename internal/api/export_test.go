package api

import "net/http"

// HandleForTest registers an extra route behind the full middleware stack.
func (s *Server) HandleForTest(pattern string, h http.HandlerFunc) { s.mux.Handle(pattern, h) }
