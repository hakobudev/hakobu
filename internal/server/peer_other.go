//go:build !linux

package server

import (
	"errors"
	"net"
)

// peerUID is Linux's alone: elsewhere a peer may do nothing.
func peerUID(*net.UnixConn) (int, error) {
	return 0, errors.New("no peer credentials on this system")
}
