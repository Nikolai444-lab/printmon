package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gosnmp/gosnmp"
)

// Standard Printer MIB + a couple of vendor OIDs we verified on the real fleet.
const (
	oidSysDescr    = "1.3.6.1.2.1.1.1.0"
	oidSysName     = "1.3.6.1.2.1.1.5.0"
	oidSysLocation = "1.3.6.1.2.1.1.6.0"
	oidHrDevDescr  = "1.3.6.1.2.1.25.3.2.1.3.1"
	oidPrtName     = "1.3.6.1.2.1.43.5.1.1.16.1"
	oidPrtSerial   = "1.3.6.1.2.1.43.5.1.1.17.1"
	oidPrtPages    = "1.3.6.1.2.1.43.10.2.1.4.1.1"
	oidHpDevID     = "1.3.6.1.4.1.11.2.3.9.1.1.7.0"
	oidHpStatus    = "1.3.6.1.4.1.11.2.3.9.1.1.3.0"
	oidSupplies    = "1.3.6.1.2.1.43.11.1.1"
)

var (
	rePID = regexp.MustCompile(`PID:([^,]+)`)
	reMDL = regexp.MustCompile(`MDL:([^;]+)`)
	reSN  = regexp.MustCompile(`SN:([^,]+)`)
)

// Supply is one consumable of a printer (toner, drum, waste box...).
type Supply struct {
	Index int      `json:"index"`
	Name  string   `json:"name"`
	Unit  int      `json:"unit"`
	Level int      `json:"level"`
	Max   int      `json:"max"`
	Pct   *float64 `json:"pct"`
	Note  string   `json:"note,omitempty"`
}

// Device is the state of one network device as seen by SNMP.
type Device struct {
	IP       string   `json:"ip"`
	Name     string   `json:"name"`
	Model    string   `json:"model"`
	Vendor   string   `json:"vendor"`
	Serial   string   `json:"serial"`
	Kind     string   `json:"kind"`
	Location string   `json:"location"`
	Status   string   `json:"status"`
	Pages    int64    `json:"pages"`
	Supplies []Supply `json:"supplies"`
	Group    string   `json:"group,omitempty"`
	Alive    bool     `json:"alive"`
	Error    string   `json:"error,omitempty"`
}

func newClient(ip string, cfg Config, timeoutMs int, retries int) *gosnmp.GoSNMP {
	return &gosnmp.GoSNMP{
		Target:    ip,
		Port:      161,
		Community: cfg.Community,
		Version:   gosnmp.Version2c,
		Timeout:   time.Duration(timeoutMs) * time.Millisecond,
		Retries:   retries,
		MaxOids:   gosnmp.MaxOids,
	}
}

func pduString(p gosnmp.SnmpPDU) string {
	switch v := p.Value.(type) {
	case nil:
		return ""
	case string:
		return cleanText(v)
	case []byte:
		if utf8.Valid(v) {
			return cleanText(string(v))
		}
		return hex.EncodeToString(v)
	default:
		return cleanText(fmt.Sprintf("%v", v))
	}
}

// cleanText trims quotes/space and drops net-snmp style "No Such Object" noise.
func cleanText(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"")
	if strings.HasPrefix(s, "No Such Object") {
		return ""
	}
	return s
}

func pduInt(p gosnmp.SnmpPDU) (int64, bool) {
	switch v := p.Value.(type) {
	case int:
		return int64(v), true
	case int32:
		return int64(v), true
	case int64:
		return v, true
	case uint:
		return int64(v), true
	case uint32:
		return int64(v), true
	case uint64:
		return int64(v), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n, err == nil
	}
	return 0, false
}

