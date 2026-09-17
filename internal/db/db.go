// Package db menyediakan akses SQLite untuk autoclawpi.
// Schema: accounts + checkin_log + config.
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var DB *sql.DB

// Account menyimpan satu akun AutoClaw.
type Account struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	AccessToken  string `json:"-"` // plaintext di memory, encrypted di disk
	RefreshToken string `json:"-"`
	Provider     string `json:"provider"` // zai | google
	UserID       string `json:"user_id"`
	UserName     string `json:"user_name"`
	DeviceID     string `json:"device_id"`
	Points       int    `json:"points"`
	// Active = saklar manual user (permanen sampai diubah).
	Active bool `json:"active"`
	// CooldownUntil = penonaktifan OTOMATIS sementara (RFC3339, kosong = tidak
	// sedang cooldown). Diisi saat upstream membalas error kuota/limit/ban, dan
	// otomatis dianggap sembuh setelah waktunya lewat. Dipisah dari Active
	// supaya keputusan user tidak tertimpa oleh logika otomatis.
	CooldownUntil string `json:"cooldown_until"`
	// CooldownReason menyimpan alasan terakhir (untuk tampilan/diagnosa).
	CooldownReason string `json:"cooldown_reason"`
	// Proxy = URL proxy keluar KHUSUS akun ini (mis. "http://127.0.0.1:7901").
	// Kosong = pakai transport default (perilaku lama, tanpa proxy).
	// Dipakai untuk memisahkan IP antar akun supaya tidak terlihat satu
	// perangkat/ jaringan oleh upstream.
	Proxy string `json:"proxy"`
	// Slot = nomor urut yang dipakai untuk memasangkan akun ke IP pool
	// (slot N <-> IP ke-N). 0 = belum dipetakan.
	Slot       int    `json:"slot"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at"`
}

// CheckinLog mencatat history check-in.
type CheckinLog struct {
	ID        int64  `json:"id"`
	AccountID int64  `json:"account_id"`
	Date      string `json:"date"`    // YYYY-MM-DD
	TaskID    string `json:"task_id"` // daily_signin, dll
	Points    int    `json:"points"`
	Status    string `json:"status"` // success | already_done | failed
	DeviceID  string `json:"device_id"`
	CreatedAt string `json:"created_at"`
}

// Init membuka/membuat database dan menjalankan migrasi.
func Init(dir string) error {
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dir = filepath.Join(home, ".autoclawpi")
	}
	_ = os.MkdirAll(dir, 0o700)
	dbPath := filepath.Join(dir, "autoclawpi.db")

	var err error
	DB, err = sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	DB.SetMaxOpenConns(1) // SQLite single-writer

	if err := migrate(); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// Close menutup database.
func Close() {
	if DB != nil {
		DB.Close()
	}
}

func migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS accounts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL DEFAULT '',
		access_token TEXT NOT NULL DEFAULT '',
		refresh_token TEXT NOT NULL DEFAULT '',
		provider TEXT NOT NULL DEFAULT 'zai',
		user_id TEXT NOT NULL DEFAULT '',
		user_name TEXT NOT NULL DEFAULT '',
		device_id TEXT NOT NULL DEFAULT '',
		points INTEGER NOT NULL DEFAULT 0,
		active INTEGER NOT NULL DEFAULT 1,
		cooldown_until TEXT NOT NULL DEFAULT '',
		cooldown_reason TEXT NOT NULL DEFAULT '',
		proxy TEXT NOT NULL DEFAULT '',
		slot INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		last_used_at TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS checkin_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id INTEGER NOT NULL,
		date TEXT NOT NULL,
		task_id TEXT NOT NULL,
		points INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'failed',
		device_id TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		FOREIGN KEY (account_id) REFERENCES accounts(id)
	);

	CREATE INDEX IF NOT EXISTS idx_checkin_log_account_date ON checkin_log(account_id, date);

	CREATE TABLE IF NOT EXISTS config (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id INTEGER NOT NULL DEFAULT 0,
		model TEXT NOT NULL DEFAULT '',
		prompt_tokens INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens INTEGER NOT NULL DEFAULT 0,
		cost REAL NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'success',
		error TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	);
	`
	_, err := DB.Exec(schema)
	if err != nil {
		return err
	}
	return migrateColumns()
}

// migrateColumns menambahkan kolom baru ke DB yang sudah ada.
// CREATE TABLE IF NOT EXISTS tidak menyentuh tabel lama, jadi kolom baru harus
// ditambahkan lewat ALTER TABLE. Idempoten: kalau kolom sudah ada, error
// "duplicate column name" diabaikan.
func migrateColumns() error {
	alters := []string{
		`ALTER TABLE accounts ADD COLUMN cooldown_until TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN cooldown_reason TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN proxy TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN slot INTEGER NOT NULL DEFAULT 0`,
	}
	for _, stmt := range alters {
		if _, err := DB.Exec(stmt); err != nil {
			if strings.Contains(err.Error(), "duplicate column name") {
				continue // sudah ada — normal untuk DB lama
			}
			return fmt.Errorf("alter: %w", err)
		}
	}
	return nil
}

// --- Account CRUD ---

// AddAccount menyimpan akun baru.
func AddAccount(name, accessToken, refreshToken, provider, userID, userName, deviceID string) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := DB.Exec(`INSERT INTO accounts (name, access_token, refresh_token, provider, user_id, user_name, device_id, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		name, accessToken, refreshToken, provider, userID, userName, deviceID, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListAccounts mengembalikan semua akun.
func ListAccounts() ([]Account, error) {
	rows, err := DB.Query(`SELECT id, name, access_token, refresh_token, provider, user_id, user_name, device_id, points, active, cooldown_until, cooldown_reason, proxy, slot, created_at, last_used_at FROM accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accounts []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.Name, &a.AccessToken, &a.RefreshToken, &a.Provider, &a.UserID, &a.UserName, &a.DeviceID, &a.Points, &a.Active, &a.CooldownUntil, &a.CooldownReason, &a.Proxy, &a.Slot, &a.CreatedAt, &a.LastUsedAt); err != nil {
			return nil, err
		}
		accounts = append(accounts, a)
	}
	return accounts, nil
}

// GetAccount mengambil satu akun berdasarkan ID.
func GetAccount(id int64) (*Account, error) {
	row := DB.QueryRow(`SELECT id, name, access_token, refresh_token, provider, user_id, user_name, device_id, points, active, cooldown_until, cooldown_reason, proxy, slot, created_at, last_used_at FROM accounts WHERE id = ?`, id)
	var a Account
	if err := row.Scan(&a.ID, &a.Name, &a.AccessToken, &a.RefreshToken, &a.Provider, &a.UserID, &a.UserName, &a.DeviceID, &a.Points, &a.Active, &a.CooldownUntil, &a.CooldownReason, &a.Proxy, &a.Slot, &a.CreatedAt, &a.LastUsedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

// GetActiveAccount mengembalikan akun aktif pertama (untuk single-account mode).
func GetActiveAccount() (*Account, error) {
	row := DB.QueryRow(`SELECT id, name, access_token, refresh_token, provider, user_id, user_name, device_id, points, active, cooldown_until, cooldown_reason, proxy, slot, created_at, last_used_at FROM accounts WHERE active = 1 ORDER BY id LIMIT 1`)
	var a Account
	if err := row.Scan(&a.ID, &a.Name, &a.AccessToken, &a.RefreshToken, &a.Provider, &a.UserID, &a.UserName, &a.DeviceID, &a.Points, &a.Active, &a.CooldownUntil, &a.CooldownReason, &a.Proxy, &a.Slot, &a.CreatedAt, &a.LastUsedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

// UpdateAccount memperbarui data akun.
func UpdateAccount(a *Account) error {
	_, err := DB.Exec(`UPDATE accounts SET name=?, access_token=?, refresh_token=?, provider=?, user_id=?, user_name=?, device_id=?, points=?, active=?, cooldown_until=?, cooldown_reason=?, proxy=?, slot=?, last_used_at=? WHERE id=?`,
		a.Name, a.AccessToken, a.RefreshToken, a.Provider, a.UserID, a.UserName, a.DeviceID, a.Points, a.Active, a.CooldownUntil, a.CooldownReason, a.Proxy, a.Slot, a.LastUsedAt, a.ID)
	return err
}

// DeleteAccount menghapus akun.
func DeleteAccount(id int64) error {
	_, err := DB.Exec(`DELETE FROM accounts WHERE id = ?`, id)
	return err
}

// --- Cooldown akun (auto-disable sementara) ---
// SetAccountProxy menyimpan proxy + slot untuk satu akun.
func SetAccountProxy(id int64, proxy string, slot int) error {
	_, err := DB.Exec(`UPDATE accounts SET proxy=?, slot=? WHERE id=?`, proxy, slot, id)
	return err
}

// NextFreeSlot mengembalikan slot berikutnya yang belum dipakai.
// Dipakai untuk memasangkan akun baru ke IP pool secara berurutan.
func NextFreeSlot() (int, error) {
	accounts, err := ListAccounts()
	if err != nil {
		return 0, err
	}
	used := make(map[int]bool, len(accounts))
	maxSlot := 0
	for _, a := range accounts {
		if a.Slot > 0 {
			used[a.Slot] = true
			if a.Slot > maxSlot {
				maxSlot = a.Slot
			}
		}
	}
	for i := 1; i <= maxSlot+1; i++ {
		if !used[i] {
			return i, nil
		}
	}
	return maxSlot + 1, nil
}

// CooldownReason* adalah kategori yang dipakai untuk menentukan lama cooldown.
const (
	// CooldownQuota: kuota/limit/欠费. Kuota biasanya reset harian → cooldown pendek.
	CooldownQuota = "quota"
	// CooldownBanned: akun diblokir upstream (410004) → cooldown panjang,
	// praktis butuh intervensi manual, tapi tetap diberi batas waktu supaya
	// tidak perlu edit DB kalau ternyata pulih.
	CooldownBanned = "banned"
	// CooldownRateLimit: rate limit sementara (429 / high demand 810002).
	CooldownRateLimit = "rate_limit"
	// CooldownUnauthorized: token invalid dan refresh gagal.
	CooldownUnauthorized = "unauthorized"
	// CooldownNetwork: proxy/egress tidak bisa dihubungi (node mati, timeout,
	// connection refused). Ini BUKAN salah akun — token masih valid — tapi
	// selama jalur keluarnya mati, akun ini tidak bisa dipakai. Tanpa cooldown,
	// setiap request akan mencoba akun ini dulu, gagal, baru failover: hasilnya
	// semua request melambat ~2s dan log dipenuhi error yang sama.
	// Cooldown pendek supaya begitu node pulih, akun otomatis kembali.
	CooldownNetwork = "network"
)

// cooldownDurations memetakan kategori ke lama cooldown.
var cooldownDurations = map[string]time.Duration{
	CooldownRateLimit:    2 * time.Minute,
	CooldownQuota:        30 * time.Minute,
	CooldownUnauthorized: 15 * time.Minute,
	CooldownBanned:       24 * time.Hour,
	CooldownNetwork:      5 * time.Minute,
}

// CooldownDuration mengembalikan lama cooldown untuk sebuah kategori.
func CooldownDuration(reason string) time.Duration {
	if d, ok := cooldownDurations[reason]; ok {
		return d
	}
	return 10 * time.Minute
}

// SetCooldown menandai akun tidak layak pakai sampai now+durasi.
// Tidak mengubah Active — itu saklar manual user.
func SetCooldown(id int64, reason string) error {
	until := time.Now().UTC().Add(CooldownDuration(reason)).Format(time.RFC3339)
	_, err := DB.Exec(`UPDATE accounts SET cooldown_until=?, cooldown_reason=? WHERE id=?`,
		until, reason, id)
	return err
}

// ClearCooldown menghapus status cooldown (dipakai saat akun berhasil).
func ClearCooldown(id int64) error {
	_, err := DB.Exec(`UPDATE accounts SET cooldown_until='', cooldown_reason='' WHERE id=?`, id)
	return err
}

// IsCoolingDown melaporkan apakah akun sedang cooldown pada waktu now.
// Cooldown yang sudah lewat dianggap sembuh (auto-recover).
func (a *Account) IsCoolingDown(now time.Time) bool {
	if a.CooldownUntil == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, a.CooldownUntil)
	if err != nil {
		return false // timestamp rusak → anggap tidak cooldown
	}
	return now.Before(t)
}

// Usable melaporkan apakah akun boleh dipakai: aktif manual, ada token,
// dan tidak sedang cooldown.
func (a *Account) Usable(now time.Time) bool {
	return a.Active && a.AccessToken != "" && !a.IsCoolingDown(now)
}

// ClearExpiredCooldowns membersihkan cooldown yang sudah lewat supaya DB tidak
// menumpuk status basi. Dipanggil berkala dari server.
func ClearExpiredCooldowns() (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := DB.Exec(`UPDATE accounts SET cooldown_until='', cooldown_reason='' WHERE cooldown_until != '' AND cooldown_until <= ?`, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// UpdatePoints menambah poin akun.
func UpdatePoints(id int64, points int) error {
	_, err := DB.Exec(`UPDATE accounts SET points = ? WHERE id = ?`, points, id)
	return err
}

// --- Checkin Log ---

// AddCheckinLog mencatat hasil check-in.
func AddCheckinLog(accountID int64, date, taskID string, points int, status, deviceID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := DB.Exec(`INSERT INTO checkin_log (account_id, date, task_id, points, status, device_id, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		accountID, date, taskID, points, status, deviceID, now)
	return err
}

// GetCheckinLog mengembalikan log check-in untuk akun pada tanggal tertentu.
func GetCheckinLog(accountID int64, date, taskID string) (*CheckinLog, error) {
	row := DB.QueryRow(`SELECT id, account_id, date, task_id, points, status, device_id, created_at FROM checkin_log WHERE account_id = ? AND date = ? AND task_id = ?`, accountID, date, taskID)
	var l CheckinLog
	if err := row.Scan(&l.ID, &l.AccountID, &l.Date, &l.TaskID, &l.Points, &l.Status, &l.DeviceID, &l.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &l, nil
}

// ListCheckinLog mengembalikan semua log check-in untuk akun.
func ListCheckinLog(accountID int64, limit int) ([]CheckinLog, error) {
	rows, err := DB.Query(`SELECT id, account_id, date, task_id, points, status, device_id, created_at FROM checkin_log WHERE account_id = ? ORDER BY created_at DESC LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []CheckinLog
	for rows.Next() {
		var l CheckinLog
		if err := rows.Scan(&l.ID, &l.AccountID, &l.Date, &l.TaskID, &l.Points, &l.Status, &l.DeviceID, &l.CreatedAt); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, nil
}

// --- Config ---

// GetConfig mengambil nilai konfigurasi.
func GetConfig(key string) (string, error) {
	row := DB.QueryRow(`SELECT value FROM config WHERE key = ?`, key)
	var val string
	if err := row.Scan(&val); err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return val, nil
}

// SetConfig menyimpan nilai konfigurasi.
func SetConfig(key, value string) error {
	_, err := DB.Exec(`INSERT OR REPLACE INTO config (key, value) VALUES (?, ?)`, key, value)
	return err
}

// ── Logs ────────────────────────────────────────────────────────────

type LogEntry struct {
	ID               int64   `json:"id"`
	AccountID        int64   `json:"account_id"`
	Model            string  `json:"model"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	Cost             float64 `json:"cost"`
	Status           string  `json:"status"`
	Error            string  `json:"error"`
	CreatedAt        string  `json:"created_at"`
}

func AddLog(l *LogEntry) (int64, error) {
	res, err := DB.Exec(`INSERT INTO logs (account_id, model, prompt_tokens, completion_tokens, total_tokens, cost, status, error, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))`,
		l.AccountID, l.Model, l.PromptTokens, l.CompletionTokens, l.TotalTokens, l.Cost, l.Status, l.Error)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func ListLogs(limit int) ([]LogEntry, error) {
	if limit < 1 {
		limit = 50
	}
	rows, err := DB.Query(`SELECT id, account_id, model, prompt_tokens, completion_tokens, total_tokens, cost, status, error, created_at FROM logs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var logs []LogEntry
	for rows.Next() {
		var l LogEntry
		if err := rows.Scan(&l.ID, &l.AccountID, &l.Model, &l.PromptTokens, &l.CompletionTokens, &l.TotalTokens, &l.Cost, &l.Status, &l.Error, &l.CreatedAt); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, rows.Err()
}

func LogStats() (totalReq int, totalTokens int, totalCost float64, err error) {
	err = DB.QueryRow(`SELECT COUNT(*), COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost),0) FROM logs WHERE created_at >= datetime('now', 'start of day')`).Scan(&totalReq, &totalTokens, &totalCost)
	return
}
