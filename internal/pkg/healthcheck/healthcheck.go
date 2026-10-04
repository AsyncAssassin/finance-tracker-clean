// Package healthcheck is the client side of the container health checks:
// the services run it with the -healthcheck flag, because their images
// have no shell or curl.
package healthcheck

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Timeout bounds one check.
const Timeout = 3 * time.Second

// LocalAddr turns a listen address such as ":8080" or "0.0.0.0:8080" into
// one a client can dial on the same machine.
func LocalAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "localhost"
	}
	return net.JoinHostPort(host, port)
}

// HTTP asks the HTTP server listening on listenAddr for path and reports an
// error unless it answers 200.
func HTTP(listenAddr, path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+LocalAddr(listenAddr)+path, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	return nil
}
