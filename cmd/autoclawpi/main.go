// Command autoclawpi adalah CLI untuk mengelola kredensial AutoClaw
// dan menyajikan API OpenAI-compatible lokal.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hirotomasato/autoclawpi/internal/client"
	"github.com/hirotomasato/autoclawpi/internal/config"
	"github.com/hirotomasato/autoclawpi/internal/db"
	"github.com/hirotomasato/autoclawpi/internal/server"
	"github.com/hirotomasato/autoclawpi/internal/store"
	"github.com/hirotomasato/autoclawpi/internal/web"
)

var version = "dev"

const usage = `autoclawpi — OpenAI-compatible proxy untuk AutoClaw (Z.ai)

Pemakaian:
  autoclawpi serve                 jalankan server OpenAI-compatible (default :8787)
  autoclawpi login                 login OAuth via browser (butuh captcha solve)
                                   default: TAMBAH akun baru ke pool (tidak menimpa)
                                   --no-browser: cetak URL saja, buka manual
                                   --name <nama>: nama akun baru
                                   --timeout <menit>: batas tunggu login (default 20)
                                   --proxy <url>: proxy khusus akun ini (kosong = ip-pool.json)
                                   --replace: TIMPA akun pertama (perilaku lama)
  autoclawpi import                import token manual (stdin: access [refresh])
                                   default: TAMBAH akun baru ke pool
                                   --name <nama>: nama akun baru
                                   --proxy <url>: proxy khusus akun ini
                                   --replace: TIMPA akun pertama
  autoclawpi refresh               perbarui access token via refresh token
  autoclawpi status                tampilkan status login (token disensor)
  autoclawpi logout                hapus kredensial tersimpan
  autoclawpi ippool                lihat pemasangan akun ↔ IP (list | check | assign)
  autoclawpi version               tampilkan versi
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	// Init DB
	cfgDir := os.Getenv("AUTOCLAWPI_DIR")
	if cfgDir == "" {
		home, _ := os.UserHomeDir()
		cfgDir = home + "/.autoclawpi"
	}
	if err := db.Init(cfgDir); err != nil {
		fmt.Fprintln(os.Stderr, "db error:", err)
		os.Exit(1)
	}
	defer db.Close()

	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "login":
		err = cmdLogin(os.Args[2:])
	case "import":
		err = cmdImport(os.Args[2:])
	case "refresh":
		err = cmdRefresh(os.Args[2:])
	case "status":
		err = cmdStatus()
	case "logout":
		err = store.Clear()
		fmt.Println("kredensial dihapus")
	case "account", "accounts":
		err = cmdAccount(os.Args[2:])
	case "ippool", "ip":
		err = cmdIPPool(os.Args[2:])
	case "checkin":
		err = cmdCheckin(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("autoclawpi", version)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func loadAll() (*config.Config, *client.Client) {
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	return cfg, client.New(cfg.InferenceBase, cfg.UserAPIBase)
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", 0, "port listen (override config)")
	host := fs.String("host", "", "host listen (override config)")
	apiKey := fs.String("api-key", "", "API key utk melindungi server ini")
	webPwd := fs.String("web-password", "", "password web panel (opsional)")
	fs.Parse(args)

	cfg, cl := loadAll()
	if *port != 0 {
		cfg.Port = *port
	}
	if *host != "" {
		cfg.Host = *host
	}
	if *apiKey != "" {
		cfg.APIKey = *apiKey
	}

	// Main handler: OpenAI API
	apiSrv := server.New(cl).WithAPIKey(cfg.APIKey)
	if cfg.RateLimitPerSec > 0 || cfg.RateLimitBurst > 0 {
		apiSrv.WithRateLimit(cfg.RateLimitPerSec, cfg.RateLimitBurst)
	}
	apiHandler := apiSrv.Handler()

	// Web panel handler
	var webOpts []web.Option
	if *webPwd != "" {
		webOpts = append(webOpts, web.WithPassword(*webPwd))
	}
	if cfg.APIKey != "" {
		webOpts = append(webOpts, web.WithAPIKey(cfg.APIKey))
	}
	webHandler := web.New(cl, webOpts...)

	// Merge: web panel di path /, API di /v1/ dan /healthz
	mux := http.NewServeMux()
	mux.Handle("/v1/", apiHandler)
	mux.Handle("/healthz", apiHandler)
	mux.Handle("/", webHandler.Handler())

	// Higiene cooldown: bersihkan status cooldown yang sudah lewat supaya DB
	// tidak menumpuk entri basi (auto-recover sendiri sudah ditangani
	// Account.Usable, ini murni kerapian + observability).
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			if n, err := db.ClearExpiredCooldowns(); err == nil && n > 0 {
				fmt.Printf("[autoclawpi] %d cooldown akun sudah lewat, dibersihkan\n", n)
			}
			<-t.C
		}
	}()

	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shCtx)
	}()

	fmt.Printf("autoclawpi listen di http://%s\n", addr)
	fmt.Printf("  API:  /v1/chat/completions, /v1/models\n")
	fmt.Printf("  Panel: /, /accounts, /checkin, /settings\n")
	if *webPwd != "" {
		fmt.Println("  Panel login: /login (password required)")
	}
	if cfg.APIKey != "" {
		fmt.Println("  API auth: Authorization: Bearer <api-key>")
	}

	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func finishLogin(ctx context.Context, cl *client.Client, vendor, code, state, navigateURI string, replace bool, acctName, proxyURL string) error {
	out, err := cl.Login(ctx, vendor, code, state, navigateURI)
	if err != nil {
		return err
	}
	if out.Code != 0 || out.Data == nil || out.Data.AccessToken == "" {
		return fmt.Errorf("login gagal code=%d msg=%s", out.Code, out.Msg)
	}
	userID := ""
	if out.Data.UserID != nil {
		userID = fmt.Sprint(out.Data.UserID)
	}
	c := &store.Creds{
		AccessToken:  out.Data.AccessToken,
		RefreshToken: out.Data.RefreshToken,
		Provider:     vendor,
		UserID:       userID,
		UserName:     out.Data.UserName,
		SavedAt:      time.Now().Format(time.RFC3339),
		Proxy:        proxyURL,
	}
	if replace {
		// Mode eksplisit: timpa akun pertama (perilaku lama, harus diminta).
		if err := store.Save(c); err != nil {
			return err
		}
		fmt.Println("login sukses — kredensial akun pertama DITIMPA (--replace).")
		return cmdStatus()
	}
	// Default: SELALU tambah akun baru (pool mode) — tidak menimpa apa pun.
	id, err := store.Add(acctName, c)
	if err != nil {
		return err
	}
	fmt.Printf("login sukses — akun baru #%d ditambahkan (akun lama tidak tersentuh).\n", id)

	// Reward newbie hanya bisa diklaim SEKALI seumur akun. Klaim di sini
	// (saat akun baru masuk) supaya checkin harian tidak perlu menyentuh
	// tugas yang sudah pasti "already" — menghemat request dan mengurangi
	// pola perilaku teratur ke upstream.
	claimNewbieRewards(id)

	return listAccountsShort()
}

// claimNewbieRewards mengklaim tugas newbie (sekali seumur akun) untuk akun
// yang baru ditambahkan. Best-effort: kegagalan TIDAK menggagalkan login —
// akun sudah tersimpan dan tetap bisa dipakai.
//
// Setiap klaim dicatat ke checkin_log dengan tanggal hari ini supaya
// konsisten dengan pencatatan checkin (dan bisa diaudit).
func claimNewbieRewards(accountID int64) {
	accounts, err := db.ListAccounts()
	if err != nil {
		return
	}
	var acct *db.Account
	for i := range accounts {
		if accounts[i].ID == accountID {
			acct = &accounts[i]
			break
		}
	}
	if acct == nil || acct.AccessToken == "" {
		return
	}

	_, cl := loadAll()
	today := time.Now().UTC().Format("2006-01-02")
	claimed, failed := 0, 0

	for _, t := range newbieTasks {
		// Sudah pernah diklaim (akun lama / login ulang) → lewati.
		if existing, _ := db.GetCheckinLog(acct.ID, today, t.ID); existing != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		points, already, cerr := cl.ClaimTaskVia(ctx, acct.AccessToken, t.ID, acct.Proxy)
		cancel()
		switch {
		case cerr != nil:
			failed++
			fmt.Printf("  newbie %s: GAGAL — %v\n", t.ID, cerr)
			db.AddCheckinLog(acct.ID, today, t.ID, 0, "failed:"+cerr.Error(), acct.DeviceID)
		case already:
			db.AddCheckinLog(acct.ID, today, t.ID, 0, "already", acct.DeviceID)
		default:
			claimed += points
			fmt.Printf("  newbie %s: ✅ %d pts\n", t.ID, points)
			db.AddCheckinLog(acct.ID, today, t.ID, points, "success", acct.DeviceID)
		}
	}

	if claimed > 0 {
		if bal, berr := fetchBalanceVia(cl, acct.AccessToken, acct.Proxy); berr == nil && bal > 0 {
			db.UpdatePoints(acct.ID, bal)
		}
		fmt.Printf("  bonus newbie: +%d pts\n", claimed)
	}
	_ = failed // best-effort; tidak menggagalkan login
}

// fetchBalanceVia membaca saldo lewat proxy akun (helper tipis untuk dipakai
// di jalur login, supaya tidak bergantung pada helper web).
func fetchBalanceVia(cl *client.Client, token, proxy string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return cl.FetchBalance(ctx, token, proxy)
}

// listAccountsShort menampilkan ringkas daftar akun setelah menambah akun.
func listAccountsShort() error {
	accounts, err := db.ListAccounts()
	if err != nil {
		return err
	}
	now := time.Now()
	fmt.Printf("\ntotal %d akun:\n", len(accounts))
	for _, a := range accounts {
		status := "siap"
		switch {
		case !a.Active:
			status = "manual-off"
		case a.IsCoolingDown(now):
			status = "cooldown:" + a.CooldownReason
		}
		fmt.Printf("  #%-3d %-18s %s\n", a.ID, truncate(a.Name, 16), status)
	}
	return nil
}

func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	access := fs.String("access", "", "access token")
	refresh := fs.String("refresh", "", "refresh token (opsional)")
	provider := fs.String("provider", "zai", "zai | google")
	newAcct := fs.Bool("replace", false, "TIMPA akun pertama (default: tambah akun baru)")
	name := fs.String("name", "", "nama akun baru")
	proxyURL := fs.String("proxy", "", "proxy khusus akun ini; kosong = ambil dari ip-pool.json")
	fs.Parse(args)

	a, r := *access, *refresh
	if a == "" {
		// baca dari stdin: baris1 access, baris2 refresh (opsional)
		rd := bufio.NewReader(os.Stdin)
		fmt.Print("access token: ")
		line, _ := rd.ReadString('\n')
		a = strings.TrimSpace(line)
		if a == "" {
			return fmt.Errorf("access token kosong")
		}
		fmt.Print("refresh token (opsional, enter utk lewati): ")
		line, _ = rd.ReadString('\n')
		r = strings.TrimSpace(line)
	}
	c := &store.Creds{
		AccessToken:  a,
		RefreshToken: r,
		Provider:     *provider,
		SavedAt:      time.Now().Format(time.RFC3339),
		Proxy:        *proxyURL,
	}
	if *newAcct {
		// --replace: timpa akun pertama (perilaku lama).
		if err := store.Save(c); err != nil {
			return err
		}
		fmt.Println("kredensial akun pertama DITIMPA (--replace).")
		return cmdStatus()
	}
	// Default: tambah akun baru ke pool.
	id, err := store.Add(*name, c)
	if err != nil {
		return err
	}
	fmt.Printf("akun baru #%d ditambahkan (akun lama tidak tersentuh).\n", id)
	return listAccountsShort()
}

func cmdRefresh(args []string) error {
	_, cl := loadAll()
	c, err := store.Load()
	if err != nil {
		return err
	}
	if c.RefreshToken == "" {
		return fmt.Errorf("tidak ada refresh token tersimpan")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := cl.Refresh(ctx, c.RefreshToken)
	if err != nil {
		return err
	}
	if out.Code != 0 || out.Data == nil || out.Data.AccessToken == "" {
		return fmt.Errorf("refresh gagal code=%d msg=%s", out.Code, out.Msg)
	}
	c.AccessToken = out.Data.AccessToken
	if out.Data.RefreshToken != "" {
		c.RefreshToken = out.Data.RefreshToken
	}
	c.SavedAt = time.Now().Format(time.RFC3339)
	if err := store.Save(c); err != nil {
		return err
	}
	fmt.Println("token diperbarui.")
	return cmdStatus()
}

func cmdStatus() error {
	c, err := store.Load()
	if err != nil {
		return err
	}
	// if --raw flag, print raw token
	if len(os.Args) > 2 && os.Args[2] == "--raw" {
		fmt.Println(c.AccessToken)
		return nil
	}
	fmt.Println("status   : login")
	fmt.Println("provider :", c.Provider)
	fmt.Println("user     :", c.UserName, c.UserID)
	fmt.Println("access   :", redact(c.AccessToken))
	fmt.Println("refresh  :", redact(c.RefreshToken))
	fmt.Println("disimpan :", c.SavedAt)
	return nil
}

// redact menampilkan hanya karakter awal.
func redact(s string) string {
	if s == "" {
		return "(kosong)"
	}
	prefix := s
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	return fmt.Sprintf("%s... (len=%d)", prefix, len(s))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
