// Package client menyediakan HTTP client untuk API AutoClaw.
package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hirotomasato/autoclawpi/internal/sign"
)

// Client adalah HTTP client ke API AutoClaw.
type Client struct {
	HTTP          *http.Client
	InferenceBase string // https://autoglm-api.autoglm.ai/autoclaw-proxy/proxy/autoclaw
	UserAPIBase   string // https://autoglm-api.autoglm.ai
	Version       string

	// proxyMu melindungi transportCache.
	proxyMu sync.Mutex
	// transportCache memetakan proxyURL -> *http.Transport.
	// Transport di-cache supaya koneksi bisa dipakai ulang (keep-alive) dan
	// tidak membuat Transport baru tiap request (itu membocorkan koneksi).
	transportCache map[string]*http.Transport
}

// New membuat client dengan default yang masuk akal.
func New(inferenceBase, userAPIBase string) *Client {
	return &Client{
		HTTP:           &http.Client{Timeout: 0},
		InferenceBase:  inferenceBase,
		UserAPIBase:    userAPIBase,
		Version:        "1.17.9",
		transportCache: make(map[string]*http.Transport),
	}
}

// HTTPFor mengembalikan http.Client untuk akun dengan proxy tertentu.
//
// proxyURL kosong → pakai c.HTTP (perilaku lama, tanpa proxy).
// proxyURL terisi → pakai Transport khusus yang di-cache per proxyURL,
// sehingga tiap akun keluar lewat IP-nya sendiri tanpa membuat koneksi baru
// setiap request.
func (c *Client) HTTPFor(proxyURL string) *http.Client {
	if proxyURL == "" {
		return c.HTTP
	}
	c.proxyMu.Lock()
	defer c.proxyMu.Unlock()
	if c.transportCache == nil {
		c.transportCache = make(map[string]*http.Transport)
	}
	if tr, ok := c.transportCache[proxyURL]; ok {
		return &http.Client{Transport: tr, Timeout: 0}
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		// URL rusak → jangan diam-diam direct (itu membocorkan IP asli);
		// pakai transport yang selalu gagal supaya terlihat di log.
		tr := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) {
			return nil, fmt.Errorf("proxy URL tidak valid: %s", proxyURL)
		}}
		c.transportCache[proxyURL] = tr
		return &http.Client{Transport: tr, Timeout: 0}
	}
	// Clone DefaultTransport agar tetap dapat tuning default (timeout, http2,
	// keep-alive) lalu ganti hanya bagian Proxy.
	base, _ := http.DefaultTransport.(*http.Transport)
	tr := base.Clone()
	tr.Proxy = http.ProxyURL(u)
	c.transportCache[proxyURL] = tr
	return &http.Client{Transport: tr, Timeout: 0}
}

// DoFor melakukan request memakai client sesuai proxy akun.
func (c *Client) DoFor(req *http.Request, proxyURL string) (*http.Response, error) {
	req.Header.Set("User-Agent", "AutoClaw/"+c.Version)
	return c.HTTPFor(proxyURL).Do(req)
}

// deviceID mengembalikan ID perangkat persisten.
var deviceIDCache string

func deviceID() string {
	if deviceIDCache != "" {
		return deviceIDCache
	}
	h, _ := os.Hostname()
	if h == "" {
		h = "unknown"
	}
	b := make([]byte, 4)
	rand.Read(b)
	deviceIDCache = fmt.Sprintf("%s-%s", h, hex.EncodeToString(b))
	return deviceIDCache
}

// Do melakukan request dengan User-Agent aplikasi.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", "AutoClaw/"+c.Version)
	return c.HTTP.Do(req)
}

// LoginResponse adalah payload balikan oauth login.
type LoginResponse struct {
	Code  int    `json:"code"`
	Msg   string `json:"msg"`
	Trace string `json:"trace,omitempty"`
	Data  *struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		UserID       any    `json:"user_id,omitempty"`
		UserName     string `json:"user_name,omitempty"`
	} `json:"data,omitempty"`
}

