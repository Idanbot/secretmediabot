package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestCounterAndGaugeExposition(t *testing.T) {
	attempts := Counter("test_open_attempts_total", "Attempts.", "result")
	attempts.Inc("allowed")
	attempts.Add(2, "denied")
	queue := Gauge("test_queue_depth", "Depth.")
	queue.Set(7)

	body := Snapshot()
	for _, want := range []string{
		"# HELP test_open_attempts_total Attempts.",
		"# TYPE test_open_attempts_total counter",
		`test_open_attempts_total{result="allowed"} 1`,
		`test_open_attempts_total{result="denied"} 2`,
		"# TYPE test_queue_depth gauge",
		"test_queue_depth 7",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Snapshot() missing %q in:\n%s", want, body)
		}
	}
}

func TestRegistryReusesSameMetric(t *testing.T) {
	first := Counter("test_reuse_total", "Reuse.")
	first.Inc()
	second := Counter("test_reuse_total", "Reuse.")
	second.Inc()
	if body := Snapshot(); !strings.Contains(body, "test_reuse_total 2") {
		t.Errorf("expected accumulated value 2, got:\n%s", body)
	}
}

func TestHandlerServesPlainText(t *testing.T) {
	Counter("test_handler_total", "Handler.").Inc()
	recorder := httptest.NewRecorder()
	Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("handler status = %d", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") {
		t.Errorf("content type = %q", contentType)
	}
}

func TestConcurrentChildCreation(t *testing.T) {
	counter := Counter("test_concurrent_total", "Concurrent.", "worker")
	var group sync.WaitGroup
	for i := range 16 {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for range 100 {
				counter.Inc("w")
			}
			_ = worker
		}(i)
	}
	group.Wait()
	if body := Snapshot(); !strings.Contains(body, "test_concurrent_total{worker=\"w\"} 1600") {
		t.Errorf("expected 1600 in:\n%s", body)
	}
}
