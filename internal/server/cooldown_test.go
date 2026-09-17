package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hirotomasato/autoclawpi/internal/db"
)

// Pola klasifikasi diambil dari klien AutoClaw resmi — pastikan cocok.
func TestClassifyUpstreamError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		// --- banned (410004) ---
		{"410004 langsung", 403, `{"code":410004,"message":"账号已被封禁"}`, db.CooldownBanned},
		{"410004 terbungkus", 403,
			`{"error":{"message":"{\"code\":410004,\"message\":\"账号已被封禁\"}"}}`, db.CooldownBanned},
		{"banned EN", 403, `{"message":"account banned"}`, db.CooldownBanned},

		// --- quota / saldo habis ---
		{"402 polos", 402, `{"message":"payment required"}`, db.CooldownQuota},
		{"insufficient_balance", 400,
			`{"error":"insufficient_balance"}`, db.CooldownQuota},
		{"余额不足", 400, `{"msg":"余额不足"}`, db.CooldownQuota},
		{"额度不足", 400, `{"msg":"账户额度不足，请充值"}`, db.CooldownQuota},
		{"积分已用完", 400, `{"msg":"积分已用完"}`, db.CooldownQuota},
		{"积分耗尽", 400, `{"msg":"积分耗尽"}`, db.CooldownQuota},

		// --- rate limit ---
		{"429", 429, `{"message":"too many requests"}`, db.CooldownRateLimit},
		{"810002", 403, `{"code":810002,"message":"high demand"}`, db.CooldownRateLimit},
		{"high demand EN", 403, `{"message":"We're experiencing high demand"}`, db.CooldownRateLimit},

		// --- JANGAN cooldown ---
		{"WAF block forbidden", 403, `{"message":"forbidden"}`, ""},
		{"500 server error", 500, `{"message":"internal error"}`, ""},
		{"503", 503, `{"message":"unavailable"}`, ""},
		{"400 model invalid", 400, `{"message":"非法模型"}`, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyUpstreamError(c.status, []byte(c.body)); got != c.want {
				t.Errorf("classify(%d, %s) = %q, want %q",
					c.status, c.body, got, c.want)
			}
		})
	}
}

// Kategori banned harus diprioritaskan di atas 403 generik.
func TestClassifyUpstreamError_BannedBeatsGeneric403(t *testing.T) {
	body := []byte(`{"code":410004,"message":"账号已被封禁"}`)
	if got := classifyUpstreamError(403, body); got != db.CooldownBanned {
		t.Fatalf("got %q, want banned (bukan dikosongkan seperti WAF block)", got)
	}
}

// Akun yang gagal karena kuota harus ditandai cooldown di DB, dan
// request tetap dilayani akun berikutnya.
func TestCooldown_QuotaMarksAccountAndFailsOver(t *testing.T) {
	resetAccounts(t)
	idBad := addAcct(t, "empty-wallet", "tok-empty")
	addAcct(t, "healthy", "tok-healthy")

	up := upstreamByToken(map[string]struct {
		status int
		body   string
	}{
		"tok-empty":   {402, `{"message":"payment required","code":402}`},
		"tok-healthy": {200, okBody},
	})
	defer up.Close()

	s := newTestServer(t, up.URL)
	code, body := chatViaHandler(t, s, reqBody)
	if code != 200 {
		t.Fatalf("code = %d, want 200; body=%s", code, body)
	}

	// Akun yang kehabisan kuota harus berstatus cooldown di DB.
	a, err := db.GetAccount(idBad)
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if !a.IsCoolingDown(time.Now()) {
		t.Errorf("akun #%d tidak ditandai cooldown (cooldown_until=%q)", idBad, a.CooldownUntil)
	}
	if a.CooldownReason != db.CooldownQuota {
		t.Errorf("reason = %q, want %q", a.CooldownReason, db.CooldownQuota)
	}
	// Active (saklar manual) TIDAK boleh diubah oleh logika otomatis.
	if !a.Active {
		t.Errorf("Active ikut berubah — logika otomatis tidak boleh menyentuhnya")
	}
}

