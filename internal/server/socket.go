package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Socket is where the API is for the server's own users: root, and the
// SSH user whose key may run hakobu's commands alone (install.sh puts it
// in the directory's group).
const Socket = "/run/hakobu-api/api.sock"

type peerKey struct{}

// listenSocket listens on the config's socket, if its directory is
// there; nil if it isn't.
func (d *Daemon) listenSocket() (net.Listener, error) {
	sock := d.Config.Socket
	if _, err := os.Stat(filepath.Dir(sock)); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err := os.Remove(sock); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("the API can't listen on %s: %w", sock, err)
	}
	// The directory's group may open it, and no one else: the directory
	// is set-group-ID, so the socket is that group's.
	if err := os.Chmod(sock, 0o660); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// peerContext tags a connection on the socket with what its peer may do:
// root what the admin token may, anyone else the operator's.
func peerContext(ctx context.Context, c net.Conn) context.Context {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return ctx
	}
	uid, err := peerUID(uc)
	if err != nil {
		return context.WithValue(ctx, peerKey{}, None)
	}
	if uid == 0 {
		return context.WithValue(ctx, peerKey{}, Admin)
	}
	return context.WithValue(ctx, peerKey{}, Operator)
}

// levelOf is what the request may do: its peer's on the socket, or its
// token's, whichever is more.
func (d *Daemon) levelOf(r *http.Request) Level {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	l := d.Config.TokenLevel(token)
	if p, ok := r.Context().Value(peerKey{}).(Level); ok && p > l {
		l = p
	}
	return l
}
