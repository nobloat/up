package main

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
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
	w := csv.NewWriter(f)
	if fi.Size() == 0 {
		w.Write(auditHeader)
	}
	w.Write([]string{r.Time.UTC().Format(time.RFC3339), r.Event, r.State, r.Message})
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	return f.Sync()
}

func readLog(path string) ([]row, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	reader := csv.NewReader(f)
	header, err := reader.Read()
	if err == io.EOF && reader.InputOffset() == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !slices.Equal(header, auditHeader) {
		return nil, errors.New("invalid audit header")
	}
	var rows []row
	for {
		r, err := reader.Read()
		if err == io.EOF {
			var last [1]byte
			if _, err := f.ReadAt(last[:], reader.InputOffset()-1); err != nil {
				return rows, err
			}
			if last[0] != '\n' {
				return rows, errors.New("unterminated audit record")
			}
			return rows, nil
		}
		if err != nil {
			return rows, err
		}
		t, err := time.Parse(time.RFC3339, r[0])
		if err != nil {
			return rows, err
		}
		rows = append(rows, row{t, r[1], r[2], r[3]})
	}
}

type restored struct {
	state, reason            string
	since                    time.Time
	paused                   bool
	pauseReason, pausedState string
	pausedSince, pausedUntil time.Time
	lastPush                 time.Time
	reported                 map[string]string
}

func restore(rows []row) restored {
	r := restored{reported: map[string]string{}}
	for _, e := range rows {
		switch e.Event {
		case "state":
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
	return r
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

func (u uptime) Down() string {
	d := time.Duration(u.DowntimeSeconds) * time.Second
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

func computeUptime(rows []row, now time.Time) uptime {
	var u uptime
	if len(rows) == 0 {
		return u
	}
	u.Since = rows[0].Time
	sum := map[string]time.Duration{}
	var cur, known string
	last := rows[0].Time
	running := false
	for _, e := range rows {
		st := cur
		if e.Event == "start" && running {
			st = ""
		}
		sum[st] += e.Time.Sub(last)
		last = e.Time
		switch e.Event {
		case "start":
			running, cur = true, known
		case "stop":
			running, cur = false, ""
		case "state":
			known, cur = e.State, e.State
			if e.Message == reasonNew {
				known, cur = "", ""
			}
		}
	}
	if running {
		sum[cur] += now.Sub(last)
	}
	if up := sum[green] + sum[red]; up > 0 {
		p := float64(sum[green]) / float64(up) * 100
		u.Percent = &p
	}
	u.DowntimeSeconds = int64(sum[red].Seconds())
	return u
}