// OAuthURL meminta URL OAuth. Return: url, state, error.
func (c *Client) OAuthURL(ctx context.Context, vendor, navigateURI, sourceID string, captcha map[string]any) (string, string, error) {
	ts := time.Now().Unix()
	body := map[string]any{
		"source_id":    sourceID,
		"navigate_uri": navigateURI,
		"device_id":    deviceID(),
	}
	if captcha != nil {
		for k, v := range captcha {
			body[k] = v
		}
	}
	if vendor == "google" {
		body["client_type"] = "pc"
	}
	var out struct {
		Code  int             `json:"code"`
		Msg   string          `json:"msg"`
		Trace string          `json:"trace"`
		Data  json.RawMessage `json:"data"`
	}
	if err := c.userapiPostWithHeaders(ctx, "/userapi/overseasv1/"+vendor+"-oauth-url", body, &out, sign.HeadersAt(ts)); err != nil {
		return "", "", err
	}
	if out.Code != 0 || out.Data == nil {
		return "", "", fmt.Errorf("oauth-url code=%d msg=%s trace=%s data=%s", out.Code, out.Msg, out.Trace, string(out.Data))
	}
	var urlData struct {
		OAuthURL string `json:"oauth_url"`
		State    string `json:"state"`
	}
	if err := json.Unmarshal(out.Data, &urlData); err != nil || urlData.OAuthURL == "" {
		return "", "", fmt.Errorf("oauth-url code=%d msg=%s trace=%s data=%s (url parse: %v)", out.Code, out.Msg, out.Trace, string(out.Data), err)
	}
	return urlData.OAuthURL, urlData.State, nil
}

