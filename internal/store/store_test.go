package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hirotomasato/autoclawpi/internal/db"
)

// initDB menyiapkan DB sekali per paket (tempdir, tidak dihapus otomatis
// supaya tidak bentrok dengan handle SQLite).
var dbReady bool

func initDB(t *testing.T) {
	t.Helper()
	if dbReady {
		return
	}
	dir, err := os.MkdirTemp("", "autoclawpi-store-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	if err := db.Init(dir); err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	dbReady = true
}

func reset(t *testing.T) {
	t.Helper()
	initDB(t)
	accounts, err := db.ListAccounts()
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	for _, a := range accounts {
		if err := db.DeleteAccount(a.ID); err != nil {
			t.Fatalf("DeleteAccount: %v", err)
		}
	}
}

func creds(tok string) *Creds {
	return &Creds{AccessToken: "Bearer " + tok, RefreshToken: "Bearer " + tok + "-r",
		Provider: "zai", SavedAt: "2026-09-15T00:00:00Z"}
}

// Save MENIMPA akun pertama (perilaku lama harus tetap utuh).
func TestSave_OverwritesFirstAccount(t *testing.T) {
	reset(t)
	if err := Save(creds("tok-1")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := Save(creds("tok-2")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	accounts, _ := db.ListAccounts()
	if len(accounts) != 1 {
		t.Fatalf("jumlah akun = %d, want 1 (Save harus menimpa)", len(accounts))
	}
	if accounts[0].AccessToken != "Bearer tok-2" {
		t.Errorf("token = %q, want token terbaru", accounts[0].AccessToken)
	}
}

// Add MENAMBAH akun baru dan tidak menyentuh akun lama — ini inti fitur加号.
func TestAdd_AppendsWithoutTouchingOthers(t *testing.T) {
	reset(t)
	if err := Save(creds("tok-original")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	before, _ := db.ListAccounts()

	id, err := Add("second", creds("tok-second"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	after, _ := db.ListAccounts()
	if len(after) != 2 {
		t.Fatalf("jumlah akun = %d, want 2", len(after))
	}
	// Akun lama harus utuh (id, token, nama).
	orig, err := db.GetAccount(before[0].ID)
	if err != nil || orig == nil {
		t.Fatalf("akun lama hilang: %v", err)
	}
	if orig.AccessToken != "Bearer tok-original" {
		t.Errorf("akun lama tertimpa! token = %q", orig.AccessToken)
	}
	if orig.Name != before[0].Name {
		t.Errorf("nama akun lama berubah: %q -> %q", before[0].Name, orig.Name)
	}
	// Akun baru benar.
	added, _ := db.GetAccount(id)
	if added == nil || added.Name != "second" {
		t.Errorf("akun baru tidak benar: %+v", added)
	}
}

// Add berkali-kali harus terus menambah, bukan menimpa.
func TestAdd_MultipleAccounts(t *testing.T) {
	reset(t)
	for i := 1; i <= 3; i++ {
		if _, err := Add("", creds("tok-"+string(rune('0'+i)))); err != nil {
			t.Fatalf("Add #%d: %v", i, err)
		}
	}
	accounts, _ := db.ListAccounts()
	if len(accounts) != 3 {
		t.Fatalf("jumlah akun = %d, want 3", len(accounts))
	}
	// Semua token harus berbeda (tidak saling menimpa).
	seen := map[string]bool{}
	for _, a := range accounts {
		if seen[a.AccessToken] {
			t.Errorf("token duplikat: %q", a.AccessToken)
		}
		seen[a.AccessToken] = true
	}
}

// device_id harus BERBEDA tiap akun (ciri korelasi kalau sama).
func TestAdd_UniqueDeviceIDPerAccount(t *testing.T) {
	reset(t)
	for i := 0; i < 5; i++ {
		if _, err := Add("", creds("tok-"+string(rune('a'+i)))); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	accounts, _ := db.ListAccounts()
	seen := map[string]bool{}
	for _, a := range accounts {
		if a.DeviceID == "" {
			t.Errorf("akun #%d device_id kosong", a.ID)
		}
		if seen[a.DeviceID] {
			t.Errorf("device_id duplikat: %q", a.DeviceID)
		}
		seen[a.DeviceID] = true
	}
}

// Nama default: pakai UserName kalau ada, else acct-N.
func TestAdd_DefaultName(t *testing.T) {
	reset(t)

	c := creds("tok-named")
	c.UserName = "xsh xin"
	if _, err := Add("", c); err != nil {
		t.Fatalf("Add: %v", err)
	}
	accounts, _ := db.ListAccounts()
	if accounts[0].Name != "xsh xin" {
		t.Errorf("name = %q, want UserName dari login", accounts[0].Name)
	}

	// Tanpa UserName → acct-N
	if _, err := Add("", creds("tok-anon")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	accounts, _ = db.ListAccounts()
	if !strings.HasPrefix(accounts[1].Name, "acct-") {
		t.Errorf("name = %q, want prefix acct-", accounts[1].Name)
	}
}

// DeviceID yang sudah diisi tidak boleh ditimpa oleh Add.
func TestAdd_PreservesExplicitDeviceID(t *testing.T) {
	reset(t)
	c := creds("tok-x")
	c.DeviceID = "my-explicit-device"
	if _, err := Add("", c); err != nil {
		t.Fatalf("Add: %v", err)
	}
	accounts, _ := db.ListAccounts()
	if accounts[0].DeviceID != "my-explicit-device" {
		t.Errorf("device_id = %q, want tetap", accounts[0].DeviceID)
	}
}

// newDeviceID harus unik dan tidak memakai prefix hostname.
func TestNewDeviceID_UniqueAndNoHostPrefix(t *testing.T) {
	host, _ := os.Hostname()
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id := newDeviceID()
		if seen[id] {
			t.Fatalf("device_id duplikat: %q", id)
		}
		seen[id] = true
		if host != "" && strings.HasPrefix(strings.ToLower(id), strings.ToLower(host)) {
			t.Errorf("device_id %q memakai prefix hostname %q", id, host)
		}
	}
}

// Clear hanya menghapus akun pertama (perilaku lama tetap).
func TestClear_RemovesFirstOnly(t *testing.T) {
	reset(t)
	if err := Save(creds("tok-a")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := Add("keep-me", creds("tok-b")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	accounts, _ := db.ListAccounts()
	if len(accounts) != 1 {
		t.Fatalf("jumlah akun = %d, want 1", len(accounts))
	}
	if accounts[0].Name != "keep-me" {
		t.Errorf("akun yang tersisa = %q, want keep-me", accounts[0].Name)
	}
}

// Add dengan IP pool: akun baru otomatis dapat slot + proxy berurutan.
func TestAdd_AssignsSlotAndProxyFromPool(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	poolPath := filepath.Join(dir, "ip-pool.json")
	if err := os.WriteFile(poolPath, []byte(`{"proxies":[
		{"name":"taiwan1","url":"http://127.0.0.1:7901"},
		{"name":"taiwan2","url":"http://127.0.0.1:7902"}]}`), 0o600); err != nil {
		t.Fatalf("write pool: %v", err)
	}
	t.Setenv("AUTOCLAWPI_IP_POOL", poolPath)

	id1, err := Add("a1", creds("tok-1"))
	if err != nil {
		t.Fatalf("Add 1: %v", err)
	}
	id2, err := Add("a2", creds("tok-2"))
	if err != nil {
		t.Fatalf("Add 2: %v", err)
	}

	a1, _ := db.GetAccount(id1)
	a2, _ := db.GetAccount(id2)
	if a1.Slot != 1 || a1.Proxy != "http://127.0.0.1:7901" {
		t.Errorf("akun1: slot=%d proxy=%q (want 1 / :7901)", a1.Slot, a1.Proxy)
	}
	if a2.Slot != 2 || a2.Proxy != "http://127.0.0.1:7902" {
		t.Errorf("akun2: slot=%d proxy=%q (want 2 / :7902)", a2.Slot, a2.Proxy)
	}
}

// Pool habis → Add gagal (jangan diam-diam berbagi IP).
func TestAdd_PoolExhaustedFails(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	poolPath := filepath.Join(dir, "ip-pool.json")
	if err := os.WriteFile(poolPath, []byte(
		`{"proxies":[{"name":"only","url":"http://127.0.0.1:7901"}]}`), 0o600); err != nil {
		t.Fatalf("write pool: %v", err)
	}
	t.Setenv("AUTOCLAWPI_IP_POOL", poolPath)

	if _, err := Add("a1", creds("tok-1")); err != nil {
		t.Fatalf("Add 1 harus sukses: %v", err)
	}
	if _, err := Add("a2", creds("tok-2")); err == nil {
		t.Fatal("Add 2 harus gagal karena pool habis")
	} else {
		t.Logf("error sesuai harapan: %v", err)
	}
}

// --proxy eksplisit menang atas pool.
func TestAdd_ExplicitProxyWins(t *testing.T) {
	reset(t)
	t.Setenv("AUTOCLAWPI_IP_POOL", filepath.Join(t.TempDir(), "none.json"))

	c := creds("tok-x")
	c.Proxy = "http://127.0.0.1:7999"
	id, err := Add("explicit", c)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	a, _ := db.GetAccount(id)
	if a.Proxy != "http://127.0.0.1:7999" {
		t.Errorf("proxy = %q, want nilai eksplisit", a.Proxy)
	}
}

// Tanpa pool → tetap jalan tanpa proxy, DAN tidak diberi slot.
// (slot tanpa proxy itu menyesatkan: akun tampak terpasang padahal direct)
func TestAdd_NoPoolMeansNoProxyAndNoSlot(t *testing.T) {
	reset(t)
	t.Setenv("AUTOCLAWPI_IP_POOL", filepath.Join(t.TempDir(), "none.json"))

	id, err := Add("plain", creds("tok-plain"))
	if err != nil {
		t.Fatalf("Add tanpa pool harus sukses: %v", err)
	}
	a, _ := db.GetAccount(id)
	if a.Proxy != "" {
		t.Errorf("proxy = %q, want kosong", a.Proxy)
	}
	if a.Slot != 0 {
		t.Errorf("slot = %d, want 0 (pool kosong tidak boleh mengisi slot)", a.Slot)
	}
}

// Pool ada tapi kosong (proxies: []) → juga tidak diberi slot.
func TestAdd_EmptyPoolListNoSlot(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "ip-pool.json")
	if err := os.WriteFile(p, []byte(`{"proxies":[]}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("AUTOCLAWPI_IP_POOL", p)

	id, err := Add("empty", creds("tok-empty"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	a, _ := db.GetAccount(id)
	if a.Slot != 0 || a.Proxy != "" {
		t.Errorf("slot=%d proxy=%q, want 0/''", a.Slot, a.Proxy)
	}
}

// NextFreeSlot mengisi lubang slot yang kosong.
func TestNextFreeSlot_FillsGaps(t *testing.T) {
	reset(t)
	c1 := creds("t1")
	c1.Slot = 1
	if _, err := Add("s1", c1); err != nil {
		t.Fatalf("Add: %v", err)
	}
	c3 := creds("t3")
	c3.Slot = 3
	if _, err := Add("s3", c3); err != nil {
		t.Fatalf("Add: %v", err)
	}

	slot, err := db.NextFreeSlot()
	if err != nil {
		t.Fatalf("NextFreeSlot: %v", err)
	}
	if slot != 2 {
		t.Errorf("NextFreeSlot = %d, want 2 (lubang di tengah)", slot)
	}
}
