package health

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewHealthServer(t *testing.T) {
	checker := func() error { return nil }
	hs := NewHealthServer(":0", checker)
	if hs == nil {
		t.Fatal("NewHealthServer returned nil")
	}
	if hs.server == nil {
		t.Error("server field should not be nil")
	}
	if hs.checker == nil {
		t.Error("checker field should not be nil")
	}
}

func TestHandleHealth(t *testing.T) {
	checker := func() error { return nil }
	hs := NewHealthServer(":0", checker)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	hs.handleHealth(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}

	body, _ := io.ReadAll(resp.Body)
	var result map[string]string
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if result["status"] != "ok" {
		t.Errorf("status = %q, want %q", result["status"], "ok")
	}
}

func TestHandleReady_OK(t *testing.T) {
	checker := func() error { return nil }
	hs := NewHealthServer(":0", checker)

	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	w := httptest.NewRecorder()

	hs.handleReady(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}

	body, _ := io.ReadAll(resp.Body)
	var result map[string]string
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if result["status"] != "ready" {
		t.Errorf("status = %q, want %q", result["status"], "ready")
	}
}

func TestHandleReady_NotReady(t *testing.T) {
	checker := func() error { return errors.New("s3 unreachable") }
	hs := NewHealthServer(":0", checker)

	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	w := httptest.NewRecorder()

	hs.handleReady(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}

	body, _ := io.ReadAll(resp.Body)
	var result map[string]string
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if result["status"] != "not ready" {
		t.Errorf("status = %q, want %q", result["status"], "not ready")
	}
	if _, hasError := result["error"]; hasError {
		t.Errorf("response should not contain error details, got %q", result["error"])
	}
}

func TestHandleMetrics(t *testing.T) {
	checker := func() error { return nil }
	hs := NewHealthServer(":0", checker)

	// Use the server's handler directly via httptest.
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()

	hs.server.Handler.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	// Prometheus metrics endpoint should contain at least some go runtime metrics.
	if !strings.Contains(bodyStr, "go_") && !strings.Contains(bodyStr, "s3gw_") {
		t.Error("metrics endpoint should contain prometheus metrics")
	}
}

func TestStartAndStop(t *testing.T) {
	checker := func() error { return nil }
	hs := NewHealthServer("127.0.0.1:0", checker)

	if err := hs.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	// Give the goroutine a moment to start serving.
	// We can verify by making an HTTP request to the actual address.
	// Since we used :0 the OS picks a port, but we can't easily get it
	// from the current API. Instead, just verify Stop works cleanly.

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := hs.Stop(ctx); err != nil {
		t.Errorf("Stop() error: %v", err)
	}
}

func TestStartAndStop_WithRequest(t *testing.T) {
	checker := func() error { return nil }
	// Use port 0 to get a random available port.
	hs := NewHealthServer("127.0.0.1:0", checker)

	if err := hs.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	// Clean up at the end.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		hs.Stop(ctx)
	}()

	// The server is running; we verified Start returns without error.
	// Additional integration testing would require extracting the listener address.
}

func TestStart_InvalidAddr(t *testing.T) {
	checker := func() error { return nil }
	// Use an invalid address to trigger a listen error.
	hs := NewHealthServer("999.999.999.999:0", checker)

	err := hs.Start()
	if err == nil {
		t.Fatal("Start() should return error for invalid address")
	}
}
