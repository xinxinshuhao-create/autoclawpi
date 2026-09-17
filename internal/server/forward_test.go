package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/hirotomasato/autoclawpi/internal/client"
	"github.com/hirotomasato/autoclawpi/internal/db"
)

// newTestServer membuat Server yang menunjuk ke upstream palsu.
//
// logUsage() dipanggil di goroutine latar dan menyentuh db.DB, jadi tes perlu
// DB nyata (tempdir) supaya tidak nil-pointer.
func newTestServer(t *testing.T, upstreamBase string) *Server {
	t.Helper()
	initTestDB(t)
	cl := client.New(upstreamBase, upstreamBase)
	return New(cl)
}

var testDBOnce sync.Once

// initTestDB menyiapkan DB sekali untuk seluruh paket. Pakai os.MkdirTemp
// (bukan t.TempDir) supaya direktori TIDAK dihapus otomatis oleh testing:
// logUsage() berjalan di goroutine latar dan masih memegang handle SQLite
// saat tes selesai, sehingga RemoveAll gagal ("being used by another process").
func initTestDB(t *testing.T) {
	t.Helper()
	testDBOnce.Do(func() {
		dir, err := os.MkdirTemp("", "autoclawpi-test-*")
		if err != nil {
			t.Fatalf("MkdirTemp: %v", err)
		}
		if err := db.Init(dir); err != nil {
			t.Fatalf("db.Init: %v", err)
		}
	})
}

// testAccount membuat akun dummy untuk forward().
func testAccount() db.Account {
	return db.Account{
		ID:          1,
		Name:        "test",
		AccessToken: "Bearer test-token",
		Active:      true,
	}
}

// forward() harus me-remap 403+810002 menjadi 429 supaya CPA memperlakukannya
// sebagai rate limit, bukan payment_required (yang menangguhkan seluruh channel).
//
// Tes ini deterministik: upstream palsu selalu membalas 403+810002.
func TestForward_Remaps810002To429(t *testing.T) {
	const upstreamBody = `{"action":{"kind":"pay-view"},"code":810002,` +
		`"image_url":"https://example.invalid/x.png",` +
		`"message":"We're experiencing high demand right now."}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer upstream.Close()

	s := newTestServer(t, upstream.URL)

	acct := testAccount()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")

	status, _, rc, err := s.forward(req.Context(), acct, "zai_auto",
		[]byte(`{"model":"zai_auto","messages":[{"role":"user","content":"hi"}]}`), false)
	body := readAndClose(rc)
	if err != nil {
		t.Fatalf("forward error: %v", err)
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (remap dari 403+810002); body=%s", status, body)
	}
	// Body asli harus diteruskan apa adanya (kode upstream tetap terlihat).
	if !strings.Contains(string(body), "810002") {
		t.Errorf("body tidak memuat 810002: %s", body)
	}
}

// 403 tanpa 810002 (mis. WAF block / akun diblokir) TIDAK boleh diremap ke 429 —
// harus tetap 403 supaya handleChat failover ke akun berikutnya.
func TestForward_Keeps403Without810002(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":410004,"message":"akun diblokir"}`))
	}))
	defer upstream.Close()

	s := newTestServer(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{}`))

	status, _, rc, err := s.forward(req.Context(), testAccount(), "zai_auto",
		[]byte(`{"model":"zai_auto"}`), false)
	body := readAndClose(rc)
	if err != nil {
		t.Fatalf("forward error: %v", err)
	}
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", status, body)
	}
}

// 403 dengan 810002 yang DIBUNGKUS {"error":{"message":"{...}"}} juga harus
// diremap — bentuk inilah yang beredar di jalur CPA.
func TestForward_RemapsWrapped810002(t *testing.T) {
	inner := `{\"code\":810002,\"message\":\"high demand\"}`
	upstreamBody := `{"error":{"message":"` + inner + `","type":"upstream_error"}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer upstream.Close()

	s := newTestServer(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{}`))

	status, _, rc, err := s.forward(req.Context(), testAccount(), "zai_auto",
		[]byte(`{"model":"zai_auto"}`), false)
	readAndClose(rc)
	if err != nil {
		t.Fatalf("forward error: %v", err)
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (wrapped 810002)", status)
	}
}

