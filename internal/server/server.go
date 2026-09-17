// Package server menyediakan endpoint OpenAI-compatible yang
// meneruskan request ke proxy inference AutoClaw dengan round-robin multi-akun.
package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hirotomasato/autoclawpi/internal/client"
	"github.com/hirotomasato/autoclawpi/internal/db"
)

// Server adalah proxy server OpenAI-compatible.
type Server struct {
	cl      *client.Client
	rrIndex int
	mu      sync.Mutex
	apiKey  string
	limiter *RateLimiter
}

// New membuat server baru.
func New(cl *client.Client) *Server {
	return &Server{
		cl:      cl,
		rrIndex: 0,
		limiter: NewRateLimiter(1.0/1.5, 3), // default: 1 req/1.5s, burst 3
	}
}

// WithRateLimit mengatur rate limiter (token/detik + burst).
func (s *Server) WithRateLimit(ratePerSec float64, burst int) *Server {
	if ratePerSec > 0 && burst > 0 {
		s.limiter = NewRateLimiter(ratePerSec, burst)
	}
	return s
}

// WithAPIKey mengatur API key untuk proteksi endpoint.
func (s *Server) WithAPIKey(key string) *Server {
	if key != "" {
		s.apiKey = key
	}
	return s
}

// Handler mengembalikan http.Handler yang menangani route OpenAI.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.wrapAuth(s.handleChat))
	mux.HandleFunc("/v1/models", s.wrapAuth(s.handleModels))
	mux.HandleFunc("/healthz", s.handleHealth)
	return mux
}

// wrapAuth melindungi endpoint dengan API key jika dikonfigurasi.
func (s *Server) wrapAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiKey != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if got != s.apiKey {
				writeOpenAIError(w, 401, "invalid_api_key", "API key lokal salah")
				return
			}
		}
		next(w, r)
	}
}

