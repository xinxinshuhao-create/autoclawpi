package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/hirotomasato/autoclawpi/internal/db"
	"github.com/hirotomasato/autoclawpi/internal/ippool"
	"github.com/hirotomasato/autoclawpi/internal/store"
)

func cmdAccount(args []string) error {
	if len(args) == 0 {
		return listAccounts()
	}
	switch args[0] {
	case "list", "ls":
		return listAccounts()
	case "add":
		return addAccount(args[1:])
	case "remove", "rm", "delete":
		return removeAccount(args[1:])
	case "show":
		return showAccount(args[1:])
	default:
		return fmt.Errorf("subcommand: list | add | remove | show")
	}
}

func listAccounts() error {
	accounts, err := db.ListAccounts()
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		fmt.Println("tidak ada akun. jalankan 'autoclawpi login' atau 'autoclawpi import'")
		return nil
	}
	fmt.Printf("%-4s %-20s %-8s %-12s %-8s %-18s %-22s %s\n", "ID", "Name", "Provider", "User", "Points", "Status", "IP (slot)", "Last Used")
	now := time.Now()
	pool, _ := ippool.Load()
	for _, a := range accounts {
		status := "✓"
		switch {
		case !a.Active:
			status = "✗ manual-off"
		case a.IsCoolingDown(now):
			status = "⏸ " + a.CooldownReason + " " + shortTime(a.CooldownUntil)
		}
		ipinfo := "-"
		if a.Slot > 0 {
			name := pool.NameForSlot(a.Slot)
			if name == "" {
				name = a.Proxy
			}
			ipinfo = fmt.Sprintf("%s (slot %d)", truncate(name, 14), a.Slot)
		} else if a.Proxy != "" {
			ipinfo = truncate(a.Proxy, 20)
		}
		fmt.Printf("%-4d %-20s %-8s %-12s %-8d %-18s %-22s %s\n", a.ID, truncate(a.Name, 18), a.Provider, truncate(a.UserName, 10), a.Points, status, ipinfo, truncate(a.LastUsedAt, 16))
	}
	return nil
}

// shortTime memformat RFC3339 jadi HH:MM tanpa risiko panic kalau pendek.
func shortTime(rfc3339 string) string {
	if t, err := time.Parse(time.RFC3339, rfc3339); err == nil {
		return t.Local().Format("15:04")
	}
	return rfc3339
}

func addAccount(args []string) error {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	name := fs.String("name", "", "nama akun")
	access := fs.String("access", "", "access token")
	refresh := fs.String("refresh", "", "refresh token (opsional)")
	provider := fs.String("provider", "zai", "zai | google")
	fs.Parse(args)

	if *access == "" {
		fmt.Fprintf(os.Stderr, "usage: autoclawpi account add --access <token> [--refresh <token>] [--name <name>] [--provider zai|google]\n")
		os.Exit(2)
	}
	c := &store.Creds{
		AccessToken:  *access,
		RefreshToken: *refresh,
		Provider:     *provider,
		SavedAt:      time.Now().Format(time.RFC3339),
	}
	// Pakai store.Add supaya device_id acak per akun (bukan prefix hostname
	// yang sama untuk semua akun).
	id, err := store.Add(*name, c)
	if err != nil {
		return err
	}
	fmt.Printf("akun #%d ditambahkan\n", id)
	return nil
}

func removeAccount(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: autoclawpi account remove <id>")
	}
	var id int64
	if _, err := fmt.Sscanf(args[0], "%d", &id); err != nil {
		return fmt.Errorf("id harus angka")
	}
	if err := db.DeleteAccount(id); err != nil {
		return err
	}
	fmt.Printf("akun #%d dihapus\n", id)
	return nil
}

func showAccount(args []string) error {
	var id int64
	if len(args) > 0 {
		fmt.Sscanf(args[0], "%d", &id)
	}
	var a *db.Account
	var err error
	if id > 0 {
		a, err = db.GetAccount(id)
	} else {
		a, err = db.GetActiveAccount()
	}
	if err != nil {
		return err
	}
	if a == nil {
		return fmt.Errorf("akun tidak ditemukan")
	}

	// fetch saldo real-time dari server (lewat proxy akun)
	_, cl := loadAll()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	balance, _ := cl.FetchBalance(ctx, a.AccessToken, a.Proxy)
	cancel()
	if balance > 0 {
		db.UpdatePoints(a.ID, balance) // update total
		a.Points = balance             // refresh local struct
	}

	fmt.Printf("ID        : %d\n", a.ID)
	fmt.Printf("Name      : %s\n", a.Name)
	fmt.Printf("Provider  : %s\n", a.Provider)
	fmt.Printf("UserID    : %s\n", a.UserID)
	fmt.Printf("UserName  : %s\n", a.UserName)
	fmt.Printf("DeviceID  : %s\n", a.DeviceID)
	fmt.Printf("Points    : %d pts (server: %d)\n", a.Points, balance)
	fmt.Printf("Active    : %v\n", a.Active)
	fmt.Printf("Created   : %s\n", a.CreatedAt)
	fmt.Printf("LastUsed  : %s\n", a.LastUsedAt)
	fmt.Printf("Access    : %s... (len=%d)\n", redactToken(a.AccessToken), len(a.AccessToken))
	fmt.Printf("Refresh   : %s... (len=%d)\n", redactToken(a.RefreshToken), len(a.RefreshToken))
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func redactToken(s string) string {
	if s == "" {
		return "(kosong)"
	}
	if len(s) > 20 {
		return s[:20]
	}
	return s
}
