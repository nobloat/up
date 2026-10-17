package main

import (
	"cmp"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type monitorType interface {
	validate(c *MonitorConfig) error
	run(ctx context.Context, m *Monitor)
}

var monitorTypes = map[string]monitorType{
	"http":   httpMonitor{},
	"push":   pushMonitor{},
	"metric": metricMonitor{},
	"promql": promqlMonitor{},
}

const (
	stateNew   = "new"
	reasonNone = "no reason given"
	reasonNew  = "new monitor"
)

const (
	green  = "green"
	yellow = "yellow"
	red    = "red"
)

type checkResult struct {
	Time     time.Time     `json:"time"`
	Duration time.Duration `json:"duration"`
	OK       bool          `json:"ok"`
	Reason   string        `json:"reason"`
	Samples  []sample      `json:"samples,omitempty"`
}

type Monitor struct {
	app  *App
	name string
	path string
	wake chan struct{}

	cancel context.CancelFunc
	done   chan struct{}

	mu          sync.Mutex
	cfg         *MonitorConfig
	state       string
	reason      string
	since       time.Time
	failures    int
	paused      bool
	pauseReason string
	pausedState string
	pausedSince time.Time
	pausedUntil time.Time
	resume      *time.Timer
	lastCheck   *checkResult
	checking    bool
	nextCheck   time.Time
	lastPush    time.Time
	reported    map[string]string
}

func monitorPath(dir, name string) string {
	return filepath.Join(dir, strings.NewReplacer(".", "_", "/", "+").Replace(name)+".csv")
}

func newMonitor(a *App, cfg *MonitorConfig, why string, rows []row) *Monitor {
	m := &Monitor{app: a, name: cfg.Name, cfg: cfg, path: monitorPath(a.dataDir, cfg.Name),
		wake: make(chan struct{}, 1)}
	r := restore(rows)
	m.log(row{Event: "start", Message: why})
	if r.state == "" {
		r.state, r.reason, r.since = red, reasonNew, time.Now()
		m.log(row{Event: "state", State: r.state, Message: r.reason})
	}
	m.state, m.reason, m.since = r.state, r.reason, r.since
	m.paused, m.pauseReason, m.pausedState, m.pausedSince, m.pausedUntil = r.paused, r.pauseReason, r.pausedState, r.pausedSince, r.pausedUntil
	m.reported, m.lastPush = r.reported, r.lastPush
	m.failures = map[string]int{green: 0, yellow: 1, red: cfg.RetryCount + 1}[m.state]
	if m.reason == reasonNew {
		m.failures = 0
	}
	m.mu.Lock()
	m.scheduleResume()
	m.mu.Unlock()
	return m
}

func (m *Monitor) log(r row) error {
	if r.Time.IsZero() {
		r.Time = time.Now()
	}
	if err := appendRow(m.path, r); err != nil {
		slog.Error("writing audit log", "monitor", m.name, "err", err)
		return err
	}
	return nil
}

func (m *Monitor) config() *MonitorConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

func (m *Monitor) start(ctx context.Context) {
	ctx, m.cancel = context.WithCancel(ctx)
	m.done = make(chan struct{})
	t := monitorTypes[m.config().Type]
	go func() {
		defer close(m.done)
		t.run(ctx, m)
	}()
}

func (m *Monitor) halt() {
	m.cancel()
	<-m.done
}

func (m *Monitor) retire(why string) {
	m.halt()
	m.mu.Lock()
	if m.resume != nil {
		m.resume.Stop()
	}
	m.log(row{Event: "stop", Message: why})
	m.mu.Unlock()
}

func (m *Monitor) checkNow() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Monitor) manualCheck() {
	m.mu.Lock()
	m.log(row{Event: "check", Message: "manual check"})
	m.mu.Unlock()
	slog.Info("manual check", "monitor", m.name)
	m.checkNow()
}