// CaptchaConfig mengambil konfigurasi captcha OAuth.
func (c *Client) CaptchaConfig(ctx context.Context) (*CaptchaConfigResponse, error) {
	var out CaptchaConfigResponse
	if err := c.userapiPost(ctx, "/userapi/overseasv1/oauth-captcha-config", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CaptchaConfigResponse adalah balikan oauth-captcha-config.
type CaptchaConfigResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data *struct {
		Enabled  bool   `json:"enabled"`
		Region   string `json:"region"`
		Prefix   string `json:"prefix"`
		SceneID  string `json:"scene_id"`
		Supplier string `json:"captcha_supplier"`
	} `json:"data"`
}

// OAuthURLVerbose meminta URL OAuth dan melaporkan apakah captcha wajib.
func (c *Client) OAuthURLVerbose(ctx context.Context, vendor, navigateURI, sourceID string) (string, bool, error) {
	url, _, err := c.OAuthURL(ctx, vendor, navigateURI, sourceID, nil)
	if err == nil {
		return url, false, nil
	}
	return "", true, nil
}

// Login menukar kode OAuth jadi access/refresh token.
func (c *Client) Login(ctx context.Context, vendor, code, state, navigateURI string) (*LoginResponse, error) {
	body := map[string]any{
		"code":         code,
		"state":        state,
		"navigate_uri": navigateURI,
		"device_id":    deviceID(),
		"source_id":    "autoclaw",
	}
	var out LoginResponse
	if err := c.userapiPost(ctx, "/userapi/overseasv1/"+vendor+"-oauth-login", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Refresh memperbarui access token pakai refresh token.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*LoginResponse, error) {
	return c.RefreshVia(ctx, refreshToken, "")
}

// RefreshVia sama seperti Refresh tapi lewat proxy akun, supaya permintaan
// refresh juga keluar dari IP akun tersebut (konsisten dengan inference dan
// tidak membocorkan IP asli).
func (c *Client) RefreshVia(ctx context.Context, refreshToken, proxyURL string) (*LoginResponse, error) {
	body := map[string]any{
		"refresh_token": refreshToken,
		"device_id":     deviceID(),
		"source_id":     "autoclaw",
	}
	var out LoginResponse
	if err := c.userapiPostVia(ctx, "/userapi/v1/agent-refresh", body, &out, proxyURL); err != nil {
		return nil, err
	}
	return &out, nil
}

// agentdrHeader menyiapkan header untuk endpoint /agentdr/* dan userapi:
// authorization lowercase tanpa prefix "Bearer", plus signature X-Auth-*.
func agentdrHeader(token string) map[string]string {
	h := sign.HeadersAt(time.Now().Unix())
	h["X-Lang"] = "en"
	h["X-Client-Type"] = "pc"
	h["authorization"] = token // lowercase, tanpa Bearer
	delete(h, "Accept")
	return h
}

// ClaimTask mengklaim task check-in (daily_signin, dll).
// Header pakai X-Authorization (uppercase) + signature inference.
func (c *Client) ClaimTask(ctx context.Context, token, taskID string) (int, bool, error) {
	return c.claimTask(ctx, token, taskID, "")
}

// ClaimTaskVia sama seperti ClaimTask tapi lewat proxy akun, supaya tiap akun
// keluar dari IP-nya sendiri (bukan IP default yang dipakai bersama).
func (c *Client) ClaimTaskVia(ctx context.Context, token, taskID, proxyURL string) (int, bool, error) {
	return c.claimTask(ctx, token, taskID, proxyURL)
}

func (c *Client) claimTask(ctx context.Context, token, taskID, proxyURL string) (int, bool, error) {
	hdrs := c.InferenceHeader(token, "")
	// Tambah header yang diperlukan userapi
	hdrs["X-Lang"] = "en"
	hdrs["X-Client-Type"] = "pc"
	hdrs["authorization"] = token   // lowercase untuk userapi
	delete(hdrs, "X-Authorization") // inference header gak dipake

	body := fmt.Sprintf(`{"task_id":"%s"}`, taskID)
	req, err := http.NewRequestWithContext(ctx, "POST",
		c.UserAPIBase+"/autoclaw-proxy/proxy/autoclaw-task-complete",
		strings.NewReader(body))
	if err != nil {
		return 0, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}

	resp, err := c.DoFor(req, proxyURL)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var result struct {
		Data *struct {
			Success         bool `json:"success"`
			AlreadyComplete bool `json:"already_completed"`
			RewardPoints    int  `json:"reward_points"`
		} `json:"data"`
	}
	json.Unmarshal(b, &result)

	if result.Data == nil {
		return 0, false, fmt.Errorf("response: %s", string(b))
	}
	if result.Data.AlreadyComplete {
		return 0, true, nil // already done
	}
	if !result.Data.Success {
		return 0, false, fmt.Errorf("server: success=false")
	}
	return result.Data.RewardPoints, false, nil
}

// InspirationItem adalah satu entri di inspiration center.
type InspirationItem struct {
	InspirationID string `json:"inspiration_id"`
	Title         string `json:"title"`
	RewardPoints  int    `json:"reward_points"`
}

// InspirationCenter mengambil daftar inspirasi. Dipakai untuk task
// daily_inspiration_center: ambil inspiration_id dulu, baru klaim reward-nya.
func (c *Client) InspirationCenter(ctx context.Context, token, proxyURL string) ([]InspirationItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.UserAPIBase+"/agentdr/v1/assistant/inspiration-center", strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	for k, v := range agentdrHeader(token) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.DoFor(req, proxyURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			List []InspirationItem `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("payload bukan JSON: %s", truncate(b, 200))
	}
	if out.Code != 0 {
		return nil, fmt.Errorf("code %d: %s", out.Code, out.Msg)
	}
	return out.Data.List, nil
}

// ClaimInspirationReward mengklaim reward daily_inspiration_center.
//
// PENTING: endpoint & payload beda dari ClaimTask. Yang benar adalah
// /agentdr/v1/assistant/inspiration-task-complete dengan
// {"inspiration_id": "<uuid>"} — bukan /autoclaw-proxy/.../autoclaw-task-complete
// dengan task_id (itu bikin reward 0 dan status task jadi "completed" palsu).
//
// Return already=true kalau server bilang 560118 (sudah pernah diklaim).
func (c *Client) ClaimInspirationReward(ctx context.Context, token, inspirationID, proxyURL string) (bool, error) {
	body, _ := json.Marshal(map[string]string{"inspiration_id": inspirationID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.UserAPIBase+"/agentdr/v1/assistant/inspiration-task-complete", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	for k, v := range agentdrHeader(token) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.DoFor(req, proxyURL)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return false, fmt.Errorf("payload bukan JSON: %s", truncate(b, 200))
	}
	switch out.Code {
	case 0:
		return false, nil
	case 560118:
		return true, nil // sudah diklaim
	case 560117:
		return false, fmt.Errorf("retryable (code 560117): %s", out.Msg)
	}
	return false, fmt.Errorf("code %d: %s", out.Code, out.Msg)
}

// FetchBalance mengambil saldo wallet akun (via proxy kalau diisi).
func (c *Client) FetchBalance(ctx context.Context, token, proxyURL string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.UserAPIBase+"/agent-assetmgr/api/v1/wallet-instances?biz_app_id=autoclaw", nil)
	if err != nil {
		return 0, err
	}
	for k, v := range agentdrHeader(token) {
		req.Header.Set(k, v)
	}
	resp, err := c.DoFor(req, proxyURL)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var r struct {
		Data *struct {
			TotalBalance int `json:"total_balance"`
		} `json:"data"`
	}
	json.Unmarshal(b, &r)
	if r.Data == nil {
		return 0, fmt.Errorf("response: %s", truncate(b, 200))
	}
	return r.Data.TotalBalance, nil
}

// ClaimNewbieToken mengklaim token newbie guide (reward 100M token untuk akun baru).
// Endpoint ini pakai header X-Authorization (uppercase, mirip inference proxy),
// BUKAN commonHeaders userapi — tanpa signature X-Auth-*.
// Body kosong. Return token guide (base64 user_id|timestamp|signature).
func (c *Client) ClaimNewbieToken(ctx context.Context, accessToken string) (string, error) {
	tok := accessToken
	if !strings.HasPrefix(tok, "Bearer ") {
		tok = "Bearer " + tok
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.UserAPIBase+"/autoclaw-proxy/proxy/autoclaw-newbie-guide/token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Authorization", tok)
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(b, 300))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", fmt.Errorf("payload bukan JSON: %s", truncate(b, 200))
	}
	if out.Token == "" {
		return "", fmt.Errorf("token kosong: %s", truncate(b, 300))
	}
	return out.Token, nil
}

// ClaimPromotionReward mengklaim reward promosi (modal_id + reward_type).
// Header X-Authorization sama kayak ClaimNewbieToken.
func (c *Client) ClaimPromotionReward(ctx context.Context, accessToken, modalID, rewardType string) (json.RawMessage, error) {
	tok := accessToken
	if !strings.HasPrefix(tok, "Bearer ") {
		tok = "Bearer " + tok
	}
	body, _ := json.Marshal(map[string]string{
		"modal_id":    modalID,
		"reward_type": rewardType,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.UserAPIBase+"/autoclaw-proxy/proxy/autoclaw-promotion-reward", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Authorization", tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(b, 300))
	}
	return json.RawMessage(b), nil
}

func (c *Client) userapiPost(ctx context.Context, path string, body any, out any) error {
	return c.userapiPostWithHeaders(ctx, path, body, out, sign.Headers())
}

// userapiPostVia sama seperti userapiPost tapi lewat proxy tertentu.
func (c *Client) userapiPostVia(ctx context.Context, path string, body any, out any, proxyURL string) error {
	return c.userapiPostWithHeadersVia(ctx, path, body, out, sign.Headers(), proxyURL)
}

func (c *Client) userapiPostWithHeaders(ctx context.Context, path string, body any, out any, hdrs map[string]string) error {
	return c.userapiPostWithHeadersVia(ctx, path, body, out, hdrs, "")
}

func (c *Client) userapiPostWithHeadersVia(ctx context.Context, path string, body any, out any, hdrs map[string]string, proxyURL string) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.UserAPIBase+path, &buf)
	if err != nil {
		return err
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	// Add common headers (authorization with token, X-Lang, etc.)
	req.Header.Set("X-Lang", "en")
	req.Header.Set("X-Client-Type", "pc")
	// Note: Token is NOT added here - it's only used for inference and agentdr endpoints
	return c.doJSONVia(req, out, proxyURL)
}

func (c *Client) doJSON(req *http.Request, out any) error {
	return c.doJSONVia(req, out, "")
}

func (c *Client) doJSONVia(req *http.Request, out any, proxyURL string) error {
	resp, err := c.DoFor(req, proxyURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("HTTP %d: payload bukan JSON: %s", resp.StatusCode, truncate(b, 200))
		}
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(b, 300))
	}
	return nil
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}

// InferenceHeader membangun header untuk proxy inference.
//
// CATATAN (2026-09-15): header "X-Tm" WAJIB berisi platform desktop
// (windows/pc/darwin/mac/android/ios). Nilai "linux" memicu WAF hard block
// 403 {"message":"forbidden"} di gateway inference. Diverifikasi lewat
// matriks header: X-Tm=linux -> 403 (3/3), X-Tm=windows/win/pc/darwin/
// android/ios/"" -> 200. Hanya endpoint inference yang terpengaruh;
// userapi (sign.go) tetap pakai "linux" dan berjalan normal.
// "X-Harness-Type" juga wajib ada — tanpa itu gateway balas 400 invalid request.
func (c *Client) InferenceHeader(accessToken, routeModelID string) map[string]string {
	tok := accessToken
	if !strings.HasPrefix(tok, "Bearer ") {
		tok = "Bearer " + tok
	}
	return map[string]string{
		"X-Authorization": tok,
		"X-Request-Id":    sign.UUID(),
		"X-Request-Model": routeModelID,
		"X-Product":       "autoclaw",
		"X-Harness-Type":  "zcode",
		"X-Tm":            "windows",
		"X-Version":       c.Version,
		"X-Lang":          "id",
		"x_trace_id":      sign.UUID(),
	}
}

// RouteID memetakan nama model OpenAI-style ke route id AutoClaw.
func RouteID(model string) string {
	if containsUnderscore(model) {
		return model // sudah route id
	}
	// DeepSeek model pakai prefix tdpsk_ + versi date.
	switch model {
	case "deepseek-v4-pro":
		return "tdpsk_deepseek-v4-pro-202606"
	case "deepseek-v4-flash":
		return "tdpsk_deepseek-v4-flash-202605"
	}
	if isVersioned(model) {
		return "zaicoding_" + model
	}
	return "zai_" + model
}

// BodyModel membalik RouteID: "zaicoding_glm-5.3" -> "glm-5.3".
func BodyModel(routeID string) string {
	for i := 0; i < len(routeID); i++ {
		if routeID[i] == '_' {
			return routeID[i+1:]
		}
	}
	return routeID
}

func containsUnderscore(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '_' {
			return true
		}
	}
	return false
}

// isVersioned true untuk glm-5.3 / glm-5.2 (versi coding plan).
func isVersioned(model string) bool {
	return model == "glm-5.3" || model == "glm-5.2"
}
