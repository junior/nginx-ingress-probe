package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:          "0 B",
		512:        "512 B",
		1024:       "1.0 KiB",
		1536:       "1.5 KiB",
		134217728:  "128.0 MiB",
		1073741824: "1.0 GiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestMaxSize(t *testing.T) {
	if got := maxSize(0); got != "unlimited" {
		t.Errorf("maxSize(0) = %q, want unlimited", got)
	}
	if got := maxSize(1024); got != "1.0 KiB" {
		t.Errorf("maxSize(1024) = %q, want 1.0 KiB", got)
	}
}

func TestCollectRequestForwarded(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	r.Header.Set("X-Forwarded-Proto", "https")
	ri := collectRequest(r)
	if !ri.ViaProxy {
		t.Error("ViaProxy = false, want true when X-Forwarded-* present")
	}
	if ri.Scheme != "https" {
		t.Errorf("Scheme = %q, want https", ri.Scheme)
	}
	if ri.ClientIP != "203.0.113.7" {
		t.Errorf("ClientIP = %q, want 203.0.113.7 (first XFF hop)", ri.ClientIP)
	}
	found := false
	for _, h := range ri.Headers {
		if h.Name == "X-Forwarded-For" && h.Forwarded {
			found = true
		}
	}
	if !found {
		t.Error("X-Forwarded-For should be flagged Forwarded")
	}
}

func TestCollectRequestDirect(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.1.2.3:5555"
	ri := collectRequest(r)
	if ri.ViaProxy {
		t.Error("ViaProxy = true, want false for a direct request")
	}
	if ri.ClientIP != "10.1.2.3" {
		t.Errorf("ClientIP = %q, want 10.1.2.3 (from RemoteAddr)", ri.ClientIP)
	}
}
