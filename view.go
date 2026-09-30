package main

import (
	"sort"
	"strings"
	"time"
)

// SupplyTrend is the observed behaviour of one consumable over time.
type SupplyTrend struct {
	Name     string   `json:"name"`
	Pct      *float64 `json:"pct"`
	PerDay   *float64 `json:"perDay,omitempty"`
	DaysLeft *float64 `json:"daysLeft,omitempty"`
	Note     string   `json:"note,omitempty"`
}

// ViewDevice is what the dashboard shows for one printer.
type ViewDevice struct {
	Device
	LastSeen    string        `json:"lastSeen"`
	Online      bool          `json:"online"`
	MinPct      *float64      `json:"minPct"`
	WorstSupply string        `json:"worstSupply,omitempty"`
	DaysLeft    *float64      `json:"daysLeft,omitempty"`
	Trends      []SupplyTrend `json:"trends"`
}

// Alert is a "order toner" style notification.
type Alert struct {
	IP       string  `json:"ip"`
	Model    string  `json:"model"`
	Location string  `json:"location,omitempty"`
	Level    string  `json:"level"`
	Pct      float64 `json:"pct"`
	Text     string  `json:"text"`
}

// GroupStat is the per-location or per-cartridge summary.
type GroupStat struct {
	Name   string  `json:"name"`
	Count  int     `json:"count"`
	Lowest float64 `json:"lowest"`
	Low    int     `json:"low"`
}

// View is the whole dashboard payload.
type View struct {
	Generated string       `json:"generated"`
	Warn      float64      `json:"warn"`
	Crit      float64      `json:"crit"`
	Devices   []ViewDevice `json:"devices"`
	Alerts    []Alert      `json:"alerts"`
	Groups    []GroupStat  `json:"groups"`
}

type point struct {
	t   time.Time
	pct float64
}

// BuildView merges the current poll results with the stored history.
func BuildView(cfg Config, devices []Device, readings []Reading, lastSeen map[string]time.Time) View {
	v := View{
		Generated: time.Now().Format("2006-01-02 15:04:05"),
		Warn:      cfg.WarnPercent,
		Crit:      cfg.CritPercent,
		Devices:   []ViewDevice{},
		Alerts:    []Alert{},
		Groups:    []GroupStat{},
	}

	byIP := map[string][]Reading{}
	for _, r := range readings {
		byIP[r.IP] = append(byIP[r.IP], r)
	}

	onlineWindow := time.Duration(cfg.IntervalMinutes*2+10) * time.Minute

	for _, d := range devices {
		vd := ViewDevice{Device: d, Trends: trendsFor(byIP[d.IP])}
		// Кабинет: значение из настроек главнее того, что отдаёт устройство;
		// пустое или «состоянием» занятое поле показываем как «не задан».
		if loc := cfg.Locations[d.IP]; loc != "" {
			vd.Location = loc
		} else {
			vd.Location = cleanLocation(vd.Location)
		}
		if t, ok := lastSeen[d.IP]; ok {
			vd.LastSeen = t.Local().Format("2006-01-02 15:04:05")
			vd.Online = time.Since(t) < onlineWindow
		}
		// current supplies from the device, enriched by trends
		trendBy := map[string]SupplyTrend{}
		for _, t := range vd.Trends {
			trendBy[t.Name] = t
		}
		for i := range d.Supplies {
			s := &d.Supplies[i]
			if s.Pct == nil {
				continue
			}
			if vd.MinPct == nil || *s.Pct < *vd.MinPct {
				p := *s.Pct
				vd.MinPct = &p
				vd.WorstSupply = s.Name
				if t, ok := trendBy[s.Name]; ok && t.DaysLeft != nil {
					dl := *t.DaysLeft
					vd.DaysLeft = &dl
				} else {
					vd.DaysLeft = nil
				}
			}
			if t, ok := trendBy[s.Name]; ok {
				if t.Pct == nil {
					p := *s.Pct
					t.Pct = &p
				}
				trendBy[s.Name] = t
			} else {
				p := *s.Pct
				trendBy[s.Name] = SupplyTrend{Name: s.Name, Pct: &p}
			}
		}
		vd.Trends = vd.Trends[:0]
		for _, name := range sortedKeys(trendBy) {
			vd.Trends = append(vd.Trends, trendBy[name])
		}
		v.Devices = append(v.Devices, vd)

		if vd.MinPct == nil {
			continue
		}
		switch {
		case *vd.MinPct <= cfg.CritPercent:
			v.Alerts = append(v.Alerts, Alert{
				IP: d.IP, Model: d.Model, Location: vd.Location, Level: "crit", Pct: *vd.MinPct,
				Text: describe(vd),
			})
		case *vd.MinPct <= cfg.WarnPercent:
			v.Alerts = append(v.Alerts, Alert{
				IP: d.IP, Model: d.Model, Location: vd.Location, Level: "warn", Pct: *vd.MinPct,
				Text: describe(vd),
			})
		}
	}

	sort.Slice(v.Alerts, func(i, j int) bool { return v.Alerts[i].Pct < v.Alerts[j].Pct })
	sort.Slice(v.Devices, func(i, j int) bool {
		a, b := v.Devices[i].MinPct, v.Devices[j].MinPct
		switch {
		case a == nil && b == nil:
			return v.Devices[i].IP < v.Devices[j].IP
		case a == nil:
			return false
		case b == nil:
			return true
		}
		return *a < *b
	})

	// groups: by configured group name, else by cartridge model family
	stats := map[string]*GroupStat{}
	for _, d := range v.Devices {
		name := d.Group
		if name == "" {
			name = "парк"
		}
		st := stats[name]
		if st == nil {
			st = &GroupStat{Name: name, Lowest: 100}
			stats[name] = st
		}
		st.Count++
		if d.MinPct != nil {
			if *d.MinPct < st.Lowest {
				st.Lowest = *d.MinPct
			}
			if *d.MinPct <= cfg.WarnPercent {
				st.Low++
			}
		}
	}
	for _, name := range sortedStats(stats) {
		v.Groups = append(v.Groups, *stats[name])
	}
	return v
}

