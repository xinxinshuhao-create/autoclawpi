package server

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hirotomasato/autoclawpi/internal/client"
)

// WAF prefix tunggal harus dibuang, payload asli utuh.
func TestStripWAFPrefixes_Single(t *testing.T) {
	body := []byte(`{"message":"forbidden"}{"id":"x","choices":[]}`)
	got := stripWAFPrefixes(body)
	if string(got) != `{"id":"x","choices":[]}` {
		t.Fatalf("got %q", got)
	}
}

// Beberapa prefix berturut-turut (gateway bisa menempel berkali-kali).
func TestStripWAFPrefixes_Multiple(t *testing.T) {
	body := []byte(`{"message":"forbidden"}{"message":"forbidden"}{"id":"y"}`)
	got := stripWAFPrefixes(body)
	if string(got) != `{"id":"y"}` {
		t.Fatalf("got %q", got)
	}
}

// Body tanpa prefix WAF tidak boleh diubah.
func TestStripWAFPrefixes_NoPrefix(t *testing.T) {
	body := []byte(`{"id":"z","choices":[{"message":{"content":"hi"}}]}`)
	got := stripWAFPrefixes(body)
	if string(got) != string(body) {
		t.Fatalf("got %q", got)
	}
}

// REGRESI: content sah yang memuat "message":"forbidden" di TENGAH tidak boleh
// terpotong (bug lama pakai bytes.Index global).
func TestStripWAFPrefixes_MarkerInsideContent(t *testing.T) {
	payload := `{"id":"a","choices":[{"message":{"content":"text with \"message\":\"forbidden\" inside"}}]}`
	got := stripWAFPrefixes([]byte(payload))
	if string(got) != payload {
		t.Fatalf("content terpotong!\n got %q\nwant %q", got, payload)
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("hasil bukan JSON valid: %v", err)
	}
}

// REGRESI: truncateRunes tidak boleh membelah karakter CJK/emoji.
func TestTruncateRunes_NoSplit(t *testing.T) {
	for _, s := range []string{
		strings.Repeat("成功", 400), // CJK
		strings.Repeat("🙂", 400),  // emoji 4-byte
		strings.Repeat("é", 400),  // 2-byte
		strings.Repeat("a", 400),  // ASCII
		"中" + strings.Repeat("文", 300),
	} {
		out := truncateRunes(s, 500)
		if !utf8.ValidString(out) {
			t.Fatalf("output bukan UTF-8 valid untuk input %q...", []rune(s)[:5])
		}
		if n := utf8.RuneCountInString(out); n > 500 {
			t.Fatalf("runecount %d > 500", n)
		}
		// Harus benar-benar prefix dari input asli
		if !strings.HasPrefix(s, out) {
			t.Fatal("output bukan prefix input")
		}
	}
}

// Perbandingan langsung: cara byte-slice lama MEMANG merusak UTF-8.
func TestTruncateRunes_vs_ByteSlice(t *testing.T) {
	s := strings.Repeat("成功", 400) // 3 byte per rune
	old := s[:min(len(s), 500)]
	if utf8.ValidString(old) {
		t.Skip("kasus ini kebetulan jatuh di batas rune, tidak membuktikan apa-apa")
	}
	if !utf8.ValidString(truncateRunes(s, 500)) {
		t.Fatal("truncateRunes masih menghasilkan UTF-8 tidak valid")
	}
	t.Logf("bukti: byte-slice valid=%v, rune-slice valid=%v",
		utf8.ValidString(old), utf8.ValidString(truncateRunes(s, 500)))
}

// content kosong + ada reasoning -> reasoning DIBUANG, content tetap kosong.
//
// Regresi (2026-09-17): dulu fungsi ini memindahkan 500 rune pertama
// reasoning ke content saat content kosong. Itu menyamarkan proses berpikir
// sebagai jawaban — klien tidak bisa membedakannya dan tidak ada error yang
// bisa memicu retry. Sekarang deteksi+retry ditangani di handleChat
// (isEmptyCompletion + escalateBudget), bukan dengan mengisi content palsu.
func TestStripNonStandard_ReasoningNotMovedToContent(t *testing.T) {
	in := []byte(`{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"思考内容"},"finish_reason":"length"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"completion_tokens_details":{"reasoning_tokens":2}}}`)
	out := stripNonStandard(in)
	if out == nil {
		t.Fatal("stripNonStandard mengembalikan nil")
	}
	var d map[string]any
	if err := json.Unmarshal(out, &d); err != nil {
		t.Fatalf("bukan JSON: %v", err)
	}
	msg := d["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "" {
		t.Fatalf("content harus tetap kosong (bukan diisi reasoning), dapat %v", msg["content"])
	}
	if _, ok := msg["reasoning_content"]; ok {
		t.Fatal("reasoning_content masih ada")
	}
	usage := d["usage"].(map[string]any)
	if _, ok := usage["completion_tokens_details"]; ok {
		t.Fatal("completion_tokens_details masih ada")
	}
}

