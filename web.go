package main

import (
	_ "embed"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var indexHTML []byte

// State is the in-memory snapshot shared between the poller and the web UI.
type State struct {
	mu       sync.RWMutex
	devices  []Device
	readings []Reading
	lastSeen map[string]time.Time
}

// NewState creates an empty state.
func NewState() *State {
	return &State{lastSeen: map[string]time.Time{}}
}

// Set replaces the snapshot.
func (s *State) Set(devices []Device, readings []Reading, lastSeen map[string]time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devices = devices
	s.readings = readings
	s.lastSeen = lastSeen
}

// View renders the dashboard payload from the current snapshot.
func (s *State) View(cfg Config) View {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return BuildView(cfg, s.devices, s.readings, s.lastSeen)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func inAnySubnet(ip string, prefixes []string) bool {
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(ip, p) {
			return true
		}
	}
	return false
}

func addUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// Serve runs the built-in web dashboard. When openOnStart is true the system
// default browser is opened on the dashboard right after the port is bound.
// Closing a browser tab never stops the program: stopping is an explicit action
// (the "Остановить программу" button, /api/quit), so monitoring cannot be
// switched off by accidentally closing a window.
func Serve(cfgStore *ConfigStore, st *Store, state *State, openOnStart bool, shutdown *Shutdown) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})

	mux.HandleFunc("/api/data", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, state.View(cfgStore.Get()))
	})

	mux.HandleFunc("/api/refresh", func(w http.ResponseWriter, r *http.Request) {
		go Refresh(cfgStore.Get(), st, state)
		writeJSON(w, map[string]bool{"started": true})
	})

	// Остановка программы из морды. Ответ отдаём до остановки, иначе браузер
	// покажет ошибку вместо подтверждения.
	mux.HandleFunc("/api/quit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "только POST", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]bool{"stopping": true})
		go func() {
			time.Sleep(400 * time.Millisecond)
			shutdown.Request()
		}()
	})

	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		c := cfgStore.Get()
		writeJSON(w, map[string]any{
			"subnets":         c.AllSubnets(),
			"hosts":           c.Hosts,
			"exclude":         c.Exclude,
			"locations":       c.Locations,
			"warnPercent":     c.WarnPercent,
			"critPercent":     c.CritPercent,
			"intervalMinutes": c.IntervalMinutes,
			"configPath":      cfgStore.Path(),
			"needsSetup":      c.NeedsSetup(),
			"localSubnets":    hostSubnets(),
		})
	})

	// Мастер первого запуска: какие подсети опрашивать.
	mux.HandleFunc("/api/setup", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "только POST", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Action  string   `json:"action"`
			Subnets []string `json:"subnets"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "не разобрать запрос: "+err.Error(), http.StatusBadRequest)
			return
		}
		switch req.Action {
		case "scan", "manual", "skip":
		default:
			http.Error(w, "неизвестное действие: "+req.Action, http.StatusBadRequest)
			return
		}
		polling := false
		_, err := cfgStore.Update(func(c *Config) {
			switch req.Action {
			case "scan", "manual":
				list := []string{}
				for _, s := range req.Subnets {
					if p := NormalizePrefix(s); p != "" {
						list = addUnique(list, p)
					}
				}
				if len(list) == 0 {
					list = hostPrefixes() // «просканировать мои подсети» без явного списка
				}
				if len(list) > 0 {
					c.Subnets = list
					polling = true
				}
				c.SetupDone = true
			case "skip":
				c.SetupDone = true
			}
		})
		if polling {
			go Refresh(cfgStore.Get(), st, state)
		}
		writeJSON(w, map[string]any{
			"ok":      err == nil,
			"error":   errString(err),
			"subnets": cfgStore.Get().AllSubnets(),
			"polling": polling,
		})
	})

	// Добавление принтера из интерфейса: адрес, кабинет и (необязательно) подсеть.
	mux.HandleFunc("/api/printer", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "только POST", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			IP       string `json:"ip"`
			Location string `json:"location"`
			Subnet   string `json:"subnet"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "не разобрать запрос: "+err.Error(), http.StatusBadRequest)
			return
		}
		ip := strings.TrimSpace(req.IP)
		if net.ParseIP(ip) == nil || !strings.Contains(ip, ".") {
			http.Error(w, "не похоже на IP-адрес: "+ip, http.StatusBadRequest)
			return
		}
		addedHost, addedSubnet := false, false
		_, err := cfgStore.Update(func(c *Config) {
			if c.Locations == nil {
				c.Locations = map[string]string{}
			}
			loc := strings.TrimSpace(req.Location)
			if loc == "" {
				delete(c.Locations, ip)
			} else {
				c.Locations[ip] = loc
			}
			if !inAnySubnet(ip, c.AllSubnets()) {
				c.Hosts = addUnique(c.Hosts, ip)
			}
			// Явно указанная подсеть начинает опрашиваться целиком (1..254).
			if s := NormalizePrefix(req.Subnet); s != "" {
				before := len(c.Subnets)
				if len(c.Subnets) == 0 {
					c.Subnets = append([]string{}, c.AllSubnets()...)
				}
				c.Subnets = addUnique(c.Subnets, s)
				addedSubnet = len(c.Subnets) != before
			}
			// Адрес внутри опрашиваемой подсети не нужно дублировать в hosts.
			if inAnySubnet(ip, c.AllSubnets()) {
				c.Hosts = removeValue(c.Hosts, ip)
			}
			for _, h := range c.Hosts {
				if h == ip {
					addedHost = true
				}
			}
		})
		if addedHost || addedSubnet {
			go Refresh(cfgStore.Get(), st, state)
		}
		writeJSON(w, map[string]any{
			"ok":          err == nil,
			"error":       errString(err),
			"addedHost":   addedHost,
			"addedSubnet": addedSubnet,
			"polling":     addedHost || addedSubnet,
		})
	})

	// Удаление принтера из мониторинга: забыть устройство и больше его не опрашивать.
	mux.HandleFunc("/api/printer/remove", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "только POST", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			IP string `json:"ip"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "не разобрать запрос: "+err.Error(), http.StatusBadRequest)
			return
		}
		ip := strings.TrimSpace(req.IP)
		if net.ParseIP(ip) == nil {
			http.Error(w, "не похоже на IP-адрес: "+ip, http.StatusBadRequest)
			return
		}
		delErr := st.DeleteDevice(ip)
		excluded := false
		_, cfgErr := cfgStore.Update(func(c *Config) {
			delete(c.Locations, ip)
			c.Hosts = removeValue(c.Hosts, ip)
			// Адрес внутри опрашиваемой подсети вернётся при следующем скане,
			// поэтому его надо явно исключить из опроса.
			if inAnySubnet(ip, c.AllSubnets()) {
				c.Exclude = addUnique(c.Exclude, ip)
				excluded = true
			}
		})
		LoadStateFromStore(cfgStore.Get(), st, state)
		err := delErr
		if err == nil {
			err = cfgErr
		}
		writeJSON(w, map[string]any{"ok": err == nil, "error": errString(err), "excluded": excluded})
	})

	// Кабинет из интерфейса (клик по ячейке).
	mux.HandleFunc("/api/locations", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "только POST", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			IP       string `json:"ip"`
			Location string `json:"location"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "не разобрать запрос: "+err.Error(), http.StatusBadRequest)
			return
		}
		ip := strings.TrimSpace(req.IP)
		if net.ParseIP(ip) == nil {
			http.Error(w, "не похоже на IP-адрес: "+ip, http.StatusBadRequest)
			return
		}
		_, err := cfgStore.Update(func(c *Config) {
			if c.Locations == nil {
				c.Locations = map[string]string{}
			}
			loc := strings.TrimSpace(req.Location)
			if loc == "" {
				delete(c.Locations, ip)
			} else {
				c.Locations[ip] = loc
			}
		})
		writeJSON(w, map[string]any{"ok": err == nil, "error": errString(err)})
	})

	// Пакетно: выгрузка таблицы кабинетов и загрузка заполненной обратно.
	mux.HandleFunc("/api/locations.csv", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			exportLocationsCSV(w, cfgStore.Get(), state)
		case http.MethodPost:
			importLocationsCSV(w, r, cfgStore, st, state)
		default:
			http.Error(w, "только GET или POST", http.StatusMethodNotAllowed)
		}
	})

	cfg := cfgStore.Get()
	// Сначала занимаем порт и только потом открываем браузер: иначе при занятом
	// порте пользователь получил бы пустую вкладку и решил, что всё сломалось.
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("не удалось открыть веб-морду на %s: %v\n"+
			"проверь поле \"listen\" в printmon.json — вероятно, порт занят другой программой\n"+
			"(или запусти с ключом -listen 127.0.0.1:49321)", cfg.Listen, err)
	}
	url := browserURL(cfg.Listen)
	log.Printf("dashboard: %s", url)
	if openOnStart {
		if err := openBrowser(url); err != nil {
			log.Printf("браузер не открылся (%v) — открой вручную: %s", err, url)
		}
	}

	srv := &http.Server{Handler: mux}
	go func() {
		<-shutdown.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil // штатная остановка по кнопке
	}
	return err
}