// cleanLocation убирает то, что принтеры кладут в sysLocation вместо кабинета:
// строку состояния устройства («ожид. вкл.», «IDLE») или мусор.
func cleanLocation(s string) string {
	t := strings.TrimSpace(s)
	switch {
	case t == "", t == "?????", t == "-", t == "—":
		return ""
	case strings.Contains(t, "ожид"), strings.Contains(t, "РѕР¶"),
		strings.Contains(t, "No Such Object"),
		strings.Contains(strings.ToUpper(t), "IDLE"):
		return ""
	}
	return t
}

func describe(d ViewDevice) string {
	txt := d.WorstSupply
	if txt == "" {
		txt = "расходник"
	}
	if d.DaysLeft != nil && *d.DaysLeft < 60 {
		txt += " заканчивается"
	}
	return txt
}

func sortedKeys(m map[string]SupplyTrend) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedStats(m map[string]*GroupStat) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// trendsFor computes consumption rate and days-left per consumable.
func trendsFor(rs []Reading) []SupplyTrend {
	byName := map[string][]point{}
	for _, r := range rs {
		for _, s := range r.Supplies {
			if s.Pct == nil {
				continue
			}
			byName[s.Name] = append(byName[s.Name], point{t: r.TS, pct: *s.Pct})
		}
	}
	var out []SupplyTrend
	for name, pts := range byName {
		sort.Slice(pts, func(i, j int) bool { return pts[i].t.Before(pts[j].t) })
		if len(pts) == 0 {
			continue
		}
		tr := SupplyTrend{Name: name}
		last := pts[len(pts)-1]
		lastPct := last.pct
		tr.Pct = &lastPct

		seg := decliningSegment(pts)
		if len(seg) >= 3 {
			span := seg[len(seg)-1].t.Sub(seg[0].t).Hours()
			if span >= 6 {
				rate := slopePerDay(seg)
				tr.PerDay = &rate
				if rate < -0.01 && lastPct > 0 {
					days := lastPct / (-rate)
					tr.DaysLeft = &days
				}
			}
		}
		out = append(out, tr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// decliningSegment keeps only the tail after the last refill (level jump up).
func decliningSegment(pts []point) []point {
	start := 0
	for i := len(pts) - 1; i > 0; i-- {
		if pts[i].pct > pts[i-1].pct+0.5 {
			start = i
			break
		}
	}
	return pts[start:]
}

// slopePerDay is a least-squares fit in percentage points per day.
func slopePerDay(pts []point) float64 {
	if len(pts) < 2 {
		return 0
	}
	t0 := pts[0].t
	var n, sx, sy, sxy, sxx float64
	for _, p := range pts {
		x := p.t.Sub(t0).Hours() / 24
		y := p.pct
		n++
		sx += x
		sy += y
		sxy += x * y
		sxx += x * x
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / den
}
