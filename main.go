package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

const version = "0.9.7"

func main() {
	var (
		cfgPath = flag.String("config", "printmon.json", "путь к файлу настроек")
		dataDir = flag.String("data", "data", "каталог с базой истории")
		once    = flag.Bool("once", false, "опросить парк один раз и вывести JSON")
		seed    = flag.String("seed", "", "загрузить printers.csv (тестовое наполнение истории)")
		listen  = flag.String("listen", "", "переопределить адрес веб-морды")
		noPoll  = flag.Bool("nopoll", false, "не опрашивать сеть, показывать только историю")
		noOpen  = flag.Bool("noopen", false, "не открывать браузер при запуске")
		showVer = flag.Bool("version", false, "показать версию")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("printmon", version)
		return
	}

	log.SetFlags(log.LstdFlags)

	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("настройки: %v", err)
	}
	cfgStore := NewConfigStore(*cfgPath, cfg)
	if *listen != "" {
		cfg.Listen = *listen
		_, _ = cfgStore.Update(func(c *Config) { c.Listen = *listen })
	}
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("data: %v", err)
	}
	st, err := OpenStore(filepath.Join(*dataDir, "printmon.db"))
	if err != nil {
		log.Fatalf("база: %v", err)
	}
	defer st.Close()

	if *seed != "" {
		if err := st.SeedFromCSV(*seed); err != nil {
			log.Fatalf("seed: %v", err)
		}
		log.Printf("история наполнена из %s", *seed)
		return
	}

	state := NewState()
	shutdown := NewShutdown()

	if *once {
		devices := PollAll(cfgStore.Get())
		out, _ := json.MarshalIndent(devices, "", "  ")
		fmt.Println(string(out))
		return
	}

	if !*noPoll {
		Refresh(cfgStore.Get(), st, state)
		go func() {
			for {
				time.Sleep(time.Duration(cfgStore.Get().IntervalMinutes) * time.Minute)
				Refresh(cfgStore.Get(), st, state)
			}
		}()
	} else {
		LoadStateFromStore(cfgStore.Get(), st, state)
	}

	cur := cfgStore.Get()
	log.Printf("printmon %s: опрос каждые %d мин, пороги %g%% / %g%%",
		version, cur.IntervalMinutes, cur.WarnPercent, cur.CritPercent)
	// Останавливаем опрос по той же кнопке, что и веб-морду.
	go func() {
		<-shutdown.Done()
		log.Printf("получена команда остановки, завершаю работу")
	}()
	if err := Serve(cfgStore, st, state, cur.OpenBrowser && !*noOpen, shutdown); err != nil {
		log.Fatalf("%v", err)
	}
	log.Printf("printmon остановлен")
}

// LoadStateFromStore fills the shared state straight from the database,
// without touching the network.
func LoadStateFromStore(cfg Config, st *Store, state *State) {
	devices, _ := st.Devices()
	lastSeen, latest, _ := st.LastSeen()
	applyLatest(devices, latest)
	readings, _ := st.LoadWindow(cfg.HistoryDays)
	state.Set(devices, readings, lastSeen)
}

// Refresh polls the fleet, stores the result and updates the shared state.
func Refresh(cfg Config, st *Store, state *State) {
	start := time.Now()
	fresh := PollAll(cfg)
	now := time.Now()

	byIP := map[string]Device{}
	for _, d := range fresh {
		byIP[d.IP] = d
		if err := st.Save(d, now); err != nil {
			log.Printf("сохранение %s: %v", d.IP, err)
		}
	}

	// Inventory (including devices that did not answer this time).
	inventory, err := st.Devices()
	if err != nil {
		log.Printf("инвентарь: %v", err)
	}
	lastSeen, latest, err := st.LastSeen()
	if err != nil {
		log.Printf("last seen: %v", err)
	}
	applyLatest(inventory, latest)

	for i := range inventory {
		if d, ok := byIP[inventory[i].IP]; ok {
			g := inventory[i].Group
			inventory[i] = d
			inventory[i].Group = g
		}
	}
	for ip, d := range byIP {
		found := false
		for _, x := range inventory {
			if x.IP == ip {
				found = true
				break
			}
		}
		if !found {
			inventory = append(inventory, d)
		}
	}
	sortByIP(inventory)

	readings, err := st.LoadWindow(cfg.HistoryDays)
	if err != nil {
		log.Printf("история: %v", err)
	}
	state.Set(inventory, readings, lastSeen)
	log.Printf("опрос завершён за %s: ответили %d устройств, всего в парке %d",
		time.Since(start).Round(time.Millisecond), len(fresh), len(inventory))
}

// applyLatest fills supplies/pages/status for devices with no fresh answer.
func applyLatest(devices []Device, latest []Reading) {
	for _, r := range latest {
		for i := range devices {
			if devices[i].IP == r.IP {
				if len(devices[i].Supplies) == 0 {
					devices[i].Supplies = r.Supplies
				}
				if devices[i].Pages == 0 {
					devices[i].Pages = r.Pages
				}
				if devices[i].Status == "" {
					devices[i].Status = r.Status
				}
			}
		}
	}
}
