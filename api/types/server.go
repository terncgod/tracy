package types

import (
	"fmt"
)

// Server is a struct that holds a configured server that has been
// resolved to a set of IPs and a port number.
type Server struct {
	Hostname string
	Port     uint
}

// Addr returns the address string of the Server to be used with libraries
// like http.Server.
func (a *Server) Addr() string {
	return fmt.Sprintf("%s:%d", a.Hostname, a.Port)
}

func (a *Server) Equal(b *Server) bool {
	if a.Hostname == b.Hostname && a.Port == b.Port {
		return true
	}
	return false
}

// IsEmpty returns true if the Server is populated.
func (a *Server) IsEmpty() bool {
	if a.Hostname == "" && a.Port == 0 {
		return true
	}

	return false
}
