package main

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

var hexNameRe = regexp.MustCompile(`^(?:[0-9A-F]{2}\s)+[0-9A-F]{2}$`)

// Store keeps device inventory and the level history in SQLite.
type Store struct {
	db *sql.DB
}

// Reading is one measurement of one device.
type Reading struct {
	IP       string
	TS       time.Time
	Pages    int64
	Status   string
	Supplies []Supply
}

const schema = `
CREATE TABLE IF NOT EXISTS devices (
  ip TEXT PRIMARY KEY, name TEXT, model TEXT, vendor TEXT, serial TEXT,
  kind TEXT, location TEXT, group_name TEXT, first_seen TEXT, last_seen TEXT);
CREATE TABLE IF NOT EXISTS readings (
  id INTEGER PRIMARY KEY AUTOINCREMENT, ip TEXT NOT NULL, ts TEXT NOT NULL,
  pages INTEGER, status TEXT, supplies TEXT);
CREATE INDEX IF NOT EXISTS idx_readings_ip_ts ON readings(ip, ts);
`

// OpenStore opens (and initialises) the database file.
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Save stores device metadata and one reading. Failed polls are not stored.
func (s *Store) Save(d Device, ts time.Time) error {
	if d.Error != "" || !d.Alive {
		return nil
	}
	t := ts.UTC().Format(time.RFC3339)
	_, err := s.db.Exec(`
INSERT INTO devices (ip,name,model,vendor,serial,kind,location,group_name,first_seen,last_seen)
VALUES (?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(ip) DO UPDATE SET
  name=excluded.name, model=excluded.model, vendor=excluded.vendor, serial=excluded.serial,
  kind=excluded.kind, location=excluded.location, group_name=excluded.group_name,
  last_seen=excluded.last_seen`,
		d.IP, d.Name, d.Model, d.Vendor, d.Serial, d.Kind, d.Location, d.Group, t, t)
	if err != nil {
		return err
	}
	sup, _ := json.Marshal(d.Supplies)
	_, err = s.db.Exec(`INSERT INTO readings (ip,ts,pages,status,supplies) VALUES (?,?,?,?,?)`,
		d.IP, t, d.Pages, d.Status, string(sup))
	return err
}

