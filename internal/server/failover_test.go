package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hirotomasato/autoclawpi/internal/db"
)

// resetAccounts menghapus semua akun supaya tes failover deterministik
// (handleChat memakai db.ListAccounts() global). Pastikan DB siap dulu.
func resetAccounts(t *testing.T) {
	t.Helper()
	initTestDB(t)
	accounts, err := db.ListAccounts()
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	for _, a := range accounts {
		if err := db.DeleteAccount(a.ID); err != nil {
			t.Fatalf("DeleteAccount(%d): %v", a.ID, err)
		}
	}
}

// addAcct menambahkan akun dengan token penanda.
func addAcct(t *testing.T, name, token string) int64 {
	t.Helper()
	initTestDB(t)
	id, err := db.AddAccount(name, "Bearer "+token, "Bearer "+token+"-refresh",
		"zai", "", "", "test-"+name)
	if err != nil {
		t.Fatalf("AddAccount(%s): %v", name, err)
	}
	return id
}

// chatViaHandler memanggil endpoint publik (bukan forward langsung), jadi
// seluruh jalur failover handleChat ikut teruji.
func chatViaHandler(t *testing.T, s *Server, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// upstreamByToken membalas berdasar token yang diterima, supaya bisa
// mensimulasikan "akun A rusak, akun B sehat".
func upstreamByToken(per map[string]struct {
	status int
	body   string
}) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("X-Authorization")
		for marker, resp := range per {
			if strings.Contains(auth, marker) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(resp.status)
				_, _ = w.Write([]byte(resp.body))
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`))
	}))
}

const okBody = `{"id":"ok","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`
const reqBody = `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`

// 核心问题：账号被封（410004）时必须自动切到下一个账号。
func TestFailover_BannedAccountSwitchesToHealthy(t *testing.T) {
	resetAccounts(t)
	addAcct(t, "banned", "tok-banned")
	addAcct(t, "healthy", "tok-healthy")

	up := upstreamByToken(map[string]struct {
		status int
		body   string
	}{
		"tok-banned":  {403, `{"code":410004,"message":"账号已被封禁"}`},
		"tok-healthy": {200, okBody},
	})
	defer up.Close()

	s := newTestServer(t, up.URL)
	code, body := chatViaHandler(t, s, reqBody)

	if code != 200 {
		t.Fatalf("code = %d, want 200 (harus failover ke akun sehat); body=%s", code, body)
	}
	if !strings.Contains(body, `"content":"OK"`) {
		t.Errorf("body bukan dari akun sehat: %s", body)
	}
}

// 额度用完（402 / 810002 / 429）也必须切号。
func TestFailover_QuotaExhaustedSwitches(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"402 insufficient": {402, `{"code":402,"message":"insufficient balance"}`},
		"429 rate/quota":   {429, `{"code":429,"message":"too many requests"}`},
		"810002 high demand (di-remap jadi 429)": {403,
			`{"code":810002,"message":"high demand"}`},
		"810002 wrapped": {403,
			`{"error":{"message":"{\"code\":810002}"},"type":"upstream_error"}`},
	}

	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			resetAccounts(t)
			addAcct(t, "exhausted", "tok-exhausted")
			addAcct(t, "healthy", "tok-healthy")

			up := upstreamByToken(map[string]struct {
				status int
				body   string
			}{
				"tok-exhausted": bad,
				"tok-healthy":   {200, okBody},
			})
			defer up.Close()

			s := newTestServer(t, up.URL)
			code, body := chatViaHandler(t, s, reqBody)
			if code != 200 {
				t.Fatalf("code = %d, want 200; body=%s", code, body)
			}
		})
	}
}

// 401 过期：refresh 失败后仍应继续试下一个账号。
func TestFailover_401ThenRefreshFailsStillSwitches(t *testing.T) {
	resetAccounts(t)
	addAcct(t, "expired", "tok-expired")
	addAcct(t, "healthy", "tok-healthy")

	// upstream selalu 401 untuk akun expired; refresh endpoint juga gagal
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("X-Authorization")
		if strings.Contains(auth, "tok-expired") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"Invalid token"}`))
			return
		}
		if strings.Contains(auth, "tok-healthy") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(okBody))
			return
		}
		// refresh 路径（POST /userapi/v1/agent-refresh）等一律失败
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401,"msg":"refresh failed"}`))
	}))
	defer up.Close()

	s := newTestServer(t, up.URL)
	code, body := chatViaHandler(t, s, reqBody)
	if code != 200 {
		t.Fatalf("code = %d, want 200 (refresh 失败也要换号); body=%s", code, body)
	}
}

