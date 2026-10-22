package web

// OpenStreams is the number of /ui/events streams currently open.
func OpenStreams(s *Server) int { return int(s.live.open.Load()) }
