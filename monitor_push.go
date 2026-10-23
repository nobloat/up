package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

type PushConfig struct {
	Token string `json:"token"`
}

type pushMonitor struct{}

func (pushMonitor) validate(c *MonitorConfig) error {
	if len(c.Push.Token) < 16 {
		return errors.New("push.token must be at least 16 characters")
	}
	return nil
}

func (pushMonitor) run(ctx context.Context, m *Monitor) {
	lastPush := func() time.Time {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.lastPush
	}
	seen := lastPush()
	since := seen
	if since.IsZero() {
		since = time.Now()
	}
	cfg := m.config()
	var lastFailure time.Time
	deadline := since.Add(cfg.Interval.D() + cfg.Grace.D())
	for {
		m.setNext(deadline)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(deadline)):
		case <-m.wake:
			next := m.config()
			if next.Interval != cfg.Interval || next.Grace != cfg.Grace {
				deadline = since.Add(next.Interval.D() + next.Grace.D())
				lastFailure = time.Time{}
			} else if next.RetryInterval != cfg.RetryInterval && !lastFailure.IsZero() {
				deadline = lastFailure.Add(next.RetryInterval.D())
			}
			cfg = next
			if p := lastPush(); p.After(seen) {
				seen, since = p, p
				lastFailure = time.Time{}
				deadline = p.Add(cfg.Interval.D() + cfg.Grace.D())
				continue
			}
			if time.Now().Before(deadline) {
				continue
			}
		}
		lastFailure = time.Now()
		deadline = lastFailure.Add(m.config().RetryInterval.D())
		m.record(checkResult{Time: lastFailure, Reason: "no push for " + time.Since(since).Round(time.Second).String()}, false)
	}
}

func payloadMessage(payload []byte) string {
	if len(payload) == 0 {
		return "(no payload)"
	}
	var b bytes.Buffer
	if json.Compact(&b, payload) == nil {
		return b.String()
	}
	s, _ := json.Marshal(string(payload))
	return string(s)
}

func (m *Monitor) push(up bool, reason string, payload []byte) {
	now := time.Now()
	m.mu.Lock()
	m.log(row{Time: now, Event: "push", Message: payloadMessage(payload)})
	m.lastPush = now
	m.mu.Unlock()
	if reason == "" {
		reason = map[bool]string{true: "push received", false: "reported down"}[up]
	}
	m.record(checkResult{Time: now, OK: up, Reason: reason}, false)
	m.checkNow()
}

func (m *Monitor) signal(kind string, payload []byte) {
	m.mu.Lock()
	m.log(row{Event: "signal", Message: kind + ": " + payloadMessage(payload)})
	m.mu.Unlock()
	slog.Info("push signal", "monitor", m.name, "kind", kind)
}

func (a *App) pushByToken(token string) *Monitor {
	for _, mc := range a.cfg.Load().Monitors {
		if mc.Push != nil && subtle.ConstantTimeCompare([]byte(token), []byte(mc.Push.Token)) == 1 {
			return a.monitor(mc.Name)
		}
	}
	return nil
}

func readPayload(r *http.Request) (body []byte, tooLarge bool, err error) {
	body, err = io.ReadAll(io.LimitReader(r.Body, 4097))
	if len(body) > 4096 {
		return body[:4096], true, err
	}
	return body, false, err
}

func (a *App) handlePush(w http.ResponseWriter, r *http.Request) {
	name, token := splitLast(r.PathValue("path"))
	m := a.monitor(name)
	if m == nil {
		http.NotFound(w, r)
		return
	}
	cfg := m.config()
	if cfg.Push == nil || subtle.ConstantTimeCompare([]byte(token), []byte(cfg.Push.Token)) != 1 {
		http.NotFound(w, r)
		return
	}
	body, tooLarge, err := readPayload(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if tooLarge {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	status := r.URL.Query().Get("status")
	if status != "" && status != "up" && status != "down" {
		http.Error(w, "status must be up or down", http.StatusBadRequest)
		return
	}
	m.push(status != "down", "", body)
	io.WriteString(w, "ok\n")
}

func (a *App) handleKumaPush(w http.ResponseWriter, r *http.Request) {
	m := a.pushByToken(r.PathValue("token"))
	if m == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "msg": "Monitor not found or not active."})
		return
	}
	body, tooLarge, err := readPayload(r)
	if err != nil || tooLarge {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "payload unreadable or larger than 4 KB"})
		return
	}
	q := r.URL.Query()
	status, msg := cmp.Or(q.Get("status"), "up"), q.Get("msg")
	if status != "up" && status != "down" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "status must be up or down"})
		return
	}
	reason := ""
	if msg != "" {
		reason = map[string]string{"up": "push received: ", "down": "reported down: "}[status] + msg
		if len(body) == 0 {
			body = []byte(msg)
		}
	}
	m.push(status == "up", reason, body)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleHealthchecksPing(w http.ResponseWriter, r *http.Request) {
	m := a.pushByToken(r.PathValue("token"))
	if m == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	body, _, err := readPayload(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch s := r.PathValue("signal"); s {
	case "":
		m.push(true, "", body)
	case "fail":
		m.push(false, "", body)
	case "start", "log":
		m.signal(s, body)
	default:
		code, err := strconv.Atoi(s)
		if err != nil || code < 0 || code > 255 {
			http.Error(w, "invalid url format", http.StatusBadRequest)
			return
		}
		m.push(code == 0, map[bool]string{true: "push received", false: "reported down"}[code == 0]+" (exit "+s+")", body)
	}
	io.WriteString(w, "OK")
}
