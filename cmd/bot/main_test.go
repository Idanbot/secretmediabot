package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthcheckUsesConfiguredAddress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("healthcheck path = %q, want /healthz", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Setenv("HTTP_ADDR", server.Listener.Addr().String())

	if err := healthcheck(); err != nil {
		t.Fatalf("healthcheck() error = %v", err)
	}
}

func TestHealthcheckDoesNotFollowRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/healthz", http.StatusFound)
	}))
	defer redirector.Close()

	t.Setenv("HTTP_ADDR", redirector.Listener.Addr().String())

	err := healthcheck()
	if err == nil {
		t.Fatal("healthcheck() error = nil, want redirect to fail")
	}
	if !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("healthcheck() error = %v, want HTTP 302", err)
	}
}
