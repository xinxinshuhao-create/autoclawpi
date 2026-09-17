package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/hirotomasato/autoclawpi/internal/db"
	"github.com/hirotomasato/autoclawpi/internal/ippool"
)

// cmdIPPool mengelola/memeriksa registri IP (ip-pool.json).
//
//	autoclawpi ippool           tampilkan pool + pemasangan akun
//	autoclawpi ippool check     validasi pool (duplikat, format URL)
//	autoclawpi ippool assign    pasangkan akun lama yang belum punya slot
func cmdIPPool(args []string) error {
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "list", "ls", "":
		return ippoolList()
	case "check":
		return ippoolCheck()
	case "assign":
		return ippoolAssign()
	default:
		return fmt.Errorf("subcommand: list | check | assign")
	}
}

func ippoolList() error {
	pool, err := ippool.Load()
	if err != nil {
		return err
	}
	fmt.Printf("ip-pool: %s\n", ippool.Path())
	fmt.Printf("total IP: %d\n\n", pool.Count())

	if pool.Count() == 0 {
		fmt.Println("(pool kosong — semua akun pakai koneksi default / tanpa proxy)")
		fmt.Println("Buat file dengan format:")
		fmt.Println(`  {"proxies":[{"name":"taiwan1","url":"http://127.0.0.1:7901"}]}`)
		return nil
	}

	accounts, err := db.ListAccounts()
	if err != nil {
		return err
	}
	// Peta slot -> akun
	bySlot := make(map[int][]string, len(accounts))
	unassigned := 0
	for _, a := range accounts {
		if a.Slot > 0 {
			bySlot[a.Slot] = append(bySlot[a.Slot], a.Name)
		} else {
			unassigned++
		}
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SLOT\tIP\tURL\tAKUN")
	for i, e := range pool.Proxies {
		slot := i + 1
		names := "-"
		if v := bySlot[slot]; len(v) > 0 {
			names = ""
			for j, n := range v {
				if j > 0 {
					names += ", "
				}
				names += n
			}
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", slot, e.Name, e.URL, names)
	}
	w.Flush()

	if unassigned > 0 {
		fmt.Printf("\n⚠️  %d akun belum punya slot (pakai --proxy manual atau jalankan 'ippool assign')\n", unassigned)
	}
	return nil
}

func ippoolCheck() error {
	pool, err := ippool.Load()
	if err != nil {
		return err
	}
	if pool.Count() == 0 {
		fmt.Println("pool kosong — tidak ada yang perlu divalidasi")
		return nil
	}
	if err := pool.Validate(); err != nil {
		return fmt.Errorf("pool tidak valid: %w", err)
	}
	fmt.Printf("✅ pool valid: %d IP, tidak ada duplikat, format URL benar\n", pool.Count())

	// Bandingkan jumlah IP dengan jumlah akun
	accounts, err := db.ListAccounts()
	if err != nil {
		return err
	}
	fmt.Printf("   akun: %d\n", len(accounts))
	if len(accounts) > pool.Count() {
		fmt.Printf("   ⚠️  akun lebih banyak dari IP (%d > %d) — akun ke-%d+ tidak bisa dipasangkan\n",
			len(accounts), pool.Count(), pool.Count()+1)
	}
	return nil
}

// ippoolAssign memasangkan akun lama (slot=0) ke IP yang belum terpakai.
func ippoolAssign() error {
	pool, err := ippool.Load()
	if err != nil {
		return err
	}
	if pool.Count() == 0 {
		return fmt.Errorf("pool kosong, tidak ada IP untuk dipasangkan")
	}
	accounts, err := db.ListAccounts()
	if err != nil {
		return err
	}
	used := make(map[int]bool, len(accounts))
	for _, a := range accounts {
		if a.Slot > 0 {
			used[a.Slot] = true
		}
	}
	next := 1
	assigned := 0
	for _, a := range accounts {
		if a.Slot > 0 {
			continue
		}
		for used[next] {
			next++
		}
		if next > pool.Count() {
			fmt.Printf("⚠️  IP habis, akun #%d (%s) dilewati\n", a.ID, a.Name)
			continue
		}
		url, err := pool.ForSlot(next)
		if err != nil {
			return err
		}
		if err := db.SetAccountProxy(a.ID, url, next); err != nil {
			return err
		}
		fmt.Printf("akun #%d (%s) → slot %d (%s)\n", a.ID, a.Name, next, pool.NameForSlot(next))
		used[next] = true
		assigned++
	}
	if assigned == 0 {
		fmt.Println("semua akun sudah punya slot")
	} else {
		fmt.Printf("\n%d akun dipasangkan\n", assigned)
	}
	return nil
}