// handleChat menangani /v1/chat/completions.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	_ = r.Body.Close()
	if err != nil {
		writeOpenAIError(w, 400, "invalid_request_error", "gagal baca body: "+err.Error())
		return
	}

	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeOpenAIError(w, 400, "invalid_request_error", "gagal parse body: "+err.Error())
		return
	}
	if req.Model == "" {
		writeOpenAIError(w, 400, "invalid_request_error", "model required")
		return
	}

	route := client.RouteID(req.Model)
	upstreamBody, err := replaceModel(raw, client.BodyModel(route))
	if err != nil {
		writeOpenAIError(w, 400, "invalid_request_error", "gagal olah body: "+err.Error())
		return
	}

	// Reasoning (thinking) memakai budget max_tokens yang SAMA dengan jawaban.
	// Kalau budgetnya pas-pasan, model kehabisan jatah sebelum menulis jawaban
	// -> content kosong, finish_reason=length. Tambahkan headroom di muka.
	//
	// JANGAN matikan thinking sebagai "solusi":实测 thinking=disabled /
	// enable_thinking=false justru membuat reasoning membengkak ~8x
	// (2262 -> 18127 rune) dan content tetap kosong.
	upstreamBody = addReasoningHeadroom(upstreamBody)

	// Round-robin: coba setiap akun hingga salah satu berhasil
	accounts, _ := db.ListAccounts()
	if len(accounts) == 0 {
		writeOpenAIError(w, 503, "no_accounts", "tidak ada akun tersedia")
		return
	}

	// Rate limit: tunggu token sebelum menyentuh upstream (hindari WAF block)
	if s.limiter != nil {
		if err := s.limiter.Wait(r.Context()); err != nil {
			writeOpenAIError(w, 503, "rate_limited", "request dibatalkan: "+err.Error())
			return
		}
	}

	s.mu.Lock()
	startIdx := s.rrIndex % len(accounts)
	s.rrIndex = (s.rrIndex + 1) % len(accounts)
	s.mu.Unlock()

	// Simpan kegagalan terakhir supaya pesan error ke klien informatif,
	// bukan cuma "semua akun gagal".
	lastStatus := 0
	var lastBody []byte

	now := time.Now()
	for i := 0; i < len(accounts); i++ {
		idx := (startIdx + i) % len(accounts)
		acct := accounts[idx]
		// Lewati akun nonaktif manual, tanpa token, atau sedang cooldown.
		if !acct.Usable(now) {
			continue
		}

		status, ct, rc, ferr := s.forward(r.Context(), acct, route, upstreamBody, req.Stream)
		if ferr != nil {
			log.Printf("[autoclawpi] akun #%d error: %v", acct.ID, ferr)
			// Kalau jalur keluarnya yang mati (node/proxy), tandai cooldown
			// pendek. Tanpa ini akun tersebut akan dicoba lagi di setiap
			// request, gagal, baru failover — semua request melambat dan log
			// dipenuhi error yang sama.
			if isEgressError(ferr) {
				s.cooldown(&acct, db.CooldownNetwork, nil)
			}
			continue
		}
		// 401 — token expired, coba refresh
		if status == 401 {
			body := readAndClose(rc)
			lastStatus, lastBody = status, body
			refreshed, rerr := s.refreshToken(&acct)
			if rerr != nil {
				log.Printf("[autoclawpi] akun #%d refresh gagal: %v", acct.ID, rerr)
				s.cooldown(&acct, db.CooldownUnauthorized, body)
				continue
			}
			// Retry dengan token baru (tulis ke variabel luar supaya
			// penanganan status di bawah memakai respons percobaan kedua).
			var ferr2 error
			status, ct, rc, ferr2 = s.forward(r.Context(), acct, route, upstreamBody, req.Stream)
			if ferr2 != nil {
				log.Printf("[autoclawpi] akun #%d retry error: %v", acct.ID, ferr2)
				if isEgressError(ferr2) {
					s.cooldown(&acct, db.CooldownNetwork, nil)
				}
				continue
			}
			_ = refreshed
		}
		// 403 bisa dua rupa: WAF block ({"message":"forbidden"}) ATAU error
		// tingkat akun yang kebetulan berstatus 403 (410004 账号已被封禁).
		// Keduanya harus dibedakan — yang pertama masalah global (jangan
		// cooldown), yang kedua milik akun ini (harus cooldown).
		if status == 403 {
			body := readAndClose(rc)
			lastStatus, lastBody = status, body
			if reason := classifyUpstreamError(status, body); reason != "" {
				log.Printf("[autoclawpi] akun #%d 403 akun-level (%s), cooldown", acct.ID, reason)
				s.cooldown(&acct, reason, body)
				continue
			}
			log.Printf("[autoclawpi] akun #%d WAF block, coba akun berikutnya", acct.ID)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if status == 200 {
			// Akun ini sehat — bersihkan cooldown basi bila ada.
			if acct.CooldownUntil != "" {
				_ = db.ClearCooldown(acct.ID)
			}
			// === Non-streaming: deteksi "thinking menghabiskan budget" ===
			// Respons belum dikirim ke klien, jadi masih bisa diulang.
			if !req.Stream {
				rawBody := readAndClose(rc)
				cleaned := stripNonStandard(rawBody)
				if cleaned == nil {
					// Bukan JSON (WAF block / respons rusak) — perlakukan
					// sebagai kegagalan akun supaya failover jalan.
					log.Printf("[autoclawpi] respons tidak valid: %s", truncateResp(rawBody, 120))
					lastStatus, lastBody = http.StatusForbidden, rawBody
					continue
				}
				// Percobaan ulang terbatas saat content kosong karena budget.
				// Tidak menyentuh reasoning_effort / thinking: kualitas
				// penalaran tidak dikorbankan, hanya budgetnya dinaikkan.
				// (Pendekatan sama dengan thinkgate untuk vLLM.)
				for attempt := 1; attempt <= emptyRetryMax && isEmptyCompletion(rawBody); attempt++ {
					nextBody := escalateBudget(upstreamBody, emptyRetryMultiplier)
					if bytes.Equal(nextBody, upstreamBody) {
						break // sudah di cap, mengulang tidak akan menolong
					}
					log.Printf("[autoclawpi] akun #%d content kosong (finish=length), "+
						"retry %d/%d dengan budget x%d", acct.ID, attempt, emptyRetryMax,
						emptyRetryMultiplier)
					upstreamBody = nextBody
					st2, ct2, rc2, e2 := s.forward(r.Context(), acct, route, upstreamBody, false)
					if e2 != nil {
						log.Printf("[autoclawpi] akun #%d retry kosong gagal: %v", acct.ID, e2)
						break
					}
					if st2 != 200 {
						readAndClose(rc2)
						break
					}
					raw2 := readAndClose(rc2)
					cleaned2 := stripNonStandard(raw2)
					if cleaned2 == nil {
						break
					}
					rawBody, cleaned, ct = raw2, cleaned2, ct2
				}
				w.Header().Set("Content-Type", ct)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(cleaned)
				go logUsage(acct.ID, route, rawBody)
				return
			}
			// === Streaming ===
			// Kasus "content kosong" pada streaming terlihat di akhir stream,
			// saat sebagian data sudah terkirim — mengulang akan menggandakan
			// output. Jadi SSE diteruskan apa adanya (tetap difilter
			// reasoning_content). Deteksi+retry hanya untuk non-streaming.
			w.Header().Set("Content-Type", ct)
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			if err := streamFiltered(w, rc, flusher); err != nil {
				log.Printf("[autoclawpi] stream terputus: %v", err)
			}
			_ = rc.Close()
			return
		}
		// Status lain (4xx, 5xx) — klasifikasikan lalu coba akun berikutnya.
		body := readAndClose(rc)
		lastStatus, lastBody = status, body
		if reason := classifyUpstreamError(status, body); reason != "" {
			s.cooldown(&acct, reason, body)
		}
	}

	// Semua akun gagal — teruskan status & pesan upstream bila ada.
	if lastStatus != 0 {
		msg := strings.TrimSpace(string(lastBody))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		log.Printf("[autoclawpi] semua akun gagal, upstream status=%d body=%s", lastStatus, msg)
		writeOpenAIError(w, lastStatus, "upstream_error", msg)
		return
	}
	writeOpenAIError(w, 503, "all_accounts_failed", "semua akun gagal memproses request")
}