func errString(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}

func removeValue(list []string, v string) []string {
	out := list[:0]
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// exportLocationsCSV hands out a spreadsheet the user can fill in Excel.
func exportLocationsCSV(w http.ResponseWriter, cfg Config, state *State) {
	view := state.View(cfg)
	rows := [][]string{{"ip", "кабинет", "модель", "имя"}}
	seen := map[string]bool{}
	for _, d := range view.Devices {
		rows = append(rows, []string{d.IP, d.Location, d.Model, d.Name})
		seen[d.IP] = true
	}
	var extra []string
	for ip := range cfg.Locations {
		if !seen[ip] {
			extra = append(extra, ip)
			seen[ip] = true
		}
	}
	for _, ip := range cfg.Hosts {
		if !seen[ip] {
			extra = append(extra, ip)
			seen[ip] = true
		}
	}
	sort.Slice(extra, func(i, j int) bool { return ipLess(extra[i], extra[j]) })
	for _, ip := range extra {
		rows = append(rows, []string{ip, cfg.Locations[ip], "", "из настроек"})
	}

	var b strings.Builder
	b.WriteString("\ufeff") // BOM, иначе Excel съест кириллицу
	cw := csv.NewWriter(&b)
	cw.Comma = ';'
	_ = cw.WriteAll(rows)
	cw.Flush()

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="printmon-cabinets.csv"`)
	_, _ = io.WriteString(w, b.String())
}

// importLocationsCSV reads the filled spreadsheet back and saves the cabinets.
func importLocationsCSV(w http.ResponseWriter, r *http.Request, cfgStore *ConfigStore, st *Store, state *State) {
	data, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, "не прочитать файл: "+err.Error(), http.StatusBadRequest)
		return
	}
	updates, err := parseLocationCSV(string(data))
	if err != nil {
		http.Error(w, "не разобрать таблицу: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(updates) == 0 {
		http.Error(w, "в таблице не нашлось ни одной строки с IP-адресом (первая колонка)", http.StatusBadRequest)
		return
	}

	added := 0
	_, err = cfgStore.Update(func(c *Config) {
		if c.Locations == nil {
			c.Locations = map[string]string{}
		}
		excluded := map[string]bool{}
		for _, e := range c.Exclude {
			excluded[e] = true
		}
		for ip, loc := range updates {
			if loc == "" || excluded[ip] {
				continue // пустой кабинет ничего не меняет, исключённые адреса не трогаем
			}
			c.Locations[ip] = loc
			if !inAnySubnet(ip, c.AllSubnets()) {
				before := len(c.Hosts)
				c.Hosts = addUnique(c.Hosts, ip)
				if len(c.Hosts) != before {
					added++
				}
			}
		}
	})
	if added > 0 {
		go Refresh(cfgStore.Get(), st, state)
	}
	writeJSON(w, map[string]any{
		"ok":       err == nil,
		"error":    errString(err),
		"rows":     len(updates),
		"newHosts": added,
		"polling":  added > 0,
	})
}

// parseLocationCSV understands both the file we export and a hand-made table.
// Первая колонка — IP, вторая — кабинет. Разделитель: ; , или табуляция.
func parseLocationCSV(text string) (map[string]string, error) {
	text = strings.TrimPrefix(text, "\ufeff")
	comma := ';'
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch {
		case strings.Contains(line, ";"):
			comma = ';'
		case strings.Contains(line, "\t"):
			comma = '\t'
		case strings.Contains(line, ","):
			comma = ','
		}
		break
	}
	rd := csv.NewReader(strings.NewReader(text))
	rd.Comma = comma
	rd.FieldsPerRecord = -1
	rd.LazyQuotes = true

	out := map[string]string{}
	for {
		rec, err := rd.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			if len(out) > 0 {
				break
			}
			return out, err
		}
		if len(rec) == 0 {
			continue
		}
		ip := strings.TrimSpace(rec[0])
		if ip == "" || net.ParseIP(ip) == nil {
			continue // заголовок или мусорная строка
		}
		loc := ""
		if len(rec) > 1 {
			loc = strings.TrimSpace(rec[1])
		}
		out[ip] = loc
	}
	return out, nil
}