// Akun yang sedang cooldown harus dilewati (tidak dihubungi upstream lagi).
func TestCooldown_SkipsCoolingAccount(t *testing.T) {
	resetAccounts(t)
	idCool := addAcct(t, "cooling", "tok-cooling")
	addAcct(t, "healthy", "tok-healthy")

	// Tandai cooldown panjang.
	if err := db.SetCooldown(idCool, db.CooldownBanned); err != nil {
		t.Fatalf("SetCooldown: %v", err)
	}

	up := upstreamByToken(map[string]struct {
		status int
		body   string
	}{
		// Kalau akun cooling tetap dihubungi, marker ini akan terlihat.
		"tok-cooling": {500, `{"message":"SHOULD-NOT-BE-CALLED"}`},
		"tok-healthy": {200, okBody},
	})
	defer up.Close()

	s := newTestServer(t, up.URL)
	code, body := chatViaHandler(t, s, reqBody)
	if code != 200 {
		t.Fatalf("code = %d, want 200; body=%s", code, body)
	}
	if strings.Contains(body, "SHOULD-NOT-BE-CALLED") {
		t.Errorf("akun cooldown masih dihubungi: %s", body)
	}
}

// Cooldown yang sudah lewat harus otomatis dianggap sembuh (auto-recover),
// tanpa perlu intervensi manual.
func TestCooldown_ExpiredIsUsableAgain(t *testing.T) {
	resetAccounts(t)
	id := addAcct(t, "recovered", "tok-recovered")

	// Cooldown masih berlaku.
	if err := db.SetCooldown(id, db.CooldownRateLimit); err != nil {
		t.Fatalf("SetCooldown: %v", err)
	}
	a, _ := db.GetAccount(id)
	if !a.IsCoolingDown(time.Now()) {
		t.Fatal("seharusnya masih cooldown")
	}
	if a.Usable(time.Now()) {
		t.Error("akun yang masih cooldown tidak boleh usable")
	}

	// Paksa waktunya ke masa lalu (simulasi cooldown lewat).
	if _, err := db.DB.Exec(
		`UPDATE accounts SET cooldown_until=? WHERE id=?`,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), id); err != nil {
		t.Fatalf("force-expire: %v", err)
	}

	a2, _ := db.GetAccount(id)
	if a2.IsCoolingDown(time.Now()) {
		t.Error("cooldown sudah lewat tapi masih dianggap cooling")
	}
	if !a2.Usable(time.Now()) {
		t.Error("akun seharusnya usable lagi setelah cooldown lewat")
	}

	// Request berikutnya harus memakai akun itu lagi (tanpa intervensi manual).
	up := upstreamByToken(map[string]struct {
		status int
		body   string
	}{
		"tok-recovered": {200, okBody},
	})
	defer up.Close()
	s := newTestServer(t, up.URL)
	code, body := chatViaHandler(t, s, reqBody)
	if code != 200 {
		t.Fatalf("code = %d, want 200 (auto-recover); body=%s", code, body)
	}
}

// ClearExpiredCooldowns hanya membersihkan yang sudah lewat (higiene DB),
// dan tidak menyentuh yang masih berlaku.
func TestCooldown_ClearExpiredOnly(t *testing.T) {
	resetAccounts(t)
	idExpired := addAcct(t, "expired", "tok-expired")
	idActive := addAcct(t, "cooling", "tok-cooling")

	// Satu sudah lewat, satu masih berlaku.
	if _, err := db.DB.Exec(
		`UPDATE accounts SET cooldown_until=?, cooldown_reason=? WHERE id=?`,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
		db.CooldownQuota, idExpired); err != nil {
		t.Fatalf("force-expire: %v", err)
	}
	if err := db.SetCooldown(idActive, db.CooldownBanned); err != nil {
		t.Fatalf("SetCooldown: %v", err)
	}

	n, err := db.ClearExpiredCooldowns()
	if err != nil {
		t.Fatalf("ClearExpiredCooldowns: %v", err)
	}
	if n != 1 {
		t.Errorf("RowsAffected = %d, want 1 (hanya yang sudah lewat)", n)
	}

	e, _ := db.GetAccount(idExpired)
	if e.CooldownUntil != "" {
		t.Errorf("cooldown lewat tidak dibersihkan: %q", e.CooldownUntil)
	}
	c, _ := db.GetAccount(idActive)
	if c.CooldownUntil == "" {
		t.Error("cooldown yang masih berlaku malah ikut dibersihkan")
	}
}

