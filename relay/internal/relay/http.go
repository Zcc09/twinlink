package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// DefaultControlAddr binds the control API to loopback: it is unauthenticated
// and must not be reachable from the internet by default (PROTOCOL §10).
const DefaultControlAddr = "127.0.0.1:51822"

// ControlHandler returns the control API mux: GET /healthz and GET /stats.
func (e *Engine) ControlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		st := e.Stats()
		body, err := json.MarshalIndent(st, "", "  ")
		if err != nil {
			http.Error(w, "encode error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		_, _ = w.Write([]byte("\n"))
	})
	return mux
}

// StartControl serves the control API on addr ("127.0.0.1:51822" by default).
func (e *Engine) StartControl(addr string) error {
	if addr == "" {
		addr = DefaultControlAddr
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("relay: control listen %q: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           e.ControlHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	e.controlLn = ln
	e.controlSrv = srv
	e.log.Info("control http listening", "addr", ln.Addr().String())
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			e.log.Debug("control server stopped", "err", err)
		}
	}()
	return nil
}

// ControlAddr returns the bound control address, if any.
func (e *Engine) ControlAddr() net.Addr {
	if e.controlLn == nil {
		return nil
	}
	return e.controlLn.Addr()
}

func (e *Engine) stopControl() {
	if e.controlSrv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = e.controlSrv.Shutdown(ctx)
}
