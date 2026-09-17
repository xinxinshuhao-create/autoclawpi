// Package ippool membaca registri IP (ip-pool.json) dan memasangkan akun
// ke IP berdasarkan nomor urut (slot).
//
// Format ip-pool.json:
//
//	{
//	  "proxies": [
//	    {"name": "taiwan1",  "url": "http://127.0.0.1:7901"},
//	    {"name": "taiwan2",  "url": "http://127.0.0.1:7902"},
//	    {"name": "riben1",   "url": "http://127.0.0.1:7904"}
//	  ]
//	}
//
// Aturan pemasangan: akun dengan slot N memakai proxies[N-1].
// Slot 0 / pool kosong → tanpa proxy (perilaku lama, tetap kompatibel).
//
// Lokasi file bisa diatur lewat env AUTOCLAWPI_IP_POOL; default
// <AUTOCLAWPI_DIR>/ip-pool.json supaya ikut pindah bersama data.
package ippool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hirotomasato/autoclawpi/internal/config"
)

// Entry adalah satu IP/proxy di pool.
type Entry struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// Note opsional untuk catatan (mis. "台湾-01 节点").
	Note string `json:"note,omitempty"`
}

// Pool adalah isi ip-pool.json.
type Pool struct {
	Proxies []Entry `json:"proxies"`
}

// Path mengembalikan lokasi ip-pool.json.
func Path() string {
	if p := os.Getenv("AUTOCLAWPI_IP_POOL"); p != "" {
		return p
	}
	return filepath.Join(config.Dir(), "ip-pool.json")
}

// Load membaca pool. File tidak ada → pool kosong (bukan error), supaya
// instalasi lama tetap jalan tanpa konfigurasi IP.
func Load() (*Pool, error) {
	p := Path()
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &Pool{}, nil
		}
		return nil, fmt.Errorf("baca ip-pool: %w", err)
	}
	var pool Pool
	if err := json.Unmarshal(b, &pool); err != nil {
		return nil, fmt.Errorf("parse ip-pool %s: %w", p, err)
	}
	return &pool, nil
}

// Count mengembalikan jumlah IP yang terdaftar.
func (p *Pool) Count() int {
	if p == nil {
		return 0
	}
	return len(p.Proxies)
}

// ForSlot mengembalikan URL proxy untuk slot tertentu (1-based).
//
//	slot <= 0      → "" (tanpa proxy)
//	slot > Count() → error ErrPoolExhausted
func (p *Pool) ForSlot(slot int) (string, error) {
	if slot <= 0 || p == nil || len(p.Proxies) == 0 {
		return "", nil
	}
	if slot > len(p.Proxies) {
		return "", fmt.Errorf("%w: slot %d diminta tapi pool hanya punya %d IP",
			ErrPoolExhausted, slot, len(p.Proxies))
	}
	return p.Proxies[slot-1].URL, nil
}

// NameForSlot mengembalikan nama IP untuk slot (untuk tampilan).
func (p *Pool) NameForSlot(slot int) string {
	if p == nil || slot <= 0 || slot > len(p.Proxies) {
		return ""
	}
	e := p.Proxies[slot-1]
	if e.Name != "" {
		return e.Name
	}
	return e.URL
}

// Names mengembalikan semua nama IP (untuk pesan error).
func (p *Pool) Names() []string {
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(p.Proxies))
	for _, e := range p.Proxies {
		if e.Name != "" {
			out = append(out, e.Name)
		} else {
			out = append(out, e.URL)
		}
	}
	return out
}

// ErrPoolExhausted dikembalikan saat slot melebihi jumlah IP di pool.
var ErrPoolExhausted = fmt.Errorf("IP pool habis")

// ProxyForSlot adalah helper praktis: baca pool lalu ambil proxy slot.
// Pool tidak ada / kosong → "" tanpa error.
func ProxyForSlot(slot int) (string, error) {
	pool, err := Load()
	if err != nil {
		return "", err
	}
	return pool.ForSlot(slot)
}

// Validate memeriksa kewajaran pool (dipakai saat startup / CLI).
func (p *Pool) Validate() error {
	if p == nil {
		return nil
	}
	seen := make(map[string]bool, len(p.Proxies))
	for i, e := range p.Proxies {
		if e.URL == "" {
			return fmt.Errorf("proxies[%d]: url kosong", i)
		}
		if !strings.Contains(e.URL, "://") {
			return fmt.Errorf("proxies[%d] (%s): url harus punya skema, mis. http://127.0.0.1:7901",
				i, e.URL)
		}
		if seen[e.URL] {
			return fmt.Errorf("proxies[%d]: url duplikat %s (satu IP untuk dua akun = tidak terisolasi)",
				i, e.URL)
		}
		seen[e.URL] = true
	}
	return nil
}
