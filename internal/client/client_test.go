package client

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Proxy kosong → pakai client default (perilaku lama, tanpa proxy).
func TestHTTPFor_EmptyProxyUsesDefault(t *testing.T) {
	c := New("http://example.invalid", "http://example.invalid")
	if got := c.HTTPFor(""); got != c.HTTP {
		t.Errorf("HTTPFor(\"\") harus mengembalikan client default")
	}
}

// Proxy terisi → client berbeda dengan default (tidak mencampur koneksi antar akun).
func TestHTTPFor_ProxyGivesDistinctClient(t *testing.T) {
	c := New("http://example.invalid", "http://example.invalid")
	got := c.HTTPFor("http://127.0.0.1:7901")
	if got == c.HTTP {
		t.Fatal("HTTPFor(proxy) tidak boleh memakai client default")
	}
	if got.Transport == nil {
		t.Fatal("transport nil")
	}
}

// Proxy berbeda → transport berbeda (inti isolasi IP).
func TestHTTPFor_DifferentProxiesDifferentTransports(t *testing.T) {
	c := New("http://example.invalid", "http://example.invalid")
	a := c.HTTPFor("http://127.0.0.1:7901")
	b := c.HTTPFor("http://127.0.0.1:7902")
	if a.Transport == b.Transport {
		t.Error("dua proxy berbeda harus punya transport berbeda")
	}
}

// Proxy sama → transport di-cache (bukan bikin baru tiap request).
func TestHTTPFor_CachesTransport(t *testing.T) {
	c := New("http://example.invalid", "http://example.invalid")
	first := c.HTTPFor("http://127.0.0.1:7901")
	second := c.HTTPFor("http://127.0.0.1:7901")
	if first.Transport != second.Transport {
		t.Error("proxy sama harus memakai transport yang sama (cache)")
	}
	if len(c.transportCache) != 1 {
		t.Errorf("cache berisi %d entri, want 1", len(c.transportCache))
	}
}

// Inti: request akun A benar-benar keluar lewat proxy A, dan akun B lewat proxy B.
// Diuji dengan dua "proxy" lokal yang menandai dirinya masing-masing.
func TestDoFor_RoutesThroughAccountProxy(t *testing.T) {
	// Proxy 1 menandai dirinya dengan header.
	proxy1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Proxy-Used", "proxy-1")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer proxy1.Close()

	proxy2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Proxy-Used", "proxy-2")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer proxy2.Close()

	c := New("http://example.invalid", "http://example.invalid")

	for _, tc := range []struct{ proxy, want string }{
		{proxy1.URL, "proxy-1"},
		{proxy2.URL, "proxy-2"},
	} {
		req, err := http.NewRequest("GET", "http://upstream.invalid/v1/chat", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		resp, err := c.DoFor(req, tc.proxy)
		if err != nil {
			t.Fatalf("DoFor(%s): %v", tc.proxy, err)
		}
		got := resp.Header.Get("X-Proxy-Used")
		resp.Body.Close()
		if got != tc.want {
			t.Errorf("proxy %s → header %q, want %q", tc.proxy, got, tc.want)
		}
	}
}

// Tanpa proxy, request TIDAK lewat proxy mana pun (langsung ke server).
func TestDoFor_NoProxyHitsServerDirectly(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Direct", "yes")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	c := New("http://example.invalid", "http://example.invalid")
	req, _ := http.NewRequest("GET", upstream.URL, nil)
	resp, err := c.DoFor(req, "")
	if err != nil {
		t.Fatalf("DoFor: %v", err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Direct") != "yes" {
		t.Error("request tanpa proxy harus langsung ke upstream")
	}
}

// URL proxy rusak → request GAGAL, bukan diam-diam direct
// (direct akan membocorkan IP asli, jadi harus fail-closed).
func TestDoFor_InvalidProxyFailsClosed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request tidak boleh sampai ke upstream saat proxy rusak")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	c := New("http://example.invalid", "http://example.invalid")
	req, _ := http.NewRequest("GET", upstream.URL, nil)
	// "://bad" tidak bisa diparse jadi URL proxy.
	_, err := c.DoFor(req, "://bad")
	if err == nil {
		t.Fatal("proxy rusak harus menghasilkan error (fail-closed), bukan direct")
	}
	if !strings.Contains(err.Error(), "proxy") && !strings.Contains(err.Error(), "invalid") {
		t.Logf("error: %v", err)
	}
}