// PollAll walks the configured address space and returns live devices.
func PollAll(cfg Config) []Device {
	// Подсети (может быть несколько) + адреса, добавленные вручную из интерфейса.
	ips := make([]string, 0, 512)
	seen := map[string]bool{}
	add := func(ip string) {
		ip = strings.TrimSpace(ip)
		if ip == "" || seen[ip] {
			return
		}
		seen[ip] = true
		ips = append(ips, ip)
	}
	for _, prefix := range cfg.AllSubnets() {
		for i := 1; i <= 254; i++ {
			add(prefix + strconv.Itoa(i))
		}
	}
	for _, ip := range cfg.Hosts {
		add(ip)
	}
	excluded := map[string]bool{}
	for _, e := range cfg.Exclude {
		excluded[e] = true
	}

	results := make([]Device, 0, len(ips))
	var mu sync.Mutex
	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup

	for _, ip := range ips {
		if excluded[ip] {
			continue
		}
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			d := pollOne(ip, cfg)
			if !d.Alive {
				return
			}
			d.Group = cfg.Groups[ip]
			// Кабинет из настроек главнее того, что отдаёт само устройство:
			// большинство HP не хранят в sysLocation ничего осмысленного.
			if loc := cfg.Locations[ip]; loc != "" {
				d.Location = loc
			}
			mu.Lock()
			results = append(results, d)
			mu.Unlock()
		}(ip)
	}
	wg.Wait()

	// stable order by IP tail
	sortByIP(results)
	return results
}

func sortByIP(ds []Device) {
	for i := 1; i < len(ds); i++ {
		for j := i; j > 0 && ipLess(ds[j].IP, ds[j-1].IP); j-- {
			ds[j], ds[j-1] = ds[j-1], ds[j]
		}
	}
}

func ipLess(a, b string) bool {
	ai, _ := strconv.Atoi(lastPart(a))
	bi, _ := strconv.Atoi(lastPart(b))
	if ai != bi {
		return ai < bi
	}
	return a < b
}

func lastPart(ip string) string {
	parts := strings.Split(ip, ".")
	if len(parts) == 0 {
		return "0"
	}
	return parts[len(parts)-1]
}

// pollOne probes a single address; Alive=false means "not an SNMP device".
func pollOne(ip string, cfg Config) Device {
	d := Device{IP: ip}

	// Fast probe: one short GET. Keeps a subnet sweep quick.
	probe := newClient(ip, cfg, 700, 0)
	if err := probe.Connect(); err != nil {
		d.Error = err.Error()
		return d
	}
	pkts, err := probe.Get([]string{oidSysDescr})
	if err != nil || len(pkts.Variables) == 0 || pkts.Variables[0].Type == gosnmp.NoSuchObject {
		_ = probe.Conn.Close()
		if err == nil {
			err = errors.New("no sysDescr")
		}
		d.Error = err.Error()
		return d
	}
	d.Alive = true
	d.Name = pduString(pkts.Variables[0])
	_ = probe.Conn.Close()

	// Full read: scalars first.
	cli := newClient(ip, cfg, cfg.TimeoutMs, cfg.Retries)
	if err := cli.Connect(); err != nil {
		d.Error = err.Error()
		return d
	}
	defer cli.Conn.Close()

	scalars := []string{oidHrDevDescr, oidPrtName, oidPrtSerial, oidPrtPages, oidHpDevID, oidHpStatus, oidSysLocation, oidSysName}
	if p, err := cli.Get(scalars); err == nil {
		vals := map[string]gosnmp.SnmpPDU{}
		for _, v := range p.Variables {
			vals[strings.TrimPrefix(v.Name, ".")] = v
		}
		d.Model = cleanText(pduString(vals[oidPrtName]))
		d.Serial = cleanText(pduString(vals[oidPrtSerial]))
		d.Location = cleanText(pduString(vals[oidSysLocation]))
		d.Status = cleanText(pduString(vals[oidHpStatus]))
		if n, ok := pduInt(vals[oidPrtPages]); ok {
			d.Pages = n
		}
		hr := cleanText(pduString(vals[oidHrDevDescr]))
		devID := pduString(vals[oidHpDevID])
		if m := rePID.FindStringSubmatch(d.Name); m != nil {
			d.Model = strings.TrimSpace(m[1])
		} else if m := reMDL.FindStringSubmatch(devID); m != nil {
			d.Model = strings.TrimSpace(m[1])
		} else if d.Model == "" {
			d.Model = hr
		}
		if d.Serial == "" {
			if m := reSN.FindStringSubmatch(d.Name); m != nil {
				d.Serial = strings.TrimSpace(m[1])
			}
		}
		if nm := cleanText(pduString(vals[oidSysName])); nm != "" {
			d.Name = nm
		}
	}

	// Supplies table (may be absent on switches and servers).
	sups, werr := walkSupplies(cli, cfg)
	if werr != nil && werr != errNoSupplies {
		d.Supplies = sups
	} else {
		d.Supplies = sups
	}

	if len(d.Supplies) > 0 {
		d.Kind = "printer"
	} else if d.Model != "" {
		d.Kind = "printer"
	} else {
		d.Kind = "other"
	}
	d.Vendor = detectVendor(d.Model + " " + d.Name)
	if d.Model == "" {
		d.Model = d.Name
	}
	return d
}

