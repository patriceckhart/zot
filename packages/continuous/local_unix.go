//go:build !windows

package continuous

import (
	"context"
	"errors"
	"net"
	"os"
)

// ListenLocal listens on a private local endpoint for the host protocol. On
// Unix it is a Unix domain socket at path with mode 0600; a stale socket file
// is removed first. The caller removes the file after closing the listener.
func ListenLocal(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// DialLocal connects to a local endpoint created by ListenLocal.
func DialLocal(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}

// LocalEndpointName returns the address clients use for a local endpoint
// chosen by path. On Unix it is the path itself.
func LocalEndpointName(path string) string { return path }