// content tidak kosong -> reasoning dibuang, content TIDAK ditimpa.
func TestStripNonStandard_KeepsContent(t *testing.T) {
	in := []byte(`{"choices":[{"message":{"role":"assistant","content":"成功","reasoning_content":"thinking..."}}]}`)
	out := stripNonStandard(in)
	var d map[string]any
	json.Unmarshal(out, &d)
	msg := d["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "成功" {
		t.Fatalf("content tertimpa: %v", msg["content"])
	}
	if _, ok := msg["reasoning_content"]; ok {
		t.Fatal("reasoning_content masih ada")
	}
}

// Body yang bukan JSON (WAF mentah) -> nil, agar handleChat failover.
func TestStripNonStandard_NonJSON(t *testing.T) {
	if stripNonStandard([]byte(`{"message":"forbidden"}`)) != nil {
		t.Fatal("WAF-only body seharusnya nil")
	}
	if stripNonStandard([]byte(``)) != nil {
		t.Fatal("body kosong seharusnya nil")
	}
	if stripNonStandard([]byte(`not json at all`)) != nil {
		t.Fatal("non-JSON seharusnya nil")
	}
}

// RouteID memetakan 6 model terverifikasi ke route id upstream.
// BodyModel adalah kebalikannya; untuk deepseek ia mengembalikan bentuk
// ber-suffix tanggal (deepseek-v4-pro-202606). Itu BUKAN bug: diuji langsung
// ke upstream, body model ber-suffix maupun tanpa suffix dua-duanya 200 —
// routing sebenarnya ditentukan oleh header X-Request-Model, bukan body.
func TestRouteID_VerifiedModels(t *testing.T) {
	cases := map[string]string{
		"auto":              "zai_auto",
		"auto-fast":         "zai_auto-fast",
		"glm-5.3":           "zaicoding_glm-5.3",
		"glm-5.3-flash":     "zai_glm-5.3-flash",
		"deepseek-v4-pro":   "tdpsk_deepseek-v4-pro-202606",
		"deepseek-v4-flash": "tdpsk_deepseek-v4-flash-202605",
	}
	for model, wantRoute := range cases {
		got := client.RouteID(model)
		if got != wantRoute {
			t.Errorf("RouteID(%q) = %q, want %q", model, got, wantRoute)
		}
	}

	// BodyModel: strip prefix sebelum "_" pertama. Untuk zai/zaicoding hasilnya
	// persis nama model; untuk tdpsk menyisakan suffix tanggal.
	bodyCases := map[string]string{
		"zai_auto":                       "auto",
		"zai_auto-fast":                  "auto-fast",
		"zaicoding_glm-5.3":              "glm-5.3",
		"zai_glm-5.3-flash":              "glm-5.3-flash",
		"tdpsk_deepseek-v4-pro-202606":   "deepseek-v4-pro-202606",
		"tdpsk_deepseek-v4-flash-202605": "deepseek-v4-flash-202605",
	}
	for route, wantBody := range bodyCases {
		if got := client.BodyModel(route); got != wantBody {
			t.Errorf("BodyModel(%q) = %q, want %q", route, got, wantBody)
		}
	}
}

// upstreamErrCode harus mengenali bentuk langsung (top-level "code").
func TestUpstreamErrCode_Direct(t *testing.T) {
	body := []byte(`{"action":{"kind":"pay-view"},"code":810002,` +
		`"message":"We're experiencing high demand right now."}`)
	if got := upstreamErrCode(body); got != 810002 {
		t.Fatalf("upstreamErrCode = %d, want 810002", got)
	}
}

// ...dan bentuk terbungkus {"error":{"message":"{...}"}} yang dipakai
// autoclawpi saat meneruskan error ke hilir.
func TestUpstreamErrCode_Wrapped(t *testing.T) {
	inner := `{\"code\":810002,\"message\":\"high demand\"}`
	body := []byte(`{"error":{"message":"` + inner + `","type":"upstream_error"}}`)
	if got := upstreamErrCode(body); got != 810002 {
		t.Fatalf("upstreamErrCode = %d, want 810002", got)
	}
}

// Prefix WAF tidak boleh menyembunyikan code.
func TestUpstreamErrCode_WithWAFPrefix(t *testing.T) {
	body := []byte(`{"message":"forbidden"}{"code":810002,"message":"high demand"}`)
	if got := upstreamErrCode(body); got != 810002 {
		t.Fatalf("upstreamErrCode = %d, want 810002", got)
	}
}

// Error non-810002 (mis. 410004 akun diblokir) jangan ikut diremap.
func TestUpstreamErrCode_Other(t *testing.T) {
	body := []byte(`{"code":410004,"message":"akun diblokir"}`)
	if got := upstreamErrCode(body); got != 410004 {
		t.Fatalf("upstreamErrCode = %d, want 410004", got)
	}
	// Body yang bukan JSON / tanpa code -> 0
	for _, b := range []string{``, `not json`, `{"foo":"bar"}`, `{"error":{}}`} {
		if got := upstreamErrCode([]byte(b)); got != 0 {
			t.Errorf("upstreamErrCode(%q) = %d, want 0", b, got)
		}
	}
}
