package core

// KindServersChanged is the EventKind of ServersChanged.
const KindServersChanged EventKind = "servers_changed"

// ServersChanged says a Server was created, changed or deleted. It carries
// no payload: receivers, such as the Usenet runner that rebuilds its
// connection pools, read the Servers themselves, which include passwords.
type ServersChanged struct{}

// Kind implements Event.
func (ServersChanged) Kind() EventKind { return KindServersChanged }
