package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/hirotomasato/autoclawpi/internal/client"
	"github.com/hirotomasato/autoclawpi/internal/db"
)

// checkinTasks = tugas HARIAN saja.
//
// Tugas newbie (newbie_cloud_lobster / newbie_local_lobster) TIDAK di sini:
// itu reward sekali seumur akun, jadi diklaim SEKALI saat akun ditambahkan
// (lihat claimNewbieRewards di main.go / finishLogin). Menaruhnya di sini
// membuat setiap checkin harian memanggil upstream untuk tugas yang sudah
// pasti "already" — request sia-sia yang menambah pola perilaku teratur
// (pola berulang adalah salah satu pemicu utama pemblokiran akun).
var checkinTasks = []struct {
	ID     string
	Points int
}{
	{"daily_signin", 400},
}

// newbieTasks = reward sekali seumur akun, diklaim sekali saat akun
// ditambahkan (lihat claimNewbieRewards di main.go).
var newbieTasks = []struct {
	ID     string
	Points int
}{
	{"newbie_cloud_lobster", 500},
	{"newbie_local_lobster", 500},
}

func cmdCheckin(args []string) error {
	fs := flag.NewFlagSet("checkin", flag.ExitOnError)
	accountID := fs.Int64("account", 0, "ID akun (0 = semua akun)")
	dryRun := fs.Bool("dry-run", false, "cek tanpa klaim")
	fs.Parse(args)

	accounts, err := db.ListAccounts()
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		return fmt.Errorf("tidak ada akun, login dulu")
	}

	_, cl := loadAll()
	today := time.Now().UTC().Format("2006-01-02")
	anyClaimed := false

	for _, a := range accounts {
		if !a.Active {
			continue
		}
		if *accountID > 0 && a.ID != *accountID {
			continue
		}

		fmt.Printf("\n=== Akun #%d (%s) ===\n", a.ID, a.Name)
		if a.AccessToken == "" {
			fmt.Println("  SKIP: token kosong")
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		balance, _ := cl.FetchBalance(ctx, a.AccessToken, a.Proxy)
		cancel()
		fmt.Printf("  Saldo: %d pts\n", balance)

		for _, task := range checkinTasks {
			if claimed, ok := claimOne(cl, a, today, task.ID, *dryRun); ok {
				anyClaimed = anyClaimed || claimed
			}
		}

		// daily_inspiration_center: endpoint beda (butuh inspiration_id).
		ctx, cancel = context.WithTimeout(context.Background(), 2*time.Minute)
		claimed, pts, note := claimInspiration(ctx, cl, a, today, *dryRun)
		cancel()
		fmt.Printf("  daily_inspiration_center: %s\n", note)
		anyClaimed = anyClaimed || claimed
		_ = pts

		if !*dryRun {
			ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
			newBalance, _ := cl.FetchBalance(ctx, a.AccessToken, a.Proxy)
			cancel()
			if newBalance > balance {
				db.UpdatePoints(a.ID, newBalance)
			}
			fmt.Printf("  Saldo akhir: %d pts\n", newBalance)
		}
	}

	if !anyClaimed && !*dryRun {
		fmt.Println("\nsemua task udah di-claim hari ini")
	}
	return nil
}

// claimOne menangani task biasa (lewat autoclaw-task-complete).
// Return (claimed, ok): ok=false kalau error sudah dilaporkan.
func claimOne(cl *client.Client, a db.Account, today, taskID string, dryRun bool) (bool, bool) {
	existing, _ := db.GetCheckinLog(a.ID, today, taskID)
	if existing != nil {
		fmt.Printf("  %s: sudah (%d pts)\n", taskID, existing.Points)
		return false, true
	}
	if dryRun {
		fmt.Printf("  %s: akan klaim\n", taskID)
		return false, true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	points, already, err := cl.ClaimTaskVia(ctx, a.AccessToken, taskID, a.Proxy)
	cancel()
	if err != nil {
		fmt.Printf("  %s: GAGAL — %v\n", taskID, err)
		db.AddCheckinLog(a.ID, today, taskID, 0, "failed:"+err.Error(), a.DeviceID)
		return false, false
	}
	if already {
		fmt.Printf("  %s: sudah diklaim\n", taskID)
		db.AddCheckinLog(a.ID, today, taskID, 0, "already", a.DeviceID)
		return false, true
	}
	fmt.Printf("  %s: ✅ %d pts\n", taskID, points)
	db.AddCheckinLog(a.ID, today, taskID, points, "success", a.DeviceID)
	return points > 0, true
}

// claimInspiration menangani daily_inspiration_center.
//
// Alurnya dua langkah (beda dari task lain):
//  1. POST /agentdr/v1/assistant/inspiration-center         → ambil inspiration_id
//  2. POST /agentdr/v1/assistant/inspiration-task-complete  → {"inspiration_id": ...}
//
// Endpoint autoclaw-task-complete TIDAK berlaku di sini: dulu dipakai dan
// hasilnya reward 0 + status task jadi "completed" palsu.
func claimInspiration(ctx context.Context, cl *client.Client, a db.Account, today string, dryRun bool) (bool, int, string) {
	const taskID = "daily_inspiration_center"

	if existing, _ := db.GetCheckinLog(a.ID, today, taskID); existing != nil {
		return false, 0, fmt.Sprintf("sudah (%d pts)", existing.Points)
	}

	items, err := cl.InspirationCenter(ctx, a.AccessToken, a.Proxy)
	if err != nil {
		return false, 0, "GAGAL ambil daftar — " + err.Error()
	}
	if len(items) == 0 {
		return false, 0, "tidak ada item inspirasi"
	}
	if dryRun {
		return false, 0, fmt.Sprintf("akan klaim %d pts (%s)", items[0].RewardPoints, items[0].InspirationID[:8])
	}

	already, err := cl.ClaimInspirationReward(ctx, a.AccessToken, items[0].InspirationID, a.Proxy)
	if err != nil {
		db.AddCheckinLog(a.ID, today, taskID, 0, "failed:"+err.Error(), a.DeviceID)
		return false, 0, "GAGAL — " + err.Error()
	}
	if already {
		db.AddCheckinLog(a.ID, today, taskID, 0, "already", a.DeviceID)
		return false, 0, "sudah diklaim"
	}
	pts := items[0].RewardPoints
	db.AddCheckinLog(a.ID, today, taskID, pts, "success", a.DeviceID)
	return pts > 0, pts, fmt.Sprintf("✅ %d pts", pts)
}
