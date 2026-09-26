// Package health tracks readiness (Postgres reachable) and exposes it over
// the standard gRPC health service and HTTP /healthz and /readyz.
//
// Readiness depends on Postgres only: Redis failures make rate limiting fail
// open, and Kafka failures only delay the outbox relay and the stream
// consumer, so neither stops the service from serving.
package health

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	grpchealth "google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// Pinger checks a dependency (satisfied by *pgxpool.Pool).
type Pinger interface {
	Ping(ctx context.Context) error
}

// Monitor pings Postgres periodically and publishes the result.
type Monitor struct {
	db       Pinger
	grpc     *grpchealth.Server
	services []string
	interval time.Duration
	log      *slog.Logger

	ready    atomic.Bool
	stopping atomic.Bool
}

// NewMonitor returns a monitor that updates the given gRPC health services
// ("" is the overall server status).
func NewMonitor(db Pinger, grpc *grpchealth.Server, interval time.Duration, log *slog.Logger, services ...string) *Monitor {
	m := &Monitor{db: db, grpc: grpc, services: services, interval: interval, log: log}
	m.publish(false)
	return m
}

// Run checks immediately and then every interval until ctx is canceled.
func (m *Monitor) Run(ctx context.Context) {
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		m.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Check pings Postgres once and publishes the result.
func (m *Monitor) Check(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, max(m.interval, time.Second))
	defer cancel()
	err := m.db.Ping(ctx)
	ok := err == nil && !m.stopping.Load()
	if was := m.ready.Load(); was != ok && !m.stopping.Load() {
		if ok {
			m.log.InfoContext(ctx, "ready: postgres reachable")
		} else {
			m.log.WarnContext(ctx, "not ready: postgres unreachable", "error", err)
		}
	}
	m.publish(ok)
}

// Shutdown reports NOT_SERVING from now on (graceful shutdown).
func (m *Monitor) Shutdown() {
	m.stopping.Store(true)
	m.ready.Store(false)
	m.grpc.Shutdown()
}

// Ready reports the last readiness result.
func (m *Monitor) Ready() bool { return m.ready.Load() }

func (m *Monitor) publish(ok bool) {
	if m.stopping.Load() {
		ok = false
	}
	m.ready.Store(ok)
	st := healthpb.HealthCheckResponse_NOT_SERVING
	if ok {
		st = healthpb.HealthCheckResponse_SERVING
	}
	for _, s := range m.services {
		m.grpc.SetServingStatus(s, st)
	}
}

// Handler serves GET /healthz (the process is up) and GET /readyz (Postgres
// is reachable and the server is not shutting down).
func (m *Monitor) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !m.Ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ready\n")
	})
	return mux
}

// ProbeURL turns a listen address (":8081", "0.0.0.0:8081",
// "127.0.0.1:18053") into the local /readyz URL.
func ProbeURL(listenAddr string) (string, error) {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("http address %q: %w", listenAddr, err)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/readyz", nil
}

// Probe GETs url and fails unless it answers 200 (the container healthcheck).
func Probe(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return nil
}
