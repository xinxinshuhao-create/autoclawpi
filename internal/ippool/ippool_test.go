package ippool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writePool(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "ip-pool.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write pool: %v", err)
	}
	t.Setenv("AUTOCLAWPI_IP_POOL", p)
	return p
}

// Slot N memakai IP ke-N (aturan utama: 号N ↔ IP N).
func TestForSlot_MapsSequentially(t *testing.T) {
	writePool(t, `{"proxies":[
		{"name":"taiwan1","url":"http://127.0.0.1:7901"},
		{"name":"taiwan2","url":"http://127.0.0.1:7902"},
		{"name":"riben1","url":"http://127.0.0.1:7904"}]}`)

	pool, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cases := map[int]string{
		1: "http://127.0.0.1:7901",
		2: "http://127.0.0.1:7902",
		3: "http://127.0.0.1:7904",
	}
	for slot, want := range cases {
		got, err := pool.ForSlot(slot)
		if err != nil {
			t.Fatalf("ForSlot(%d): %v", slot, err)
		}
		if got != want {
			t.Errorf("ForSlot(%d) = %q, want %q", slot, got, want)
		}
	}
}

// slot <= 0 → tanpa proxy (perilaku lama tetap jalan).
func TestForSlot_ZeroOrNegativeMeansNoProxy(t *testing.T) {
	writePool(t, `{"proxies":[{"name":"a","url":"http://127.0.0.1:7901"}]}`)
	pool, _ := Load()
	for _, slot := range []int{0, -1} {
		got, err := pool.ForSlot(slot)
		if err != nil {
			t.Errorf("ForSlot(%d) error: %v", slot, err)
		}
		if got != "" {
			t.Errorf("ForSlot(%d) = %q, want kosong", slot, got)
		}
	}
}

// Slot melebihi jumlah IP → ErrPoolExhausted (bukan diam-diam reuse).
func TestForSlot_Exhausted(t *testing.T) {
	writePool(t, `{"proxies":[{"name":"a","url":"http://127.0.0.1:7901"}]}`)
	pool, _ := Load()
	_, err := pool.ForSlot(2)
	if err == nil {
		t.Fatal("slot 2 harus gagal karena pool hanya punya 1 IP")
	}
	if !os.IsNotExist(err) && err.Error() == "" {
		t.Errorf("error kosong")
	}
	t.Logf("error: %v", err)
}

// File tidak ada → pool kosong, bukan error (kompatibilitas ke belakang).
func TestLoad_MissingFileIsEmpty(t *testing.T) {
	t.Setenv("AUTOCLAWPI_IP_POOL", filepath.Join(t.TempDir(), "nope.json"))
	pool, err := Load()
	if err != nil {
		t.Fatalf("Load harus toleran saat file tidak ada, dapat: %v", err)
	}
	if pool.Count() != 0 {
		t.Errorf("Count = %d, want 0", pool.Count())
	}
	// Dan ForSlot tetap aman.
	if got, err := pool.ForSlot(1); err != nil || got != "" {
		t.Errorf("ForSlot(1) = %q, %v; want kosong tanpa error", got, err)
	}
}

// JSON rusak → error jelas (jangan diam-diam tanpa proxy, itu membocorkan IP).
func TestLoad_MalformedJSON(t *testing.T) {
	writePool(t, `{"proxies": [ this is not json`)
	if _, err := Load(); err == nil {
		t.Fatal("JSON rusak harus menghasilkan error")
	}
}

// Validate: URL tanpa skema ditolak.
func TestValidate_RejectsSchemeLessURL(t *testing.T) {
	p := &Pool{Proxies: []Entry{{Name: "x", URL: "127.0.0.1:7901"}}}
	if err := p.Validate(); err == nil {
		t.Error("URL tanpa skema harus ditolak")
	}
}

// Validate: URL duplikat ditolak (satu IP untuk dua akun = tidak terisolasi).
func TestValidate_RejectsDuplicateURL(t *testing.T) {
	p := &Pool{Proxies: []Entry{
		{Name: "a", URL: "http://127.0.0.1:7901"},
		{Name: "b", URL: "http://127.0.0.1:7901"},
	}}
	if err := p.Validate(); err == nil {
		t.Error("URL duplikat harus ditolak")
	}
}

// Validate: pool yang benar lolos.
func TestValidate_AcceptsGoodPool(t *testing.T) {
	p := &Pool{Proxies: []Entry{
		{Name: "taiwan1", URL: "http://127.0.0.1:7901"},
		{Name: "riben1", URL: "http://127.0.0.1:7904"},
	}}
	if err := p.Validate(); err != nil {
		t.Errorf("pool valid malah ditolak: %v", err)
	}
}

// Names untuk pesan error.
func TestNames(t *testing.T) {
	p := &Pool{Proxies: []Entry{
		{Name: "taiwan1", URL: "http://127.0.0.1:7901"},
		{URL: "http://127.0.0.1:7902"}, // tanpa name → pakai URL
	}}
	got := p.Names()
	if len(got) != 2 || got[0] != "taiwan1" || got[1] != "http://127.0.0.1:7902" {
		t.Errorf("Names = %v", got)
	}
}

// ProxyForSlot membaca file lewat env (jalur yang dipakai store.Add).
func TestProxyForSlot(t *testing.T) {
	writePool(t, `{"proxies":[{"name":"a","url":"http://127.0.0.1:7901"},
	                          {"name":"b","url":"http://127.0.0.1:7902"}]}`)
	got, err := ProxyForSlot(2)
	if err != nil {
		t.Fatalf("ProxyForSlot: %v", err)
	}
	if got != "http://127.0.0.1:7902" {
		t.Errorf("got %q", got)
	}
}

// Format contoh di doc harus benar-benar bisa diparse.
func TestDocExampleParses(t *testing.T) {
	var p Pool
	doc := `{"proxies":[
	  {"name": "taiwan1",  "url": "http://127.0.0.1:7901"},
	  {"name": "taiwan2",  "url": "http://127.0.0.1:7902"}
	]}`
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatalf("contoh di doc tidak bisa diparse: %v", err)
	}
	if p.Count() != 2 {
		t.Errorf("Count = %d, want 2", p.Count())
	}
}
