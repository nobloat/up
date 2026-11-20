package main

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"
)

var auditHeader = []string{"timestamp", "event", "state", "message"}

type row struct {
	Time    time.Time
	Event   string
	State   string
	Message string
}

// appendRow appends one complete record in a single write and syncs it; the file is never rewritten.
// A failed write (a full disk) is cut off again, so the next record starts on a line of its own.
// ponytail: the log grows forever, read once at startup; split per year if that gets slow
func appendRow(path string, r row) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	if fi.Size() == 0 {
		w.Write(auditHeader)
	}
	w.Write([]string{r.Time.UTC().Format(time.RFC3339), r.Event, r.State, r.Message})
	w.Flush()
	if _, err := f.Write(b.Bytes()); err != nil {
		f.Truncate(fi.Size())
		return err
	}
	return f.Sync()
}

// maxTorn: a broken end of a log up to this size is one torn append (a record is at most a few KB); more is damage startup refuses.
const maxTorn = 64 << 10

// scanLog reads an audit log record by record. A record that does not parse is skipped and reported in bad;
// a broken last record (a power cut during an append) is also reported with its offset in torn (else -1), so startup can cut it off.
// err is for what is not one bad record: the file cannot be read, its header is wrong, or a large broken end.
func scanLog(path string, fn func(row)) (bad []error, torn int64, err error) {
	torn = -1
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, torn, nil
	}
	if err != nil {
		return nil, torn, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, torn, err
	}
	size := fi.Size()
	var last [1]byte
	if size > 0 {
		if _, err := f.ReadAt(last[:], size-1); err != nil {
			return nil, torn, fmt.Errorf("%s: %w", path, err)
		}
	}
	reader := csv.NewReader(f)
	header, err := reader.Read()
	if err == io.EOF && reader.InputOffset() == 0 {
		return nil, torn, nil
	}
	if err != nil || !slices.Equal(header, auditHeader) {
		return nil, torn, fmt.Errorf("%s:1: invalid audit header", path)
	}
	for {
		start := reader.InputOffset()
		r, err := reader.Read()
		if err == io.EOF {
			return bad, torn, nil
		}
		var pe *csv.ParseError
		if err != nil && !errors.As(err, &pe) {
			return bad, -1, fmt.Errorf("%s: %w", path, err)
		}
		atEnd := reader.InputOffset() == size
		var t time.Time
		switch {
		case err != nil:
			err = fmt.Errorf("%s: %w", path, err) // a csv.ParseError carries the line
		case atEnd && last[0] != '\n':
			line, _ := reader.FieldPos(0)
			err = fmt.Errorf("%s:%d: unterminated audit record", path, line)
		default:
			line, _ := reader.FieldPos(0)
			if t, err = time.Parse(time.RFC3339, r[0]); err != nil {
				err = fmt.Errorf("%s:%d: %w", path, line, err)
			}
		}
		if err == nil {
			fn(row{t, r[1], r[2], r[3]})
			continue
		}
		bad = append(bad, err)
		if atEnd && (last[0] != '\n' || pe != nil) { // also an open quote that ran to the end: the next append must not land inside it
			if size-start > maxTorn {
				return bad, -1, fmt.Errorf("%w: %d broken bytes at the end, repair it", err, size-start)
			}
			torn = start
		}
	}
}

// readLog returns every row, or the first problem.
func readLog(path string) ([]row, error) {
	var rows []row
	bad, _, err := scanLog(path, func(r row) { rows = append(rows, r) })
	if err == nil && len(bad) > 0 {
		err = bad[0]
	}
	return rows, err
}

