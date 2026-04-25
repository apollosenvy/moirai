package modelmgr

import (
	"fmt"
	"net"
	"net/http"
	"testing"
)

// fakeLlamaServer is a tiny test helper that binds a local tcp listener
// and serves the /v1/models endpoint so waitReady() is satisfied. Used
// by start() tests that need a "successful" boot.
type fakeLlamaServer struct {
	listener net.Listener
	server   *http.Server
	port     int
}

func newFakeLlamaServer(t *testing.T) *fakeLlamaServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = fmt.Fprint(w, `{"data":[]}`)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(l) }()
	port := l.Addr().(*net.TCPAddr).Port
	return &fakeLlamaServer{listener: l, server: srv, port: port}
}

func (f *fakeLlamaServer) close() {
	_ = f.server.Close()
	_ = f.listener.Close()
}

// pickUnusedPort returns a TCP port that's (briefly) unused at the moment
// it's called. The listener is closed before return, so the port may be
// rebound by the caller -- good enough for tests that want a "definitely
// not listening" port. Caller should spawn quickly after this to avoid
// racing another binder.
func pickUnusedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}