// Sukses harus membersihkan cooldown basi milik akun itu.
func TestCooldown_SuccessClearsStaleCooldown(t *testing.T) {
	resetAccounts(t)
	id := addAcct(t, "ok", "tok-ok")

	// Cooldown hampir habis, lalu upstream sehat.
	if err := db.SetCooldown(id, db.CooldownRateLimit); err != nil {
		t.Fatalf("SetCooldown: %v", err)
	}
	// Paksa until ke masa lalu supaya akun tetap dicoba.
	if _, err := db.DB.Exec(
		`UPDATE accounts SET cooldown_until=? WHERE id=?`,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), id); err != nil {
		t.Fatalf("force-expire: %v", err)
	}

	up := upstreamByToken(map[string]struct {
		status int
		body   string
	}{
		"tok-ok": {200, okBody},
	})
	defer up.Close()

	s := newTestServer(t, up.URL)
	if code, body := chatViaHandler(t, s, reqBody); code != 200 {
		t.Fatalf("code = %d, body=%s", code, body)
	}
	a, _ := db.GetAccount(id)
	if a.CooldownUntil != "" {
		t.Errorf("cooldown basi tidak dibersihkan: %q", a.CooldownUntil)
	}
}

// Regresi: 403 dengan 410004 HARUS di-cooldown, sedangkan 403 WAF block
// TIDAK boleh. Keduanya berstatus 403, jadi urutan percabangan penting —
// sebelumnya 410004 ikut jatuh ke cabang WAF dan tidak pernah di-cooldown.
func TestCooldown_403BannedCooldownsButWAFDoesNot(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCool bool
	}{
		{"410004 (akun diblokir)", `{"code":410004,"message":"账号已被封禁"}`, true},
		{"WAF forbidden", `{"message":"forbidden"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resetAccounts(t)
			idSick := addAcct(t, "sick", "tok-sick")
			addAcct(t, "healthy", "tok-healthy")

			up := upstreamByToken(map[string]struct {
				status int
				body   string
			}{
				"tok-sick":    {403, c.body},
				"tok-healthy": {200, okBody},
			})
			defer up.Close()

			s := newTestServer(t, up.URL)
			code, body := chatViaHandler(t, s, reqBody)
			if code != 200 {
				t.Fatalf("code = %d, want 200; body=%s", code, body)
			}

			a, err := db.GetAccount(idSick)
			if err != nil {
				t.Fatalf("GetAccount: %v", err)
			}
			cooling := a.IsCoolingDown(time.Now())
			if cooling != c.wantCool {
				t.Errorf("cooling = %v, want %v (reason=%q)",
					cooling, c.wantCool, a.CooldownReason)
			}
		})
	}
}

// healthz harus membedakan active (manual) vs usable (nyata).
func TestHealthz_ReportsUsableAndCooling(t *testing.T) {
	initTestDB(t)
	resetAccounts(t)
	addAcct(t, "fine", "tok-fine")
	idCool := addAcct(t, "cooling", "tok-cooling")
	if err := db.SetCooldown(idCool, db.CooldownBanned); err != nil {
		t.Fatalf("SetCooldown: %v", err)
	}

	s := newTestServer(t, "http://127.0.0.1:1")
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("healthz bukan JSON: %v", err)
	}
	if n, _ := out["accounts"].(float64); int(n) != 2 {
		t.Errorf("accounts = %v, want 2", out["accounts"])
	}
	if n, _ := out["usable"].(float64); int(n) != 1 {
		t.Errorf("usable = %v, want 1 (satu akun cooldown)", out["usable"])
	}
	if n, _ := out["cooling"].(float64); int(n) != 1 {
		t.Errorf("cooling = %v, want 1", out["cooling"])
	}
}

// Lama cooldown per kategori harus masuk akal (kuota < banned).
func TestCooldownDuration(t *testing.T) {
	if db.CooldownDuration(db.CooldownRateLimit) >= db.CooldownDuration(db.CooldownQuota) {
		t.Errorf("rate_limit harus lebih pendek dari quota")
	}
	if db.CooldownDuration(db.CooldownQuota) >= db.CooldownDuration(db.CooldownBanned) {
		t.Errorf("quota harus lebih pendek dari banned")
	}
	if db.CooldownDuration("unknown-reason") <= 0 {
		t.Errorf("kategori tak dikenal harus punya durasi positif")
	}
}
