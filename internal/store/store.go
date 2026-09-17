// Package store menyediakan akses kredensial akun (single-account mode).
// Backend: SQLite via internal/db.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/hirotomasato/autoclawpi/internal/db"
	"github.com/hirotomasato/autoclawpi/internal/ippool"
)

// Creds adalah kredensial akun AutoClaw (single-account compatibility).
type Creds struct {
	AccessToken  string `json:"access"`
	RefreshToken string `json:"refresh,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	UserName     string `json:"user_name,omitempty"`
	Provider     string `json:"provider,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	SavedAt      string `json:"saved_at,omitempty"`
	// Proxy = proxy keluar untuk akun ini (kosong = tanpa proxy).
	Proxy string `json:"proxy,omitempty"`
	// Slot = nomor pasangan akun↔IP di ip-pool.json.
	Slot int `json:"slot,omitempty"`
}

// ErrNotFound berarti belum ada akun.
var ErrNotFound = fmt.Errorf("belum login (tidak ada kredensial tersimpan)")

// newDeviceID menghasilkan device_id acak untuk satu akun.
//
// Sengaja TIDAK memakai prefix hostname: prefix yang sama di semua akun
// (mis. "<hostname>-") adalah ciri korelasi yang mudah dibaca upstream dan
// membuat akun-akun tampak satu perangkat. Setiap akun dapat nilai sendiri.
func newDeviceID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Fallback: timestamp, tetap unik per akun.
		return fmt.Sprintf("ac-%d", time.Now().UnixNano())
	}
	return "ac-" + hex.EncodeToString(b)
}

// Save menyimpan atau memperbarui akun pertama (single-account).
// Untuk MENAMBAH akun baru tanpa menimpa, pakai Add.
func Save(c *Creds) error {
	existing, err := db.GetActiveAccount()
	if err != nil {
		return err
	}
	if c.DeviceID == "" {
		h, _ := os.Hostname()
		if h == "" {
			h = "unknown"
		}
		c.DeviceID = h + "-autoclawpi"
	}
	if existing != nil {
		// update existing
		existing.AccessToken = c.AccessToken
		existing.RefreshToken = c.RefreshToken
		existing.Provider = c.Provider
		existing.UserID = c.UserID
		existing.UserName = c.UserName
		existing.DeviceID = c.DeviceID
		existing.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
		return db.UpdateAccount(existing)
	}
	// create new
	_, err = db.AddAccount("default", c.AccessToken, c.RefreshToken, c.Provider, c.UserID, c.UserName, c.DeviceID)
	return err
}

// Add menyimpan kredensial sebagai akun BARU — tidak pernah menimpa akun lain.
// Dipakai oleh `login` dan `import` untuk menambah akun ke pool.
//
// name kosong → pakai UserName dari respons login; kalau itu juga kosong,
// pakai "acct-N" berdasarkan jumlah akun yang ada.
//
// Pemasangan IP: akun baru mengambil slot berikutnya, lalu proxy diambil dari
// ip-pool.json slot tersebut. Kalau pool kosong → tanpa proxy (perilaku lama).
// Kalau pool ada tapi slot melebihi jumlah IP → error ErrPoolExhausted supaya
// tidak ada dua akun berbagi IP tanpa disadari.
func Add(name string, c *Creds) (int64, error) {
	if c.DeviceID == "" {
		c.DeviceID = newDeviceID()
	}
	if name == "" {
		name = c.UserName
	}
	if name == "" {
		accounts, err := db.ListAccounts()
		if err != nil {
			return 0, err
		}
		name = fmt.Sprintf("acct-%d", len(accounts)+1)
	}

	// Pasangkan ke IP pool kalau ada.
	// Urutan prioritas: proxy eksplisit > slot eksplisit > slot bebas berikutnya.
	// Pool kosong → JANGAN isi slot (slot tanpa proxy itu menyesatkan:
	// akun tampak terpasang padahal keluar lewat IP default).
	if c.Proxy == "" {
		pool, err := ippool.Load()
		if err != nil {
			return 0, err
		}
		if pool.Count() > 0 {
			slot := c.Slot
			if slot <= 0 {
				s, err := db.NextFreeSlot()
				if err != nil {
					return 0, err
				}
				slot = s
			}
			proxy, err := pool.ForSlot(slot)
			if err != nil {
				return 0, fmt.Errorf("%w (pool: %v). Tambahkan IP ke %s atau pakai --proxy",
					err, pool.Names(), ippool.Path())
			}
			c.Proxy = proxy
			c.Slot = slot
		}
	}

	id, err := db.AddAccount(name, c.AccessToken, c.RefreshToken, c.Provider,
		c.UserID, c.UserName, c.DeviceID)
	if err != nil {
		return 0, err
	}
	if c.Proxy != "" || c.Slot > 0 {
		if err := db.SetAccountProxy(id, c.Proxy, c.Slot); err != nil {
			return id, err
		}
	}
	return id, nil
}

// Load membaca kredensial akun pertama.
func Load() (*Creds, error) {
	a, err := db.GetActiveAccount()
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, ErrNotFound
	}
	return &Creds{
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		UserID:       a.UserID,
		UserName:     a.UserName,
		Provider:     a.Provider,
		DeviceID:     a.DeviceID,
		SavedAt:      a.CreatedAt,
	}, nil
}

// Clear menghapus akun pertama.
func Clear() error {
	existing, err := db.GetActiveAccount()
	if err != nil {
		return err
	}
	if existing != nil {
		return db.DeleteAccount(existing.ID)
	}
	return nil
}