// Sanity: 200 upstream tetap 200, body di-clean, reasoning_content dibuang
// (TIDAK dipindahkan ke content — lihat stripNonStandard).
func TestForward_Success200(t *testing.T) {
	ok := `{"id":"x","object":"chat.completion","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"OK","reasoning_content":"thinking"},` +
		`"finish_reason":"stop"}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(ok))
	}))
	defer upstream.Close()

	s := newTestServer(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{}`))

	status, _, rc, err := s.forward(req.Context(), testAccount(), "zai_auto",
		[]byte(`{"model":"zai_auto"}`), false)
	body := readAndClose(rc)
	if err != nil {
		t.Fatalf("forward error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	cleaned := stripNonStandard(body)
	var out map[string]any
	if err := json.Unmarshal(cleaned, &out); err != nil {
		t.Fatalf("body bukan JSON valid: %v (%s)", err, cleaned)
	}
	if !strings.Contains(string(cleaned), `"content":"OK"`) {
		t.Errorf("content hilang: %s", cleaned)
	}
	if strings.Contains(string(cleaned), "reasoning_content") {
		t.Errorf("reasoning_content tidak dibuang: %s", cleaned)
	}
}

// TestStripNonStandard_DoesNotFakeContent: content kosong TIDAK boleh diisi
// dengan potongan reasoning.
//
// Regresi: dulu ada fallback yang menyalin 500 rune pertama reasoning ke
// content saat content kosong. Itu membuat klien menerima proses berpikir
// sebagai "jawaban", dan karena tidak error, tidak ada yang bisa memicu
// retry. Sekarang reasoning dibuang; deteksi+retry ada di handleChat.
func TestStripNonStandard_DoesNotFakeContent(t *testing.T) {
	in := `{"choices":[{"index":0,"finish_reason":"length",` +
		`"message":{"role":"assistant","content":"",` +
		`"reasoning_content":"Pertama saya hitung 45 x 0.6 = 27 ..."}}]}`
	out := stripNonStandard([]byte(in))
	if out == nil {
		t.Fatal("stripNonStandard mengembalikan nil")
	}
	var d map[string]any
	if err := json.Unmarshal(out, &d); err != nil {
		t.Fatalf("output bukan JSON: %v", err)
	}
	msg := d["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if c, _ := msg["content"].(string); c != "" {
		t.Errorf("content harus tetap kosong, dapat %q", c)
	}
	if _, ok := msg["reasoning_content"]; ok {
		t.Error("reasoning_content harus dibuang")
	}
}

// TestIsEmptyCompletion: hanya finish_reason=length + content ~kosong yang
// dianggap "thinking menghabiskan budget".
func TestIsEmptyCompletion(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"length + content kosong -> ya",
			`{"choices":[{"finish_reason":"length","message":{"content":""}}]}`, true},
		{"length + content whitespace saja -> ya",
			`{"choices":[{"finish_reason":"length","message":{"content":"  \n "}}]}`, true},
		{"length + content panjang -> TIDAK (pemotongan biasa)",
			`{"choices":[{"finish_reason":"length","message":{"content":"ini jawaban panjang yang terpotong"}}]}`, false},
		{"stop + content kosong -> TIDAK (bukan kasus budget)",
			`{"choices":[{"finish_reason":"stop","message":{"content":""}}]}`, false},
		{"stop + content ada -> TIDAK",
			`{"choices":[{"finish_reason":"stop","message":{"content":"OK"}}]}`, false},
		{"bukan JSON -> TIDAK", `not json`, false},
		{"choices kosong -> TIDAK", `{"choices":[]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isEmptyCompletion([]byte(tc.body)); got != tc.want {
				t.Errorf("isEmptyCompletion = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEscalateBudget: retry menaikkan budget x4, dibatasi cap, tidak menurunkan.
func TestEscalateBudget(t *testing.T) {
	// 1000 x 4 = 4000
	got := escalateBudget([]byte(`{"max_tokens":1000}`), 4)
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatal(err)
	}
	if v, _ := m["max_tokens"].(float64); v != 4000 {
		t.Errorf("max_tokens = %v, want 4000", v)
	}

	// Di atas cap -> dijepit ke cap
	got = escalateBudget([]byte(`{"max_tokens":20000}`), 4)
	_ = json.Unmarshal(got, &m)
	if v, _ := m["max_tokens"].(float64); v != reasoningHeadroomCap {
		t.Errorf("max_tokens = %v, want %d (cap)", v, reasoningHeadroomCap)
	}

	// Sudah di cap -> tidak berubah
	in := []byte(`{"max_tokens":32000}`)
	if string(escalateBudget(in, 4)) != string(in) {
		t.Error("body di cap seharusnya tidak berubah")
	}

	// Tanpa field budget -> tidak berubah (tidak ada yang bisa dinaikkan)
	in2 := []byte(`{"model":"x"}`)
	if string(escalateBudget(in2, 4)) != string(in2) {
		t.Error("tanpa budget seharusnya tidak berubah")
	}
}