// checkData reads every audit log in dir like startup does and reports every problem, also those startup skips; a missing dir has none.
func checkData(dir string) (errs []error) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.csv"))
	for _, f := range files {
		bad, _, err := scanLog(f, func(row) {})
		errs = append(errs, bad...)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

const tailRows = 200 // rows on the detail page

// history is what a monitor keeps of its audit log: its state, running uptime sums and the newest rows.
type history struct {
	restored
	up   uptimeSum
	tail []row
}

func newHistory() *history { return &history{restored: restored{reported: map[string]string{}}} }

func (h *history) add(e row) {
	h.restored.add(e)
	h.up.add(e)
	h.tail = append(h.tail, e)
	if len(h.tail) > 2*tailRows {
		h.tail = slices.Clone(h.tail[len(h.tail)-tailRows:])
	}
}

// loadHistory reads a monitor's log at startup: bad records are skipped and logged, a torn last record is cut off.
func (a *App) loadHistory(path string) (*history, error) {
	h := newHistory()
	bad, torn, err := scanLog(path, h.add)
	for _, b := range bad {
		slog.Error("audit log: skipped a damaged record", "err", b)
	}
	if err != nil || torn < 0 {
		return h, err
	}
	a.store.RLock()
	defer a.store.RUnlock()
	slog.Warn("audit log: cutting off a torn last record", "file", path, "offset", torn)
	return h, os.Truncate(path, torn)
}

type restored struct {
	state, reason             string
	since, downSince, upSince time.Time
	paused                    bool
	pauseReason, pausedState  string
	pausedSince, pausedUntil  time.Time
	lastPush                  time.Time
	reported                  map[string]string
}

func restore(rows []row) restored {
	h := newHistory()
	for _, e := range rows {
		h.add(e)
	}
	return h.restored
}

func (r *restored) add(e row) {
	switch e.Event {
	case "state":
		if e.State == red && (r.state != red || r.reason == reasonNew) {
			r.downSince = e.Time
		} else if e.State != red && r.state == red {
			r.upSince = e.Time
		}
		r.state, r.reason, r.since = e.State, e.Message, e.Time
	case "paused":
		r.paused, r.pauseReason, r.pausedState, r.pausedSince = true, e.Message, r.state, e.Time
		r.pausedUntil, _ = time.Parse(time.RFC3339, e.State)
		if e.Message == reasonNone {
			r.pauseReason = ""
		}
	case "resumed":
		r.paused, r.pauseReason, r.pausedUntil = false, "", time.Time{}
	case "push":
		r.lastPush = e.Time
	case "reported":
		r.reported[e.Message] = e.State
	}
}

type uptime struct {
	Percent         *float64  `json:"percent"`
	DowntimeSeconds int64     `json:"downtime_seconds"`
	Since           time.Time `json:"since"`
}

func (u uptime) Pct() string {
	if u.Percent == nil {
		return "–"
	}
	return fmt.Sprintf("%.2f%%", *u.Percent)
}

func (u uptime) Down() string { return fmtDuration(time.Duration(u.DowntimeSeconds) * time.Second) }

func fmtDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

// uptimeSum adds up the time per state as rows are appended, so no page re-reads the log.
type uptimeSum struct {
	since, last time.Time
	sum         map[string]time.Duration
	cur, known  string
	running     bool
}

func (u *uptimeSum) add(e row) {
	if u.sum == nil {
		u.sum, u.since, u.last = map[string]time.Duration{}, e.Time, e.Time
	}
	st := u.cur
	if e.Event == "start" && u.running {
		st = ""
	}
	u.sum[st] += e.Time.Sub(u.last)
	u.last = e.Time
	switch e.Event {
	case "start":
		u.running, u.cur = true, u.known
	case "stop":
		u.running, u.cur = false, ""
	case "state":
		u.known, u.cur = e.State, e.State
		if e.Message == reasonNew {
			u.known, u.cur = "", ""
		}
	}
}

func (u *uptimeSum) at(now time.Time) uptime {
	var out uptime
	if u.sum == nil {
		return out
	}
	out.Since = u.since
	g, r := u.sum[green], u.sum[red]
	if u.running && u.cur == green {
		g += now.Sub(u.last)
	} else if u.running && u.cur == red {
		r += now.Sub(u.last)
	}
	if g+r > 0 {
		p := float64(g) / float64(g+r) * 100
		out.Percent = &p
	}
	out.DowntimeSeconds = int64(r.Seconds())
	return out
}

func computeUptime(rows []row, now time.Time) uptime {
	var u uptimeSum
	for _, e := range rows {
		u.add(e)
	}
	return u.at(now)
}