// forward mengirim request ke upstream dan mengembalikan respons MENTAH —
// belum ada satu byte pun yang ditulis ke klien.
//
// Kenapa tidak menulis langsung: kasus "thinking menghabiskan seluruh budget"
// hanya terlihat SETELAH respons selesai (finish_reason=length + content
// kosong). Kalau respons sudah terlanjur dikirim, tidak ada yang bisa
// diulang. Jadi pemanggil (handleChat) yang memutuskan: kirim, ulangi, atau
// buang.
//
// Return:
//   - err != nil    -> kegagalan jaringan/permintaan (bukan respons HTTP)
//   - status != 200 -> respBody berisi body error; pemanggil WAJIB menutup
//   - status == 200 -> respBody berisi body penuh (non-stream) atau
//     stream SSE; pemanggil WAJIB menutup
func (s *Server) forward(ctx context.Context, acct db.Account, route string, body []byte, stream bool) (int, string, io.ReadCloser, error) {
	baseURL := s.cl.InferenceBase
	if baseURL == "" {
		baseURL = "https://autoglm-api.autoglm.ai"
	}

	url := baseURL + "/autoclaw-proxy/proxy/autoclaw/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return 0, "", nil, fmt.Errorf("buat request: %w", err)
	}

	headers := s.cl.InferenceHeader(acct.AccessToken, route)
	headers["Content-Type"] = "application/json"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}

	resp, err := s.cl.DoFor(req, acct.Proxy)
	if err != nil {
		return 0, "", nil, fmt.Errorf("request: %w", err)
	}

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}

	// Non-200: baca body error, tutup stream, kembalikan ke handleChat
	// supaya bisa failover ke akun berikutnya.
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		// Upstream membalas 403 + code 810002 untuk "high demand" (antrean
		// sementara), bukan pembayaran. CPA memetakan 403 -> payment_required
		// lalu MENANGGUHKAN seluruh channel (semua model), jadi salah klasifikasi
		// ini mahal. Remap ke 429 supaya diperlakukan sebagai rate limit.
		if code := upstreamErrCode(b); code == 810002 {
			log.Printf("[autoclawpi] upstream 810002 (high demand) -> remap 403 ke 429")
			return http.StatusTooManyRequests, ct, io.NopCloser(bytes.NewReader(b)), nil
		}
		return resp.StatusCode, ct, io.NopCloser(bytes.NewReader(b)), nil
	}

	return resp.StatusCode, ct, resp.Body, nil
}

