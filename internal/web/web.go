package web

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hirotomasato/autoclawpi/internal/client"
	"github.com/hirotomasato/autoclawpi/internal/db"
	"github.com/hirotomasato/autoclawpi/internal/sign"
)

//go:embed templates/*.html
var templateFS embed.FS

// Server adalah web panel server.
type Server struct {
	mux      *http.ServeMux
	tmpl     *template.Template
	password string
	apiKey   string
	strategy string
	cl       *client.Client
}

// Option untuk konfigurasi web panel.
type Option func(*Server)

// WithPassword mengatur password panel.
func WithPassword(pwd string) Option {
	return func(s *Server) { s.password = pwd }
}

// WithAPIKey menyimpan API key untuk fetch models internal.
func WithAPIKey(key string) Option {
	return func(s *Server) { s.apiKey = key }
}

// New membuat web panel server baru.
func New(cl *client.Client, opts ...Option) *Server {
	s := &Server{
		mux:      http.NewServeMux(),
		strategy: "round-robin",
		cl:       cl,
	}
	for _, o := range opts {
		o(s)
	}

	tmpl := template.New("").Funcs(template.FuncMap{
		"pageTitle": pageTitle,
	})
	tmpl = template.Must(tmpl.ParseFS(templateFS, "templates/*.html"))
	s.tmpl = tmpl

	// Routes
	s.mux.HandleFunc("/", s.authMiddleware(s.handleDashboard))
	s.mux.HandleFunc("/accounts", s.authMiddleware(s.handleAccounts))
	s.mux.HandleFunc("/accounts/", s.authMiddleware(s.handleAccounts))
	s.mux.HandleFunc("/accounts/login", s.authMiddleware(s.handleAccountsLogin))
	s.mux.HandleFunc("/accounts/login/start", s.authMiddleware(s.handleAccountsLoginStart))
	s.mux.HandleFunc("/accounts/login/captcha-result", s.authMiddleware(s.handleAccountsLoginCaptcha))
	s.mux.HandleFunc("/accounts/import", s.authMiddleware(s.handleAccountsImport))
	s.mux.HandleFunc("/accounts/claim", s.authMiddleware(s.handleClaim100M))
	s.mux.HandleFunc("/checkin", s.authMiddleware(s.handleCheckin))
	s.mux.HandleFunc("/checkin/run", s.authMiddleware(s.handleCheckinRun))
	s.mux.HandleFunc("/settings", s.authMiddleware(s.handleSettings))
	s.mux.HandleFunc("/settings/password", s.authMiddleware(s.handleSettingsPassword))
	s.mux.HandleFunc("/settings/strategy", s.authMiddleware(s.handleSettingsStrategy))
	s.mux.HandleFunc("/settings/apikey", s.authMiddleware(s.handleSettingsAPIKey))
	s.mux.HandleFunc("/settings/apikey/delete", s.authMiddleware(s.handleSettingsAPIKeyDelete))
	s.mux.HandleFunc("/docs", s.authMiddleware(s.handleDocs))
	s.mux.HandleFunc("/health", s.authMiddleware(s.handleHealth))
	s.mux.HandleFunc("/health/run", s.authMiddleware(s.handleHealthRun))
	s.mux.HandleFunc("/logs", s.authMiddleware(s.handleLogs))
	s.mux.HandleFunc("/auth/callback-zai", s.handleOAuthCallback)
	s.mux.HandleFunc("/auth/callback-google", s.handleOAuthCallback)
	s.mux.HandleFunc("/login", s.handleLogin)
	s.mux.HandleFunc("/logout", s.handleLogout)

	return s
}