func (m *Monitor) record(res checkResult, fatal bool) {
	m.mu.Lock()
	m.lastCheck, m.checking = &res, false
	placeholder := m.reason == reasonNew
	state := green
	if res.OK {
		m.failures = 0
	} else {
		m.failures++
		state = yellow
		if fatal || m.failures > m.cfg.RetryCount || m.state == red && !placeholder {
			state = red
		}
	}
	old := m.state
	m.reason = res.Reason
	if state != old || placeholder {
		m.state, m.since = state, res.Time
		m.log(row{Time: res.Time, Event: "state", State: state, Message: res.Reason})
	}
	paused := m.paused
	m.mu.Unlock()
	slog.Info("check", "monitor", m.name, "ok", res.OK, "reason", res.Reason, "duration", res.Duration.Round(time.Millisecond), "state", state)
	m.app.publish(event{Type: "check", Monitor: m.name, State: state})
	if placeholder {
		old = stateNew
	}
	if state != old {
		slog.Warn("state changed", "monitor", m.name, "old", old, "new", state, "reason", res.Reason)
		m.app.publish(event{Type: "state", Monitor: m.name, Old: old, State: state,
			Important: !paused && !placeholder && (old == red) != (state == red)})
	}
}

func (m *Monitor) startCheck() {
	m.mu.Lock()
	m.checking = true
	m.mu.Unlock()
	m.app.publish(event{Type: "checking", Monitor: m.name})
}

func (m *Monitor) setNext(t time.Time) {
	m.mu.Lock()
	m.nextCheck = t
	m.mu.Unlock()
}

func (m *Monitor) retryDelay() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == yellow {
		return m.cfg.RetryInterval.D()
	}
	return m.cfg.Interval.D()
}

func (m *Monitor) setPaused(paused bool, reason string, until time.Time) {
	m.mu.Lock()
	ended := !paused && !until.IsZero()
	if ended && !(m.paused && m.pausedUntil.Equal(until)) {
		m.mu.Unlock()
		return
	}
	if m.resume != nil {
		m.resume.Stop()
		m.resume = nil
	}
	ev, msg, end := "resumed", "resumed, was paused: "+cmp.Or(m.pauseReason, reasonNone), ""
	if ended {
		msg = "pause ended, was paused: " + cmp.Or(m.pauseReason, reasonNone)
	}
	m.pausedUntil = time.Time{}
	if paused {
		ev, msg, m.pausedState, m.pausedSince, m.pausedUntil = "paused", cmp.Or(reason, reasonNone), m.state, time.Now(), until
		if !until.IsZero() {
			end = until.UTC().Format(time.RFC3339)
		}
	}
	m.paused, m.pauseReason = paused, reason
	m.log(row{Event: ev, State: end, Message: msg})
	m.scheduleResume()
	state := m.state
	m.mu.Unlock()
	slog.Info(ev, "monitor", m.name, "reason", reason, "until", end)
	m.app.publish(event{Type: ev, Monitor: m.name, State: state})
}

func (m *Monitor) scheduleResume() {
	if until := m.pausedUntil; m.paused && !until.IsZero() {
		m.resume = time.AfterFunc(time.Until(until), func() { m.setPaused(false, "", until) })
	}
}

type monitorView struct {
	Name        string       `json:"name"`
	Type        string       `json:"type"`
	Description string       `json:"description,omitempty"`
	Tags        []string     `json:"tags"`
	State       string       `json:"state"`
	Reason      string       `json:"reason"`
	Since       time.Time    `json:"since"`
	Paused      bool         `json:"paused"`
	PauseReason string       `json:"pause_reason,omitempty"`
	PausedSince time.Time    `json:"paused_since,omitzero"`
	PausedUntil time.Time    `json:"paused_until,omitzero"`
	Uptime      *uptime      `json:"uptime,omitempty"`
	LastCheck   *checkResult `json:"last_check,omitempty"`
	Checking    bool         `json:"checking"`
	NextCheck   time.Time    `json:"next_check,omitzero"`
	URL         string       `json:"url,omitempty"`
	Pos         int          `json:"-"`
}

func (m *Monitor) view() monitorView {
	m.mu.Lock()
	defer m.mu.Unlock()
	return monitorView{Name: m.name, Type: m.cfg.Type, Description: m.cfg.Description, Tags: m.cfg.Tags, State: m.state, Reason: m.reason,
		Since: m.since, Paused: m.paused, PauseReason: m.pauseReason, PausedSince: m.pausedSince, PausedUntil: m.pausedUntil, LastCheck: m.lastCheck, Checking: m.checking, NextCheck: m.nextCheck}
}