// readAndClose membaca seluruh isi rc (dibatasi) lalu menutupnya.
func readAndClose(rc io.ReadCloser) []byte {
	if rc == nil {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(rc, 1<<20))
	_ = rc.Close()
	return b
}

// streamFiltered meneruskan SSE dari upstream ke klien, sambil membuang
// field reasoning_content dari tiap delta.
//
// Kenapa perlu: jalur non-streaming sudah membersihkan reasoning_content
// lewat stripNonStandard, tapi jalur streaming dulu hanya copy byte mentah.
// Akibatnya klien OpenAI-compatible yang ketat bisa menampilkan proses
// berpikir model sebagai jawaban, atau gagal parse karena field tak dikenal.
//
// Cara kerja: baca per-baris (SSE dipisah newline). Baris yang bukan "data:"
// (event:, id:, komentar, baris kosong pemisah) diteruskan apa adanya. Baris
// "data:" diparse sebagai JSON; kalau valid, reasoning_content dibuang lalu
// diserialisasi ulang. Kalau JSON tidak valid (upstream aneh / chunk terpotong),
// baris aslinya diteruskan supaya tidak ada data yang hilang.
func streamFiltered(w io.Writer, src io.Reader, flusher http.Flusher) error {
	br := bufio.NewReaderSize(src, 32*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			out := cleanSSEDataLine(line)
			if _, werr := w.Write(out); werr != nil {
				return werr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// cleanSSEDataLine menghapus reasoning_content dari satu baris SSE.
// Baris yang tidak diawali "data:" dikembalikan apa adanya (termasuk newline).
func cleanSSEDataLine(line []byte) []byte {
	trimmed := bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return line
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return line
	}

	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return line // bukan JSON valid -> teruskan apa adanya
	}

	choices, ok := root["choices"].([]any)
	if !ok {
		return line
	}
	changed := false
	for _, c := range choices {
		choice, ok := c.(map[string]any)
		if !ok {
			continue
		}
		// Delta (streaming) dan message (kalau upstream kirim penuh).
		for _, key := range []string{"delta", "message"} {
			m, ok := choice[key].(map[string]any)
			if !ok {
				continue
			}
			if _, has := m["reasoning_content"]; has {
				delete(m, "reasoning_content")
				changed = true
			}
		}
	}
	if !changed {
		return line
	}

	cleaned, err := json.Marshal(root)
	if err != nil {
		return line
	}
	// Pertahankan newline asli supaya framing SSE tidak rusak.
	suffix := line[len(trimmed):]
	return append(append([]byte("data: "), cleaned...), suffix...)
}

// reasoningHeadroom adalah budget tambahan untuk proses berpikir (thinking).
// Diukur dari data nyata: pada prompt "tulis esai 800 kata", reasoning normal
// mencapai ~2262 rune (≈1300 token); kalau thinking dimatikan (yang justru
// salah) membengkak sampai ~18127 rune (≈4000 token habis).
// 2048 token memberi ruang lega tanpa mengubah perilaku model.
const reasoningHeadroom = 2048

// reasoningHeadroomCap adalah plafon budget setelah ditambah headroom.
// Diukur: upstream menerima max_tokens sampai 32000 (diuji 1000..32000, semua
// 200 kecuali satu EOF transien). Jadi cap 4000 terlalu ketat — request dengan
// max_tokens=4000 (nilai yang justru memicu content kosong) tidak akan dapat
// headroom sama sekali. 32000 dipakai sebagai batas aman.
const reasoningHeadroomCap = 32000

// addReasoningHeadroom menambahkan headroom ke max_tokens / max_completion_tokens
// supaya proses berpikir tidak menghabiskan jatah jawaban.
//
// Perilaku:
//   - Tidak ada field budget  -> tambahkan max_tokens = headroom
//     (kalau klien tidak menyebut budget, dia tidak peduli panjangnya)
//   - Ada budget              -> naikkan sebesar headroom, dibatasi cap
//   - max_tokens=0 / absen    -> diperlakukan sebagai tidak ada
//
// Body yang bukan JSON valid dikembalikan apa adanya (biar jalur error
// upstream yang melaporkannya, bukan kita).
func addReasoningHeadroom(body []byte) []byte {
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return body
	}

	applied := false
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		raw, ok := data[key]
		if !ok {
			continue
		}
		f, ok := raw.(float64)
		if !ok {
			continue
		}
		// 0 / negatif = klien tidak benar-benar menetapkan budget.
		// Pakai headroom, jangan teruskan 0 ke upstream.
		if f <= 0 {
			data[key] = reasoningHeadroom
			applied = true
			continue
		}
		// Jangan pernah MENURUNKAN budget yang klien minta secara eksplisit;
		// kalau sudah di atas cap, biarkan apa adanya.
		if f >= reasoningHeadroomCap {
			applied = true
			continue
		}
		next := f + reasoningHeadroom
		if next > reasoningHeadroomCap {
			next = reasoningHeadroomCap
		}
		data[key] = next
		applied = true
	}

	// Klien tidak menyebut budget sama sekali -> pakai headroom sebagai budget.
	if !applied {
		if _, hasMT := data["max_tokens"]; !hasMT {
			if _, hasMCT := data["max_completion_tokens"]; !hasMCT {
				data["max_tokens"] = reasoningHeadroom
			}
		}
	}

	out, err := json.Marshal(data)
	if err != nil {
		return body
	}
	return out
}

