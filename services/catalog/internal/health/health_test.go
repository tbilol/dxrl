package health

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	grpchealth "google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

type pinger struct{ fail atomic.Bool }

func (p *pinger) Ping(context.Context) error {
	if p.fail.Load() {
		return errors.New("connection refused")
	}
	return nil
}

func TestMonitor(t *testing.T) {
	db := &pinger{}
	g := grpchealth.NewServer()
	m := NewMonitor(db, g, time.Second, slog.New(slog.DiscardHandler), "", "agrogram.catalog.v1.CatalogService")
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	grpcStatus := func() healthpb.HealthCheckResponse_ServingStatus {
		resp, err := g.Check(context.Background(), &healthpb.HealthCheckRequest{Service: "agrogram.catalog.v1.CatalogService"})
		if err != nil {
			t.Fatal(err)
		}
		return resp.GetStatus()
	}
	httpStatus := func(path string) int {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if m.Ready() || grpcStatus() != healthpb.HealthCheckResponse_NOT_SERVING || httpStatus("/readyz") != 503 {
		t.Fatal("must start not ready")
	}
	m.Check(context.Background())
	if !m.Ready() || grpcStatus() != healthpb.HealthCheckResponse_SERVING || httpStatus("/readyz") != 200 {
		t.Fatal("postgres up: want ready")
	}
	db.fail.Store(true)
	m.Check(context.Background())
	if m.Ready() || grpcStatus() != healthpb.HealthCheckResponse_NOT_SERVING || httpStatus("/healthz") != 200 {
		t.Fatal("postgres down: want not ready but alive")
	}
	db.fail.Store(false)
	m.Shutdown()
	m.Check(context.Background())
	if m.Ready() || httpStatus("/readyz") != 503 || grpcStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatal("shutting down: want not ready")
	}
	if err := Probe(context.Background(), srv.URL+"/readyz"); err == nil {
		t.Fatal("probe must fail while not ready")
	}
	if err := Probe(context.Background(), srv.URL+"/healthz"); err != nil {
		t.Fatalf("probe /healthz: %v", err)
	}
}

func TestProbeURL(t *testing.T) {
	for in, want := range map[string]string{
		":8081":           "http://127.0.0.1:8081/readyz",
		"0.0.0.0:8081":    "http://127.0.0.1:8081/readyz",
		"[::]:8081":       "http://127.0.0.1:8081/readyz",
		"127.0.0.1:18051": "http://127.0.0.1:18051/readyz",
		"localhost:9":     "http://localhost:9/readyz",
	} {
		if got, err := ProbeURL(in); err != nil || got != want {
			t.Errorf("%s -> %s, %v; want %s", in, got, err, want)
		}
	}
	if _, err := ProbeURL("8081"); err == nil {
		t.Error("want an error for an address without a port separator")
	}
}