func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.password != "" {
			cookie, err := r.Cookie("panel_auth")
			if err != nil || cookie.Value != s.password {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) renderTemplate(w http.ResponseWriter, page string, currentPage string, data any) {
	d := map[string]any{
		"Page":  currentPage,
		"Title": pageTitle(currentPage),
	}
	if data != nil {
		if m, ok := data.(map[string]any); ok {
			for k, v := range m {
				d[k] = v
			}
		}
	}
	tmpl := template.Must(template.Must(template.New("").Funcs(template.FuncMap{"pageTitle": pageTitle}).Parse(s.tmplStr("base.html"))).Parse(s.tmplStr(page)))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	err := tmpl.ExecuteTemplate(w, "base", d)
	if err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func (s *Server) tmplStr(name string) string {
	data, err := templateFS.ReadFile("templates/" + name)
	if err != nil {
		return ""
	}
	return string(data)
}

func pageTitle(page string) string {
	switch page {
	case "dashboard":
		return "Dashboard — autoclawpi"
	case "accounts":
		return "Accounts — autoclawpi"
	case "checkin":
		return "Check-In — autoclawpi"
	case "settings":
		return "Settings — autoclawpi"
	case "login":
		return "Login — autoclawpi"
	case "docs":
		return "API Docs — autoclawpi"
	case "health":
		return "Health — autoclawpi"
	case "logs":
		return "Logs — autoclawpi"
	default:
		return "autoclawpi"
	}
}

func renderStandalone(w http.ResponseWriter, tmplName string, data any) {
	content, err := templateFS.ReadFile("templates/" + tmplName)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	t, err := template.New(tmplName).Parse(string(content))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	t.Execute(w, data)
}

func (s *Server) renderString(name string, data any) (string, error) {
	var buf strings.Builder
	err := s.tmpl.ExecuteTemplate(io.Writer(&buf), name, data)
	return buf.String(), err
}

// ── Handlers ────────────────────────────────────────────────────────

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		pwd := r.FormValue("password")
		if pwd == s.password {
			http.SetCookie(w, &http.Cookie{
				Name: "panel_auth", Value: pwd,
				Path: "/", MaxAge: 86400 * 30,
				HttpOnly: true, SameSite: http.SameSiteLaxMode,
			})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		renderStandalone(w, "panel-login.html", map[string]any{"Error": "Wrong password"})
		return
	}
	renderStandalone(w, "panel-login.html", nil)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "panel_auth", Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	accounts, _ := db.ListAccounts()
	totalPts := 0
	activeCount := 0
	type accountView struct {
		db.Account
		LastCheckinDate string
	}
	var views []accountView
	for _, a := range accounts {
		if a.Active {
			activeCount++
		}
		// Auto-fetch balance from server for each account
		balance := fetchBalanceDashboard(a.AccessToken)
		if balance > 0 && balance != a.Points {
			_ = db.UpdatePoints(a.ID, balance)
			a.Points = balance
		}
		totalPts += a.Points
		logs, _ := db.ListCheckinLog(a.ID, 1)
		lastDate := ""
		if len(logs) > 0 {
			lastDate = logs[0].Date
		}
		views = append(views, accountView{Account: a, LastCheckinDate: lastDate})
	}
	models := fetchModels("http://"+r.Host+"/v1/models", s.apiKey)
	totalReq, totalTokens, totalCost, _ := db.LogStats()
	s.renderTemplate(w, "dashboard.html", "dashboard", map[string]any{
		"Accounts":       views,
		"TotalAccounts":  len(accounts),
		"ActiveAccounts": activeCount,
		"TotalPoints":    totalPts,
		"CheckedInToday": 0,
		"Models":         models,
		"TotalReq":       totalReq,
		"TotalTokens":    totalTokens,
		"TotalCost":      totalCost,
	})
}

// fetchBalanceDashboard ambil balance dari server. Mirip fetchBalance di checkin.go
func fetchBalanceDashboard(token string) int {
	ts := time.Now().Unix()
	req, err := http.NewRequest("GET", "https://autoglm-api.autoglm.ai/agent-assetmgr/api/v1/wallet-instances?biz_app_id=autoclaw", nil)
	if err != nil {
		return 0
	}
	req.Header.Set("authorization", token)
	req.Header.Set("X-Auth-Appid", "100003")
	req.Header.Set("X-Auth-TimeStamp", fmt.Sprintf("%d", ts))
	req.Header.Set("X-Auth-Sign", sign.Sign(ts))
	req.Header.Set("X-Product", "autoclaw")
	req.Header.Set("X-Version", "1.17.9")
	req.Header.Set("X-Tm", "linux")
	req.Header.Set("X-Lang", "en")
	req.Header.Set("X-Client-Type", "pc")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("X-Trace-Id", sign.UUID())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()

	var data struct {
		Data *struct {
			TotalBalance float64 `json:"total_balance"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return 0
	}
	if data.Data == nil {
		return 0
	}
	return int(data.Data.TotalBalance)
}

func fetchModels(url string, apikey string) []string {
	models := []string{}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return models
	}
	if apikey != "" {
		req.Header.Set("Authorization", "Bearer "+apikey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		var data struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		json.NewDecoder(resp.Body).Decode(&data)
		for _, m := range data.Data {
			models = append(models, m.ID)
		}
	}
	return models
}

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/accounts")
	if path != "" && path != "/" {
		parts := strings.Split(strings.Trim(path, "/"), "/")
		if len(parts) >= 2 {
			id, err := parseInt64(parts[0])
			if err == nil && id > 0 {
				switch parts[1] {
				case "delete":
					db.DeleteAccount(id)
					http.Redirect(w, r, "/accounts", http.StatusSeeOther)
					return
				case "refresh":
					refreshBalance(id)
					w.WriteHeader(204)
					return
				case "claim":
					s.handleClaim100MForAccount(w, r, id)
					return
				}
			}
		}
		if len(parts) >= 1 {
			id, err := parseInt64(parts[0])
			if err == nil && id > 0 {
				s.handleAccountDetail(w, r, id)
				return
			}
		}
		http.NotFound(w, r)
		return
	}
	accounts, _ := db.ListAccounts()
	s.renderTemplate(w, "accounts.html", "accounts", map[string]any{"Accounts": accounts})
}

func parseInt64(s string) (int64, error) {
	var id int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number")
		}
		id = id*10 + int64(c-'0')
	}
	return id, nil
}

func (s *Server) handleAccountDetail(w http.ResponseWriter, r *http.Request, id int64) {
	a, err := db.GetAccount(id)
	if err != nil || a == nil {
		http.NotFound(w, r)
		return
	}
	balance := refreshBalance(id)
	history, _ := db.ListCheckinLog(id, 20)

	// Decode JWT to get user info
	userEmail, userID := decodeJWT(a.AccessToken)

	// Update DB if we got user info from JWT
	if userEmail != "" && a.Name == "" {
		a.Name = userEmail
		a.UserName = userEmail
		a.UserID = userID
		_ = db.UpdateAccount(a)
	}

	s.renderTemplate(w, "account.html", "accounts", map[string]any{
		"Account":        a,
		"Balance":        balance,
		"CheckinHistory": history,
	})
}

// decodeJWT extracts user info from a JWT token (second segment, base64).
func decodeJWT(token string) (email, userID string) {
	// Strip "Bearer " prefix
	raw := strings.TrimPrefix(token, "Bearer ")
	parts := strings.Split(raw, ".")
	if len(parts) < 2 {
		return "", ""
	}
	// Add padding
	payload := parts[1]
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", ""
	}
	var data struct {
		UserID any    `json:"user_id"`
		JTI    string `json:"jti"`
	}
	if err := json.Unmarshal(decoded, &data); err != nil {
		return "", ""
	}
	uid := ""
	if data.UserID != nil {
		uid = fmt.Sprint(data.UserID)
	}
	return data.JTI, uid
}

func refreshBalance(id int64) int {
	a, err := db.GetAccount(id)
	if err != nil || a == nil {
		return 0
	}
	return a.Points
}

func (s *Server) handleAccountsLogin(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/accounts/login" {
		http.NotFound(w, r)
		return
	}
	s.renderTemplate(w, "login.html", "login", map[string]any{"Flow": "start"})
}

func (s *Server) handleAccountsLoginStart(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	capCfg, err := s.cl.CaptchaConfig(ctx)
	prefix := "sq51tr"
	sceneID := "18vhnjxl"
	if err == nil && capCfg != nil && capCfg.Data != nil {
		prefix = capCfg.Data.Prefix
		sceneID = capCfg.Data.SceneID
	}
	s.renderTemplate(w, "login.html", "login", map[string]any{
		"Flow":     "captcha",
		"Prefix":   prefix,
		"SceneID":  sceneID,
		"LoginURL": "/accounts/login/captcha-result",
	})
}

func (s *Server) handleAccountsLoginCaptcha(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VerifyParam string `json:"verifyParam"`
		Provider    string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "msg": "bad request"})
		return
	}
	if req.VerifyParam == "" {
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "msg": "missing verifyParam"})
		return
	}

	// Use a registered port for OAuth callback
	callbackPort := 0
	var listener net.Listener
	registeredPorts := []int{18432, 19654, 19723, 53699}
	for _, p := range registeredPorts {
		if p == 8787 {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", p))
		if err == nil {
			listener = ln
			callbackPort = p
			break
		}
	}
	if listener == nil {
		// Fallback
		var err error
		listener, err = net.Listen("tcp", "localhost:18432")
		if err != nil {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "msg": "no port available"})
			return
		}
		callbackPort = 18432
	}

	// Start temporary callback server on the listener
	callbackMux := http.NewServeMux()
	webPanelURL := fmt.Sprintf("http://localhost:%d", 8787) // known port
	callbackMux.HandleFunc("/auth/callback-zai", func(w2 http.ResponseWriter, r2 *http.Request) {
		handleOAuthCallbackRedirect(w2, r2, s.cl, webPanelURL+"/accounts")
	})
	callbackServer := &http.Server{Handler: callbackMux}
	go func() {
		if err := callbackServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("[autoclawpi] callback server error: %v", err)
		}
	}()
	time.AfterFunc(5*time.Minute, func() { callbackServer.Close() })

	navigateURI := fmt.Sprintf("http://localhost:%d/auth/callback-zai", callbackPort)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	oauthURL, _, err := s.cl.OAuthURL(ctx, "zai", navigateURI, "autoclaw", map[string]any{
		"ali_captcha_verify_param": req.VerifyParam,
	})
	if err != nil {
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "msg": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": oauthURL})
}

func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" {
		http.Error(w, "missing code", 400)
		return
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	navigateURI := fmt.Sprintf("%s://%s%s", scheme, r.Host, r.URL.Path)

	vendor := "zai"
	if strings.Contains(r.URL.Path, "google") {
		vendor = "google"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := s.cl.Login(ctx, vendor, code, state, navigateURI)
	if err != nil {
		http.Error(w, "Login failed: "+err.Error(), 500)
		return
	}
	if out.Code != 0 || out.Data == nil || out.Data.AccessToken == "" {
		http.Error(w, fmt.Sprintf("Login failed: code=%d msg=%s", out.Code, out.Msg), 500)
		return
	}

	deviceID := "web-oauth-" + fmt.Sprintf("%x", time.Now().UnixNano())
	userID := ""
	if out.Data.UserID != nil {
		userID = fmt.Sprint(out.Data.UserID)
	}
	acctID, err := db.AddAccount(out.Data.UserName, out.Data.AccessToken, out.Data.RefreshToken, vendor, userID, out.Data.UserName, deviceID)
	if err != nil {
		http.Error(w, "Save failed: "+err.Error(), 500)
		return
	}

	// Auto-fetch balance (synchronous)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	points := fetchBalance(ctx2, out.Data.AccessToken, s.cl)
	cancel2()
	if points > 0 {
		_ = db.UpdatePoints(acctID, points)
	}

	// Redirect back to web panel
	http.Redirect(w, r, "/accounts", http.StatusSeeOther)
}

func (s *Server) handleAccountsImport(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		name := r.FormValue("name")
		accessToken := r.FormValue("access_token")
		refreshToken := r.FormValue("refresh_token")
		if accessToken == "" {
			s.renderTemplate(w, "import.html", "accounts", map[string]any{"Error": "Access token required", "Name": name})
			return
		}
		deviceID := "import-" + fmt.Sprintf("%x", time.Now().UnixNano())
		_, err := db.AddAccount(name, accessToken, refreshToken, "zai", "", "", deviceID)
		if err != nil {
			s.renderTemplate(w, "import.html", "accounts", map[string]any{"Error": err.Error(), "Name": name})
			return
		}
		http.Redirect(w, r, "/accounts", http.StatusSeeOther)
		return
	}
	s.renderTemplate(w, "import.html", "accounts", nil)
}

// handleClaim100MForAccount handles /accounts/{id}/claim (HTMX button).
func (s *Server) handleClaim100MForAccount(w http.ResponseWriter, r *http.Request, id int64) {
	a, err := db.GetAccount(id)
	if err != nil || a == nil || a.AccessToken == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<div style="padding:12px;border-radius:10px;background:rgba(220,38,38,0.12);color:#f87171;font-size:13px"><i class="fas fa-times-circle"></i> Account not found</div>`)
		return
	}
	s.doClaim100M(w, r, a)
}

// handleClaim100M mengklaim reward 100M token untuk akun (JSON body).
func (s *Server) handleClaim100M(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "msg": "POST only"})
		return
	}
	var req struct {
		AccountID int64 `json:"account_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "invalid body"})
		return
	}
	if req.AccountID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "account_id required"})
		return
	}
	a, err := db.GetAccount(req.AccountID)
	if err != nil || a == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "msg": "account not found"})
		return
	}
	if a.AccessToken == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "account has no token"})
		return
	}
	s.doClaim100M(w, r, a)
}

// doClaim100M mengklaim token newbie guide dan mengembalikan HTML.
func (s *Server) doClaim100M(w http.ResponseWriter, r *http.Request, a *db.Account) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	token, err := s.cl.ClaimNewbieToken(ctx, a.AccessToken)
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<div style="padding:12px;border-radius:10px;background:rgba(220,38,38,0.12);color:#f87171;font-size:13px"><i class="fas fa-times-circle"></i> Claim gagal: %s</div>`, template.HTMLEscapeString(err.Error()))
		return
	}

	// Refresh balance after claim
	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	points := fetchBalance(ctx2, a.AccessToken, s.cl)
	cancel2()
	if points > 0 {
		_ = db.UpdatePoints(a.ID, points)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<div style="padding:14px;border-radius:10px;background:rgba(5,150,105,0.12);border:1px solid rgba(52,211,153,0.25)">
  <div style="font-size:14px;font-weight:700;color:#34d399;margin-bottom:6px"><i class="fas fa-gift"></i> 100M Token Claimed!</div>
  <div style="font-size:12px;color:#a7f3d0;font-family:mono;word-break:break-all;margin-bottom:8px">%s</div>
  <div style="font-size:13px;color:#e2e8f0">Balance: <span style="color:#fbbf24;font-weight:700">%d pts</span></div>
</div>`, template.HTMLEscapeString(token), points)
}

func (s *Server) handleCheckin(w http.ResponseWriter, r *http.Request) {
	accounts, _ := db.ListAccounts()
	s.renderTemplate(w, "checkin.html", "checkin", map[string]any{"Accounts": accounts})
}

func (s *Server) handleCheckinRun(w http.ResponseWriter, r *http.Request) {
	accounts, _ := db.ListAccounts()
	type taskResult struct {
		Name    string `json:"name"`
		Points  int    `json:"points"`
		Success bool   `json:"success"`
		Status  string `json:"status"`
	}
	type accountResult struct {
		AccountName   string       `json:"account_name"`
		Status        string       `json:"status"`
		BalanceBefore int          `json:"balance_before"`
		BalanceAfter  int          `json:"balance_after"`
		Tasks         []taskResult `json:"tasks"`
	}
	var results []accountResult
	today := time.Now().UTC().Format("2006-01-02")
	tasks := []struct{ ID, Name string }{
		{"daily_signin", "Daily Check-In"},
	}

	for _, a := range accounts {
		if !a.Active {
			continue
		}
		ar := accountResult{
			AccountName:   ifEmpty(a.Name, fmt.Sprintf("Account #%d", a.ID)),
			BalanceBefore: a.Points,
			Status:        "success",
		}
		for _, t := range tasks {
			existing, _ := db.GetCheckinLog(a.ID, today, t.ID)
			if existing != nil {
				ar.Tasks = append(ar.Tasks, taskResult{Name: t.Name, Points: existing.Points, Success: true, Status: "already"})
				continue
			}
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			points, alreadyDone, err := s.cl.ClaimTaskVia(ctx, a.AccessToken, t.ID, a.Proxy)
			cancel()
			if err != nil {
				ar.Tasks = append(ar.Tasks, taskResult{Name: t.Name, Points: 0, Success: false, Status: "failed: " + err.Error()})
				db.AddCheckinLog(a.ID, today, t.ID, 0, "failed: "+err.Error(), a.DeviceID)
				continue
			}
			if alreadyDone {
				ar.Tasks = append(ar.Tasks, taskResult{Name: t.Name, Points: 0, Success: true, Status: "already"})
				db.AddCheckinLog(a.ID, today, t.ID, 0, "already", a.DeviceID)
				continue
			}
			ar.Tasks = append(ar.Tasks, taskResult{Name: t.Name, Points: points, Success: true, Status: "claimed"})
			db.AddCheckinLog(a.ID, today, t.ID, points, "success", a.DeviceID)
			db.UpdatePoints(a.ID, a.Points+points)
			ar.BalanceAfter += points
		}

		// Inspiration Hub: endpoint beda (dua langkah, butuh inspiration_id).
		if existing, _ := db.GetCheckinLog(a.ID, today, "daily_inspiration_center"); existing == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
			pts, alreadyDone, err := s.claimInspiration(ctx, a)
			cancel()
			switch {
			case err != nil:
				ar.Tasks = append(ar.Tasks, taskResult{Name: "Inspiration Hub", Success: false, Status: "failed: " + err.Error()})
				db.AddCheckinLog(a.ID, today, "daily_inspiration_center", 0, "failed: "+err.Error(), a.DeviceID)
			case alreadyDone:
				ar.Tasks = append(ar.Tasks, taskResult{Name: "Inspiration Hub", Success: true, Status: "already"})
				db.AddCheckinLog(a.ID, today, "daily_inspiration_center", 0, "already", a.DeviceID)
			default:
				ar.Tasks = append(ar.Tasks, taskResult{Name: "Inspiration Hub", Points: pts, Success: true, Status: "claimed"})
				db.AddCheckinLog(a.ID, today, "daily_inspiration_center", pts, "success", a.DeviceID)
				db.UpdatePoints(a.ID, a.Points+pts)
				ar.BalanceAfter += pts
			}
		}
		if ar.BalanceAfter == 0 {
			ar.BalanceAfter = ar.BalanceBefore
		}
		results = append(results, ar)
	}
	s.renderTemplate(w, "checkin.html", "checkin", map[string]any{
		"Results": results,
	})
}

// claimInspiration mengklaim daily_inspiration_center untuk satu akun.
// Alur dua langkah: ambil inspiration_id → klaim reward.
// Return (points, alreadyDone, error).
func (s *Server) claimInspiration(ctx context.Context, a db.Account) (int, bool, error) {
	items, err := s.cl.InspirationCenter(ctx, a.AccessToken, a.Proxy)
	if err != nil {
		return 0, false, err
	}
	if len(items) == 0 {
		return 0, false, fmt.Errorf("tidak ada item inspirasi")
	}
	already, err := s.cl.ClaimInspirationReward(ctx, a.AccessToken, items[0].InspirationID, a.Proxy)
	if err != nil {
		return 0, false, err
	}
	if already {
		return 0, true, nil
	}
	return items[0].RewardPoints, false, nil
}

func ifEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	apikey, _ := db.GetConfig("api_key")
	apiKeys := loadAPIKeys()
	accounts, _ := db.ListAccounts()
	// Fetch models with API key
	models := []string{}
	modelsReq, _ := http.NewRequest("GET", "http://"+r.Host+"/v1/models", nil)
	if s.apiKey != "" {
		modelsReq.Header.Set("Authorization", "Bearer "+s.apiKey)
	}
	resp, err := http.DefaultClient.Do(modelsReq)
	if err == nil {
		defer resp.Body.Close()
		var data struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		json.NewDecoder(resp.Body).Decode(&data)
		for _, m := range data.Data {
			models = append(models, m.ID)
		}
	}
	s.renderTemplate(w, "settings.html", "settings", map[string]any{
		"Strategy":      s.strategy,
		"DBPath":        "~/.autoclawpi/autoclawpi.db",
		"DBSize":        "OK",
		"TotalAccounts": len(accounts),
		"Models":        models,
		"APIKey":        apikey,
		"APIKeys":       apiKeys,
	})
}

type apiKeyEntry struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

func loadAPIKeys() []apiKeyEntry {
	data, _ := db.GetConfig("api_keys")
	if data == "" {
		return nil
	}
	var keys []apiKeyEntry
	json.Unmarshal([]byte(data), &keys)
	return keys
}

func saveAPIKeys(keys []apiKeyEntry) {
	b, _ := json.Marshal(keys)
	db.SetConfig("api_keys", string(b))
}

func (s *Server) handleSettingsPassword(w http.ResponseWriter, r *http.Request) {
	pwd := r.FormValue("password")
	if pwd == "" {
		w.Write([]byte(`<span style="color:#f87171">Password cannot be empty</span>`))
		return
	}
	s.password = pwd
	http.SetCookie(w, &http.Cookie{Name: "panel_auth", Value: pwd, Path: "/", MaxAge: 86400 * 30, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	w.Write([]byte(`<span style="color:#34d399">Password updated</span>`))
}

func (s *Server) handleSettingsStrategy(w http.ResponseWriter, r *http.Request) {
	strat := r.FormValue("strategy")
	if strat != "" {
		s.strategy = strat
	}
	w.Write([]byte(`<span style="color:#34d399">Strategy updated</span>`))
}

func (s *Server) handleSettingsAPIKey(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	key := r.FormValue("apikey")
	if name == "" || key == "" {
		w.Write([]byte(`<span style="color:#f87171">Name and key required</span>`))
		return
	}
	keys := loadAPIKeys()
	keys = append(keys, apiKeyEntry{Name: name, Key: key})
	saveAPIKeys(keys)
	w.Write([]byte(`<span style="color:#34d399">API key added</span>`))
}

func (s *Server) handleSettingsAPIKeyDelete(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	if name == "" {
		w.Write([]byte(`<span style="color:#f87171">Name required</span>`))
		return
	}
	keys := loadAPIKeys()
	filtered := keys[:0]
	for _, k := range keys {
		if k.Name != name {
			filtered = append(filtered, k)
		}
	}
	saveAPIKeys(filtered)
	w.Write([]byte(`<span style="color:#34d399">API key deleted</span>`))
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	logs, _ := db.ListLogs(100)
	totalReq, totalTokens, totalCost, _ := db.LogStats()
	s.renderTemplate(w, "logs.html", "logs", map[string]any{
		"Logs":        logs,
		"TotalReq":    totalReq,
		"TotalTokens": totalTokens,
		"TotalCost":   totalCost,
	})
}

func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	models := fetchModels("http://"+r.Host+"/v1/models", s.apiKey)
	s.renderTemplate(w, "docs.html", "docs", map[string]any{"Models": models})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	accounts, _ := db.ListAccounts()
	s.renderTemplate(w, "health.html", "health", map[string]any{"Accounts": accounts})
}

func (s *Server) handleHealthRun(w http.ResponseWriter, r *http.Request) {
	accounts, _ := db.ListAccounts()
	type result struct {
		ID        int64  `json:"id"`
		OK        bool   `json:"ok"`
		Refreshed bool   `json:"refreshed"`
		Error     string `json:"error,omitempty"`
	}
	var results []result
	for _, a := range accounts {
		if !a.Active {
			results = append(results, result{ID: a.ID, OK: false, Error: "inactive"})
			continue
		}
		// Try refresh directly
		rerr := s.refreshTokenAPI(&a)
		if rerr == nil {
			_ = db.UpdateAccount(&a)
			results = append(results, result{ID: a.ID, OK: true, Refreshed: true})
			continue
		}
		a.Active = false
		_ = db.UpdateAccount(&a)
		results = append(results, result{ID: a.ID, OK: false, Error: rerr.Error()})
	}
	writeJSON(w, 200, map[string]any{"results": results})
}

func (s *Server) refreshTokenAPI(acct *db.Account) error {
	if acct.RefreshToken == "" {
		return fmt.Errorf("no refresh token")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := s.cl.Refresh(ctx, acct.RefreshToken)
	if err != nil {
		return err
	}
	if out.Code != 0 || out.Data == nil || out.Data.AccessToken == "" {
		return fmt.Errorf("refresh code=%d msg=%s", out.Code, out.Msg)
	}
	acct.AccessToken = out.Data.AccessToken
	if out.Data.RefreshToken != "" {
		acct.RefreshToken = out.Data.RefreshToken
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// handleOAuthCallbackRedirect handles the OAuth callback on the temporary port
// and redirects back to the web panel.
func handleOAuthCallbackRedirect(w http.ResponseWriter, r *http.Request, cl *client.Client, redirectURL string) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" {
		http.Error(w, "missing code", 400)
		return
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	navigateURI := fmt.Sprintf("%s://%s%s", scheme, r.Host, r.URL.Path)

	vendor := "zai"
	if strings.Contains(r.URL.Path, "google") {
		vendor = "google"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := cl.Login(ctx, vendor, code, state, navigateURI)
	if err != nil {
		http.Error(w, "Login failed: "+err.Error(), 500)
		return
	}
	if out.Code != 0 || out.Data == nil || out.Data.AccessToken == "" {
		http.Error(w, fmt.Sprintf("Login failed: code=%d msg=%s", out.Code, out.Msg), 500)
		return
	}

	deviceID := "web-oauth-" + fmt.Sprintf("%x", time.Now().UnixNano())
	userID := ""
	if out.Data.UserID != nil {
		userID = fmt.Sprint(out.Data.UserID)
	}
	acctID, err := db.AddAccount(out.Data.UserName, out.Data.AccessToken, out.Data.RefreshToken, vendor, userID, out.Data.UserName, deviceID)
	if err != nil {
		http.Error(w, "Save failed: "+err.Error(), 500)
		return
	}

	// Auto-fetch balance (synchronous)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	points := fetchBalance(ctx2, out.Data.AccessToken, cl)
	cancel2()
	if points > 0 {
		_ = db.UpdatePoints(acctID, points)
	}

	http.Redirect(w, r, redirectURL, http.StatusSeeOther)
}

// fetchBalance mengambil saldo points dari server.
func fetchBalance(ctx context.Context, token string, cl *client.Client) int {
	ts := time.Now().Unix()
	hdrs := map[string]string{
		"authorization":    token,
		"X-Lang":           "en",
		"X-Product":        "autoclaw",
		"X-Version":        "1.17.9",
		"X-Tm":             "linux",
		"X-Client-Type":    "pc",
		"X-Auth-Appid":     "100003",
		"X-Auth-TimeStamp": fmt.Sprintf("%d", ts),
		"X-Auth-Sign":      sign.Sign(ts),
		"X-Trace-Id":       sign.UUID(),
	}
	req, err := http.NewRequestWithContext(ctx, "GET", cl.UserAPIBase+"/agent-assetmgr/api/v1/wallet-instances?biz_app_id=autoclaw", nil)
	if err != nil {
		return 0
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var data struct {
		Code int `json:"code"`
		Data *struct {
			TotalBalance float64 `json:"total_balance"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &data); err != nil {
		return 0
	}
	if data.Data == nil {
		return 0
	}
	return int(data.Data.TotalBalance)
}