// emptyContentThreshold: beberapa karakter nyasar sebelum cutoff secara
// fungsional sama dengan content kosong. 8 byte cukup untuk whitespace/tanda
// baca nyasar tanpa salah menandai jawaban pendek yang sah.
// (Angka yang sama dipakai thinkgate — pola kegagalan ini identik.)
const emptyContentThreshold = 8

// emptyRetryMax adalah jumlah maksimum percobaan ulang saat mendeteksi
// "thinking menghabiskan seluruh budget".
// 1 sudah cukup: penyebabnya budget, dan budget sudah dinaikkan 4x.
const emptyRetryMax = 1

// emptyRetryMultiplier: pada retry, budget dikalikan sekian.
// Mengikuti pendekatan thinkgate untuk vLLM ("retries once with max_tokens
// multiplied by 4") — bekerja tanpa mematikan thinking, jadi kualitas
// penalaran tidak dikorbankan.
const emptyRetryMultiplier = 4

// isEmptyCompletion mendeteksi pola "model berpikir sampai kehabisan budget,
// lalu tidak menulis jawaban sama sekali".
//
// Tanda tangan (semua harus benar):
//   - finish_reason == "length"  -> budget habis, bukan berhenti wajar
//   - content ~kosong (< 8 char) -> tidak ada jawaban yang ditulis
//
// Kenapa harus dua-duanya: finish_reason=length dengan content yang panjang
// itu pemotongan biasa (jawaban sah tapi kepanjangan) — TIDAK boleh di-retry,
// karena mengulang hanya akan memotong di tempat yang sama.
//
// Respons yang bukan JSON / tidak punya choices dianggap bukan kasus ini.
func isEmptyCompletion(body []byte) bool {
	body = stripWAFPrefixes(body)
	if len(body) == 0 {
		return false
	}
	var data struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return false
	}
	if len(data.Choices) == 0 {
		return false
	}
	ch := data.Choices[0]
	if ch.FinishReason != "length" {
		return false
	}
	return len(strings.TrimSpace(ch.Message.Content)) < emptyContentThreshold
}

// escalateBudget menaikkan max_tokens / max_completion_tokens sebesar
// multiplier kali untuk percobaan ulang.
//
// Berbeda dari addReasoningHeadroom (yang menambah jumlah tetap), fungsi ini
// dipakai HANYA pada retry: kalau percobaan pertama sudah kehabisan budget,
// menambah sedikit tidak cukup — reasoning terbukti tumbuh mengikuti budget.
//
// Tetap dibatasi reasoningHeadroomCap supaya tidak meminta di luar yang
// diterima upstream. Body non-JSON dikembalikan apa adanya.
func escalateBudget(body []byte, multiplier int) []byte {
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return body
	}
	changed := false
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		raw, ok := data[key]
		if !ok {
			continue
		}
		f, ok := raw.(float64)
		if !ok || f <= 0 {
			continue
		}
		next := f * float64(multiplier)
		if next > reasoningHeadroomCap {
			next = reasoningHeadroomCap
		}
		if next <= f {
			// Sudah di cap — tidak ada ruang menaikkan.
			continue
		}
		data[key] = next
		changed = true
	}
	// Kalau klien tidak menyebut budget sama sekali, tidak ada yang bisa
	// dinaikkan secara berarti (addReasoningHeadroom sudah memberi default).
	if !changed {
		return body
	}
	out, err := json.Marshal(data)
	if err != nil {
		return body
	}
	return out
}