// 全部账号失败时：应返回上游最后的错误（不是笼统 503）。
func TestFailover_AllFailReturnsLastUpstreamError(t *testing.T) {
	resetAccounts(t)
	addAcct(t, "bad1", "tok-bad1")
	addAcct(t, "bad2", "tok-bad2")

	up := upstreamByToken(map[string]struct {
		status int
		body   string
	}{
		"tok-bad1": {403, `{"code":410004,"message":"账号已被封禁"}`},
		"tok-bad2": {403, `{"code":410004,"message":"账号已被封禁"}`},
	})
	defer up.Close()

	s := newTestServer(t, up.URL)
	code, body := chatViaHandler(t, s, reqBody)

	if code == 503 {
		t.Errorf("code = 503 (generic), ingin status upstream asli; body=%s", body)
	}
	if !strings.Contains(body, "410004") {
		t.Errorf("body tidak memuat error upstream terakhir: %s", body)
	}
}

// inactive / token kosong harus dilewati, tidak boleh bikin request gagal.
func TestFailover_SkipsInactiveAndEmptyToken(t *testing.T) {
	resetAccounts(t)
	// akun tidak aktif
	idOff, _ := db.AddAccount("off", "Bearer tok-off", "", "zai", "", "", "d")
	if err := db.UpdateAccount(&db.Account{ID: idOff, Name: "off",
		AccessToken: "Bearer tok-off", Provider: "zai", DeviceID: "d",
		Active: false}); err != nil {
		t.Fatalf("UpdateAccount: %v", err)
	}
	// akun token kosong
	if _, err := db.AddAccount("empty", "", "", "zai", "", "", "d2"); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	addAcct(t, "healthy", "tok-healthy")

	up := upstreamByToken(map[string]struct {
		status int
		body   string
	}{
		"tok-off":     {500, `{"message":"should never be called"}`},
		"tok-healthy": {200, okBody},
	})
	defer up.Close()

	s := newTestServer(t, up.URL)
	code, body := chatViaHandler(t, s, reqBody)
	if code != 200 {
		t.Fatalf("code = %d, want 200; body=%s", code, body)
	}
	if strings.Contains(body, "should never be called") {
		t.Errorf("akun nonaktif/kosong malah dipakai: %s", body)
	}
}

// 轮询确实推进：连续请求会轮换起始账号（两个都健康时应交替命中）。
func TestFailover_RoundRobinAdvances(t *testing.T) {
	resetAccounts(t)
	addAcct(t, "a1", "tok-a1")
	addAcct(t, "a2", "tok-a2")

	var hits []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Header.Get("X-Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(okBody))
	}))
	defer up.Close()

	s := newTestServer(t, up.URL)
	for i := 0; i < 4; i++ {
		if code, body := chatViaHandler(t, s, reqBody); code != 200 {
			t.Fatalf("req %d: code=%d body=%s", i, code, body)
		}
	}
	// 4 次请求、2 个账号 -> 两边都应被用到
	joined := strings.Join(hits, ",")
	if !strings.Contains(joined, "tok-a1") || !strings.Contains(joined, "tok-a2") {
		t.Errorf("round-robin 未轮换, hits=%v", hits)
	}
}

// 健康检查应报出账号总数与活跃数（容灾可见性）。
func TestHealthz_ReportsAccountCounts(t *testing.T) {
	resetAccounts(t)
	addAcct(t, "h1", "tok-h1")
	addAcct(t, "h2", "tok-h2")

	s := newTestServer(t, "http://127.0.0.1:1")
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("healthz bukan JSON: %v (%s)", err, rec.Body.String())
	}
	if n, _ := out["accounts"].(float64); int(n) != 2 {
		t.Errorf("accounts = %v, want 2", out["accounts"])
	}
	if ok, _ := out["ok"].(bool); !ok {
		t.Errorf("ok = %v, want true", out["ok"])
	}
}
