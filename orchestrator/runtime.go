package orchestration

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Serve binds before starting work and shuts down when the controller context ends.
func Serve(ctx context.Context, port int, handler http.Handler, run func(context.Context) error) error {
	listener, e := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if e != nil {
		return e
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	defer server.Close()
	results := make(chan error, 2)
	go func() { results <- server.Serve(listener) }()
	go func() { results <- run(ctx) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case e := <-results:
		return e
	}
}