// stripNonStandard removes non-OpenAI fields from chat completion response.
//
// WAF prefix hanya muncul sebagai objek {"message":"forbidden"} di AWAL body.
// Jangan pakai bytes.Index global: kalau model kebetulan mengembalikan teks
// yang mengandung `"message":"forbidden"` di dalam content, pencarian global
// akan memotong response yang sah.
//
// PENTING: fungsi ini TIDAK memindahkan reasoning_content ke content.
// Dulu ada fallback yang menyalin 500 rune pertama reasoning ke content saat
// content kosong. Itu berbahaya: klien menerima potongan proses berpikir
// sebagai "jawaban" tanpa tahu bedanya — dan karena tidak error, tidak ada
// yang bisa memicu retry. Deteksi + retry ditangani di handleChat
// (isEmptyCompletion), bukan dengan menyamarkan reasoning sebagai jawaban.
//
// Dipakai HANYA di jalur non-streaming; untuk streaming lihat streamFiltered.
func stripNonStandard(body []byte) []byte {
	body = stripWAFPrefixes(body)
	if len(body) == 0 {
		return nil
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil
	}
	// Remove reasoning_content from choices (dibuang, tidak dipindahkan).
	if choices, ok := data["choices"].([]any); ok {
		for _, c := range choices {
			if choice, ok := c.(map[string]any); ok {
				if msg, ok := choice["message"].(map[string]any); ok {
					delete(msg, "reasoning_content")
				}
			}
		}
	}
	// Remove non-standard usage details
	if usage, ok := data["usage"].(map[string]any); ok {
		delete(usage, "completion_tokens_details")
		delete(usage, "prompt_tokens_details")
	}
	cleaned, _ := json.Marshal(data)
	return cleaned
}

// isEgressError menentukan apakah kegagalan request berasal dari jalur keluar
// (node proxy mati / tidak bisa dihubungi), bukan dari request itu sendiri.
//
// Kenapa penting dipisahkan: kalau request body-nya yang salah (JSON rusak,
// model tidak dikenal), menandai cooldown akan menonaktifkan akun sehat —
// padahal request berikutnya mungkin baik-baik saja. Hanya error jaringan
// yang boleh memicu cooldown.
//
// Yang dianggap error egress (proxy mati):
//   - connection refused / reset / no route / network unreachable
//   - proxyconnect tcp: ... (mihomo mati / port listener tutup)
//   - dial tcp ...: i/o timeout / context deadline exceeded (node tidak respons)
//   - EOF / unexpected EOF saat handshake lewat proxy
//
// Yang TIDAK dianggap egress (jangan cooldown):
//   - "buat request: ..." (request gagal dibuat → bug body/URL)
//   - context canceled (klien membatalkan sendiri)
func isEgressError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())

	// Klien yang membatalkan bukan salah akun/node.
	if strings.Contains(msg, "context canceled") {
		return false
	}
	// Kegagalan membuat request (bukan mengirim) → masalah body/URL.
	if strings.Contains(msg, "buat request") {
		return false
	}

	markers := []string{
		"connection refused",
		"connection reset",
		"proxyconnect",
		"no route to host",
		"network is unreachable",
		"i/o timeout",
		"context deadline exceeded",
		"connectex",
		"unexpected eof",
		"dial tcp",
	}
	for _, m := range markers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// cooldown menandai akun agar dilewati sementara, lalu mencatatnya.
// Best-effort: kegagalan menulis DB tidak boleh menggagalkan request.
func (s *Server) cooldown(acct *db.Account, reason string, body []byte) {
	if err := db.SetCooldown(acct.ID, reason); err != nil {
		log.Printf("[autoclawpi] akun #%d gagal set cooldown: %v", acct.ID, err)
		return
	}
	acct.CooldownUntil = time.Now().UTC().Add(db.CooldownDuration(reason)).Format(time.RFC3339)
	acct.CooldownReason = reason
	log.Printf("[autoclawpi] akun #%d cooldown %s selama %s (upstream: %s)",
		acct.ID, reason, db.CooldownDuration(reason), truncateResp(body, 120))
}