// DeleteDevice forgets a device completely: card, history and level readings.
// «Не хочу его контролировать» должно убирать аппарат из таблицы, а не
// оставлять его там вечно «не отвечающим».
func (s *Store) DeleteDevice(ip string) error {
	if _, err := s.db.Exec(`DELETE FROM readings WHERE ip = ?`, ip); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM devices WHERE ip = ?`, ip)
	return err
}

// Devices returns the known inventory.
func (s *Store) Devices() ([]Device, error) {
	rows, err := s.db.Query(`SELECT ip,name,model,vendor,serial,kind,location,group_name FROM devices`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.IP, &d.Name, &d.Model, &d.Vendor, &d.Serial, &d.Kind, &d.Location, &d.Group); err != nil {
			return nil, err
		}
		d.Alive = true
		out = append(out, d)
	}
	sortByIP(out)
	return out, rows.Err()
}

// LoadWindow returns readings newer than the given number of days.
func (s *Store) LoadWindow(days int) ([]Reading, error) {
	since := time.Now().AddDate(0, 0, -days).UTC().Format(time.RFC3339)
	rows, err := s.db.Query(`SELECT ip,ts,pages,status,supplies FROM readings WHERE ts >= ? ORDER BY ts`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reading
	for rows.Next() {
		var ip, ts, status, sup string
		var pages int64
		if err := rows.Scan(&ip, &ts, &pages, &status, &sup); err != nil {
			return nil, err
		}
		t, _ := time.Parse(time.RFC3339, ts)
		var supplies []Supply
		_ = json.Unmarshal([]byte(sup), &supplies)
		out = append(out, Reading{IP: ip, TS: t, Pages: pages, Status: status, Supplies: supplies})
	}
	return out, rows.Err()
}

// LastSeen returns the newest reading timestamp per IP.
func (s *Store) LastSeen() (map[string]time.Time, []Reading, error) {
	rows, err := s.db.Query(`SELECT ip, ts, pages, status, supplies FROM readings
WHERE id IN (SELECT MAX(id) FROM readings GROUP BY ip)`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	var latest []Reading
	for rows.Next() {
		var ip, ts, status, sup string
		var pages int64
		if err := rows.Scan(&ip, &ts, &pages, &status, &sup); err != nil {
			return nil, nil, err
		}
		t, _ := time.Parse(time.RFC3339, ts)
		var supplies []Supply
		_ = json.Unmarshal([]byte(sup), &supplies)
		out[ip] = t
		latest = append(latest, Reading{IP: ip, TS: t, Pages: pages, Status: status, Supplies: supplies})
	}
	return out, latest, rows.Err()
}

// SeedFromCSV imports a printers.csv produced by the PowerShell poller.
// Used for testing and for a quick first fill of the history.
func (s *Store) SeedFromCSV(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// PowerShell writes UTF-8 BOM; Go's csv reader would choke on it.
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	r := csv.NewReader(bytes.NewReader(raw))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	r.TrimLeadingSpace = true
	recs, err := r.ReadAll()
	if err != nil {
		return err
	}
	if len(recs) < 2 {
		return fmt.Errorf("empty csv")
	}
	head := map[string]int{}
	for i, h := range recs[0] {
		head[strings.TrimSpace(strings.ToLower(h))] = i
	}
	get := func(rec []string, key string) string {
		i, ok := head[key]
		if !ok || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}
	now := time.Now()
	n := 0
	for _, rec := range recs[1:] {
		ip := get(rec, "ip")
		if ip == "" {
			continue
		}
		model := get(rec, "model")
		if strings.HasPrefix(model, "No Such Object") {
			model = ""
		}
		sd := get(rec, "sysdescr")
		if model == "" && strings.Contains(sd, "ExtremeXOS") {
			model = "ExtremeXOS switch"
		}
		d := Device{
			IP:       ip,
			Name:     get(rec, "name"),
			Model:    model,
			Serial:   cleanText(get(rec, "serial")),
			Pages:    parseInt(get(rec, "pages")),
			Location: get(rec, "location"),
			Status:   get(rec, "location"), // poller put device status here
			Supplies: parseSuppliesCell(get(rec, "supplies")),
			Alive:    true,
		}
		d.Vendor = detectVendor(d.Model + " " + sd)
		if len(d.Supplies) > 0 {
			d.Kind = "printer"
		} else {
			d.Kind = "other"
		}
		if err := s.Save(d, now); err != nil {
			return err
		}
		n++
	}
	return nil
}

func parseInt(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

// parseSuppliesCell understands "Black Cartridge HP CF226X=48% | TK-1140=432/7200".
func parseSuppliesCell(cell string) []Supply {
	var out []Supply
	for i, part := range strings.Split(cell, "|") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		at := strings.LastIndex(part, "=")
		if at < 0 {
			continue
		}
		name := strings.TrimSpace(part[:at])
		name = decodeHexName(name)
		val := strings.TrimSpace(part[at+1:])
		s := Supply{Index: i + 1, Name: name}
		switch {
		case strings.HasSuffix(val, "%"):
			pct := parseFloat(strings.TrimSuffix(val, "%"))
			s.Pct = &pct
			s.Level = int(pct)
			s.Max = 100
			s.Unit = 19
		case strings.Contains(val, "/"):
			parts := strings.SplitN(val, "/", 2)
			lv, mx := parseInt(parts[0]), parseInt(parts[1])
			s.Level = int(lv)
			s.Max = int(mx)
			if mx > 0 {
				pct := float64(lv) / float64(mx) * 100
				s.Pct = &pct
				s.Note = fmt.Sprintf("raw %d/%d", lv, mx)
			}
		default:
			s.Note = val
		}
		out = append(out, s)
	}
	return out
}

func parseFloat(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return f
}

// decodeHexName turns "D0 A7 D0 B5 ..." (how net-snmp prints non-ASCII strings)
// back into readable text. Some HP firmware stores Russian supply names.
func decodeHexName(s string) string {
	if !hexNameRe.MatchString(s) {
		return s
	}
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil || !utf8.Valid(b) {
		return s
	}
	return strings.TrimSpace(string(b))
}
