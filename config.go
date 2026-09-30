package main

import (
	"encoding/json"
	"os"
	"strings"
)

// TelegramConfig holds optional alert delivery settings.
type TelegramConfig struct {
	Enabled bool   `json:"enabled"`
	Token   string `json:"token"`
	ChatID  string `json:"chatId"`
}

// Config is the on-disk configuration (printmon.json).
type Config struct {
	Subnet          string            `json:"subnet"`
	Subnets         []string          `json:"subnets,omitempty"`
	Hosts           []string          `json:"hosts"`
	Exclude         []string          `json:"exclude"`
	Community       string            `json:"community"`
	TimeoutMs       int               `json:"timeoutMs"`
	Retries         int               `json:"retries"`
	Concurrency     int               `json:"concurrency"`
	IntervalMinutes int               `json:"intervalMinutes"`
	Listen          string            `json:"listen"`
	WarnPercent     float64           `json:"warnPercent"`
	CritPercent     float64           `json:"critPercent"`
	HistoryDays     int               `json:"historyDays"`
	OpenBrowser     bool              `json:"openBrowser"`
	SetupDone       bool              `json:"setupDone"`
	Groups          map[string]string `json:"groups"`
	Locations       map[string]string `json:"locations"`
	Telegram        TelegramConfig    `json:"telegram"`
}

// Listen по умолчанию: порт вне стандартных занятых (80, 443, 8080, 8443, 9090),
// взят из верхнего диапазона — там почти нет служб, поэтому конфликт маловероятен.
const DefaultListen = "127.0.0.1:49321"

// oldListens — порты из прошлых версий; их надо уводить на новый автоматически,
// иначе обновлённая программа у владельца осталась бы на 8080 и могла не подняться.
var oldListens = map[string]bool{
	"": true, "127.0.0.1:8080": true, "localhost:8080": true,
	"0.0.0.0:8080": true, ":8080": true,
}

// DefaultConfig returns sane defaults for the first run. Подсеть намеренно
// не прописана: адреса выбирает владелец на первом запуске, а не автор программы.
func DefaultConfig() Config {
	return Config{
		Subnet:          "",
		Community:       "public",
		TimeoutMs:       1500,
		Retries:         1,
		Concurrency:     8,
		IntervalMinutes: 15,
		Listen:          DefaultListen,
		WarnPercent:     25,
		CritPercent:     10,
		HistoryDays:     14,
		OpenBrowser:     true,
		Groups:          map[string]string{},
		Locations:       map[string]string{},
	}
}

// AllSubnets returns every prefix the poller must scan. `subnets` is the new
// list; the single `subnet` field stays supported for older configs.
func (c Config) AllSubnets() []string {
	out := make([]string, 0, len(c.Subnets)+1)
	for _, s := range c.Subnets {
		if s = NormalizePrefix(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		if s := NormalizePrefix(c.Subnet); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// NormalizePrefix keeps subnet prefixes in the "192.168.1." form.
func NormalizePrefix(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// NeedsSetup tells whether the dashboard should ask about the subnet.
func (c Config) NeedsSetup() bool {
	return !c.SetupDone && len(c.AllSubnets()) == 0 && len(c.Hosts) == 0
}

// LoadConfig reads the config file, creating it with defaults when missing.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			out, _ := json.MarshalIndent(cfg, "", "  ")
			_ = os.WriteFile(path, out, 0o644)
			return cfg, nil
		}
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	// Поле openBrowser добавлялось позже остальных: его отсутствие в старом файле
	// значит «как раньше принято» — открывать браузер, а не выключать его.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err == nil {
		if _, ok := probe["openBrowser"]; !ok {
			cfg.OpenBrowser = true
		}
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 8
	}
	if cfg.IntervalMinutes <= 0 {
		cfg.IntervalMinutes = 15
	}
	if cfg.HistoryDays <= 0 {
		cfg.HistoryDays = 14
	}
	if cfg.Hosts == nil {
		cfg.Hosts = []string{}
	}
	if cfg.Groups == nil {
		cfg.Groups = map[string]string{}
	}
	if cfg.Locations == nil {
		cfg.Locations = map[string]string{}
	}
	// Поле setupDone появилось позже: если адреса уже заданы, значит владелец
	// уже настроил программу — спрашивать заново не нужно.
	if len(cfg.AllSubnets()) > 0 || len(cfg.Hosts) > 0 {
		cfg.SetupDone = true
	}
	// Переезд со старого порта: конфиг владельца может хранить 8080,
	// на котором часто сидит что-то чужое.
	if oldListens[strings.ToLower(strings.TrimSpace(cfg.Listen))] {
		cfg.Listen = DefaultListen
		if err := SaveConfig(path, cfg); err != nil {
			return cfg, nil // не мешаем запуску из-за неудавшейся записи
		}
	}
	return cfg, nil
}

// SaveConfig writes the config back, keeping the human-readable layout.
func SaveConfig(path string, cfg Config) error {
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