// classifyUpstreamError memetakan respons error upstream ke kategori cooldown.
// Kosong = tidak perlu cooldown (mis. error yang mungkin hilang sendiri).
//
// Pola diambil dari klien AutoClaw resmi (app.asar, NO_POINTS_PATTERNS dkk)
// supaya klasifikasinya konsisten dengan yang dipakai aplikasi desktop:
//
//	402 / "payment required" / insufficient balance|quota|credit|points /
//	账户额度不足|额度不足|余额不足|积分不足|积分已用完|积分耗尽  -> kuota
//	410004 "账号已被封禁"                                        -> banned
//	429 / 810002 (high demand)                                   -> rate_limit
func classifyUpstreamError(status int, body []byte) string {
	b := strings.ToLower(string(body))

	// Akun diblokir upstream — dicek lebih dulu karena statusnya 403,
	// sama seperti WAF block yang TIDAK boleh di-cooldown.
	if upstreamErrCode(body) == 410004 || strings.Contains(b, "已被封禁") ||
		strings.Contains(b, "account banned") || strings.Contains(b, "diblokir") {
		return db.CooldownBanned
	}

	// Kuota / saldo habis (402 dan variannya).
	if status == 402 ||
		strings.Contains(b, "payment required") ||
		strings.Contains(b, "insufficient") ||
		strings.Contains(b, "余额不足") || strings.Contains(b, "额度不足") ||
		strings.Contains(b, "积分不足") || strings.Contains(b, "积分已用完") ||
		strings.Contains(b, "积分耗尽") || strings.Contains(b, "积分已使用完毕") {
		return db.CooldownQuota
	}

	// Rate limit / antrean sementara.
	if status == 429 || upstreamErrCode(body) == 810002 ||
		strings.Contains(b, "too many requests") || strings.Contains(b, "high demand") {
		return db.CooldownRateLimit
	}

	// WAF block ({"message":"forbidden"}) dan 5xx: JANGAN cooldown —
	// yang pertama masalah header/global, yang kedua bisa sembuh sendiri.
	return ""
}

// upstreamErrCode mengekstrak field "code" dari body error upstream.
// Bentuknya bisa dua rupa:
//
// Jadi cek top-level dulu, lalu coba unwrap error.message sebagai JSON.
func upstreamErrCode(body []byte) int {
	body = stripWAFPrefixes(body)
	var direct struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(body, &direct); err == nil && direct.Code != 0 {
		return direct.Code
	}
	var wrapped struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil && wrapped.Error.Message != "" {
		var inner struct {
			Code int `json:"code"`
		}
		if err := json.Unmarshal([]byte(wrapped.Error.Message), &inner); err == nil {
			return inner.Code
		}
	}
	return 0
}

// stripWAFPrefixes membuang objek WAF `{"message":"forbidden"}` yang menempel
// di AWAL body. Hanya prefix yang dibuang (bukan pencarian global) supaya
// content sah yang kebetulan memuat string tersebut tidak ikut terpotong.
// Loop karena gateway bisa menempelkan beberapa prefix berturut-turut.
func stripWAFPrefixes(body []byte) []byte {
	const marker = `"message":"forbidden"`
	for {
		trimmed := bytes.TrimLeft(body, " \t\r\n")
		if !bytes.HasPrefix(trimmed, []byte("{")) {
			break
		}
		// Cari akhir objek pertama: marker harus berada di dalam objek itu,
		// dan objek ditutup sebelum awal JSON payload sebenarnya.
		end := bytes.IndexByte(trimmed, '}')
		if end < 0 {
			break
		}
		head := trimmed[:end+1]
		if !bytes.Contains(head, []byte(marker)) {
			break // objek pertama bukan WAF -> ini payload asli
		}
		body = trimmed[end+1:]
	}
	return bytes.TrimLeft(body, " \t\r\n")
}