var errNoSupplies = errors.New("no supplies")

func walkSupplies(cli *gosnmp.GoSNMP, cfg Config) ([]Supply, error) {
	byIndex := map[string]*Supply{}
	handler := func(p gosnmp.SnmpPDU) error {
		oid := strings.TrimPrefix(p.Name, ".")
		rest := strings.TrimPrefix(oid, oidSupplies+".")
		if rest == oid {
			return nil
		}
		parts := strings.SplitN(rest, ".", 2)
		if len(parts) != 2 {
			return nil
		}
		col, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil
		}
		idx := parts[1]
		s := byIndex[idx]
		if s == nil {
			var n int
			fmt.Sscanf(strings.Split(idx, ".")[0], "%d", &n)
			s = &Supply{Index: n}
			byIndex[idx] = s
		}
		switch col {
		case 6:
			s.Name = pduString(p)
		case 7:
			if v, ok := pduInt(p); ok {
				s.Unit = int(v)
			}
		case 8:
			if v, ok := pduInt(p); ok {
				s.Max = int(v)
			}
		case 9:
			if v, ok := pduInt(p); ok {
				s.Level = int(v)
			}
		}
		return nil
	}

	err := cli.BulkWalk(oidSupplies, handler)
	if err != nil {
		err = cli.Walk(oidSupplies, handler)
	}

	out := make([]Supply, 0, len(byIndex))
	for _, s := range byIndex {
		if s.Name == "" {
			continue
		}
		switch {
		case s.Level < 0:
			switch s.Level {
			case -1:
				s.Note = "unit reports 'other'"
			case -2:
				s.Note = "level unknown"
			case -3:
				s.Note = "some remaining, no percentage"
			}
		case s.Max > 0:
			pct := float64(s.Level) / float64(s.Max) * 100
			if pct > 100 {
				pct = 100
			}
			s.Pct = &pct
			if s.Unit != 19 {
				s.Note = fmt.Sprintf("raw %d/%d", s.Level, s.Max)
			}
		}
		out = append(out, *s)
	}
	if len(out) == 0 {
		return out, errNoSupplies
	}
	sortSupplies(out)
	return out, err
}

func sortSupplies(s []Supply) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Index < s[j-1].Index; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func detectVendor(s string) string {
	u := strings.ToUpper(s)
	switch {
	case strings.Contains(u, "KYOCERA") || strings.Contains(u, "ECOSYS"):
		return "Kyocera"
	case strings.Contains(u, "HEWLETT") || strings.Contains(u, "HP ") || strings.Contains(u, "LASERJET"):
		return "HP"
	case strings.Contains(u, "EXTREME"):
		return "Extreme"
	}
	return ""
}
