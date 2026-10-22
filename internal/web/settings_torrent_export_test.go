package web

// SetInterfaceBinding says whether this host can bind sockets to a network
// interface (Linux only). Tests only.
func SetInterfaceBinding(s *Server, supported bool) { s.interfaceBinding = supported }