// logUsage parse response body dan catat ke database.
func logUsage(acctID int64, model string, body []byte) {
	body = stripWAFPrefixes(body)
	var data struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return
	}
	status := "success"
	errMsg := ""
	if data.Error != nil {
		status = "error"
		errMsg = data.Error.Message
	}
	pts := 0
	cts := 0
	tts := 0
	if data.Usage != nil {
		pts = data.Usage.PromptTokens
		cts = data.Usage.CompletionTokens
		tts = data.Usage.TotalTokens
	}
	cost := float64(tts) * 0.001 / 1000.0
	_, _ = db.AddLog(&db.LogEntry{
		AccountID:        acctID,
		Model:            model,
		PromptTokens:     pts,
		CompletionTokens: cts,
		TotalTokens:      tts,
		Cost:             cost,
		Status:           status,
		Error:            errMsg,
	})
}

// handleModels menangani /v1/models.
func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	// Model yang sudah diverifikasi bisa inference (2026-09-05).
	// CATATAN (2026-09-15): "glm-5-turbo" dihapus — diverifikasi 400 非法模型
	// di semua varian route id (zai_/zaicoding_glm-5-turbo, glm-4.6, glm-4.5, ...),
	// jadi model ini tidak lagi ada di upstream. Jangan iklankan sebagai tersedia.
	models := []map[string]any{
		{"id": "auto", "object": "model", "created": 1, "owned_by": "autoclaw"},
		{"id": "auto-fast", "object": "model", "created": 1, "owned_by": "autoclaw"},
		{"id": "glm-5.3", "object": "model", "created": 1, "owned_by": "autoclaw"},
		{"id": "glm-5.3-flash", "object": "model", "created": 1, "owned_by": "autoclaw"},
		{"id": "deepseek-v4-pro", "object": "model", "created": 1, "owned_by": "autoclaw"},
		{"id": "deepseek-v4-flash", "object": "model", "created": 1, "owned_by": "autoclaw"},
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": models})
}

// handleHealth menangani /healthz.
//
// Melaporkan tiga angka supaya kondisi nyata terlihat (dulu hanya "active"
// dari flag manual, sehingga akun yang sedang cooldown tetap terhitung sehat):
//
//	accounts   = total akun di DB
//	active     = flag manual user (Active)
//	usable     = siap dipakai sekarang (active && ada token && tidak cooldown)
//	cooling    = sedang cooldown (auto-disable sementara)
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	accounts, _ := db.ListAccounts()
	now := time.Now()
	activeCount, usableCount, coolingCount := 0, 0, 0
	for _, a := range accounts {
		if a.Active {
			activeCount++
		}
		if a.IsCoolingDown(now) {
			coolingCount++
		}
		if a.Usable(now) {
			usableCount++
		}
	}
	writeJSON(w, 200, map[string]any{
		"ok":       true,
		"status":   "healthy",
		"accounts": len(accounts),
		"active":   activeCount,
		"usable":   usableCount,
		"cooling":  coolingCount,
		"time":     time.Now().Format(time.RFC3339),
	})
}

// refreshToken mencoba memperbarui token akun.
func (s *Server) refreshToken(acct *db.Account) (bool, error) {
	if acct.RefreshToken == "" {
		return false, fmt.Errorf("no refresh token")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := s.cl.RefreshVia(ctx, acct.RefreshToken, acct.Proxy)
	if err != nil {
		return false, err
	}
	if out.Code != 0 || out.Data == nil || out.Data.AccessToken == "" {
		msg := "refresh gagal"
		if out != nil {
			msg = out.Msg
		}
		return false, fmt.Errorf("%s", msg)
	}
	newRefresh := acct.RefreshToken
	if out.Data.RefreshToken != "" {
		newRefresh = out.Data.RefreshToken
	}
	acct.AccessToken = out.Data.AccessToken
	acct.RefreshToken = newRefresh
	_ = db.UpdateAccount(acct)
	log.Printf("[autoclawpi] akun #%d token diperbarui", acct.ID)
	return true, nil
}

// ── helpers ─────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"message": message,
			"type":    code,
		},
	})
}

func replaceModel(raw []byte, model string) ([]byte, error) {
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	data["model"] = model
	return json.Marshal(data)
}

func truncateResp(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}

// truncateRunes memotong string pada batas rune (bukan byte) supaya hasilnya
// selalu UTF-8 valid walau berisi karakter multibyte (CJK, emoji).
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}
