package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

type notifierType interface {
	validate(n *NotifierConfig) error
	send(a *App, n *NotifierConfig, msg message) error
}

var notifierTypes = map[string]notifierType{
	"webhook": webhookNotifier{},
	"smtp":    smtpNotifier{},
	"ilert":   ilertNotifier{},
}

type change struct {
	Name, Old, New, Reason, URL string
}

type message struct {
	Changes []change
	Red     []monitorView
	All     []monitorView
}

func (msg message) subject() string {
	if len(msg.Changes) > 0 {
		return fmt.Sprintf("%d change(s)", len(msg.Changes))
	}
	return fmt.Sprintf("%d still %s", len(msg.Red), dot(red))
}

var emoji = map[string]string{green: "🟢", yellow: "🟡", red: "🔴"}

func dot(state string) string {
	if e, ok := emoji[state]; ok {
		return e
	}
	return state
}

func (msg message) text() string {
	var b strings.Builder
	if len(msg.Changes) > 0 {
		for _, c := range msg.Changes {
			fmt.Fprintf(&b, "%s: %s (%s) %s\n", c.Name, dot(c.New), c.Reason, c.URL)
		}
		return b.String()
	}
	b.WriteString("still " + dot(red) + ":\n")
	for _, v := range msg.Red {
		fmt.Fprintf(&b, "%s: %s %s\n", v.Name, v.Reason, v.URL)
	}
	return b.String()
}

func (m *Monitor) diff(n string) (change, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev, ok := m.reported[n]
	if !ok {
		prev = stateNew
	}
	if m.paused {
		m.setReportedLocked(n, m.pausedState)
		return change{}, false
	}
	if prev == stateNew {
		if m.state == yellow || m.reason == reasonNew {
			return change{}, false
		}
		return change{Name: m.name, Old: prev, New: m.state, Reason: m.reason}, true
	}
	c := change{Name: m.name, Old: prev, New: m.state, Reason: m.reason}
	if (prev == red) != (m.state == red) {
		return c, true
	}
	m.setReportedLocked(n, m.state)
	return c, false
}

func (m *Monitor) setReported(n, state string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.setReportedLocked(n, state)
}

func (m *Monitor) setReportedLocked(n, state string) error {
	if m.reported[n] == state {
		return nil
	}
	if err := m.log(row{Event: "reported", State: state, Message: n}); err != nil {
		return err
	}
	m.reported[n] = state
	return nil
}

func (a *App) evalLoop(ctx context.Context) {
	lastSent := map[string]time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(a.cfg.Load().EvaluationInterval.D()):
		}
		a.evaluate(lastSent)
	}
}

func (a *App) evaluate(lastSent map[string]time.Time) {
	cfg := a.cfg.Load()
	a.mu.Lock()
	ms := maps.Clone(a.monitors)
	a.mu.Unlock()
	now := time.Now()
	for _, name := range slices.Sorted(maps.Keys(cfg.Notifiers)) {
		nc := cfg.Notifiers[name]
		var msg message
		sent := map[*Monitor]string{}
		for _, mc := range cfg.Monitors {
			m := ms[mc.Name]
			if m == nil || !slices.Contains(mc.Notifiers, name) {
				continue
			}
			link := cfg.BaseURL + "/monitor/" + mc.Name
			if c, ok := m.diff(name); ok {
				c.URL = link
				sent[m] = c.New
				msg.Changes = append(msg.Changes, c)
			}
			v := m.view()
			v.URL = link
			msg.All = append(msg.All, v)
			if v.State == red && !v.Paused && v.Reason != reasonNew {
				msg.Red = append(msg.Red, v)
			}
		}
		if _, ok := lastSent[name]; !ok {
			lastSent[name] = now
		}
		reminder := len(msg.Changes) == 0 && len(msg.Red) > 0 && nc.ReminderInterval > 0 && now.Sub(lastSent[name]) >= nc.ReminderInterval.D()
		if len(msg.Changes) == 0 && !reminder {
			continue
		}
		err := notifierTypes[nc.Type].send(a, nc, msg)
		if err == nil {
			for m, state := range sent {
				err = errors.Join(err, m.setReported(name, state))
			}
		}
		if err != nil {
			slog.Error("notification failed", "notifier", name, "err", err)
			a.mu.Lock()
			f, ok := a.failing[name]
			if !ok {
				f = failure{Notifier: name, Since: now}
			}
			f.Err = err.Error()
			a.failing[name] = f
			a.mu.Unlock()
			continue
		}
		slog.Info("notification sent", "notifier", name, "changes", len(msg.Changes), "red", len(msg.Red))
		lastSent[name] = now
		a.mu.Lock()
		delete(a.failing, name)
		a.mu.Unlock()
	}
}

type failure struct {
	Notifier, Err string
	Since         time.Time
}

func (a *App) httpSend(method, url string, headers map[string]string, body []byte) error {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return errors.New(errText(err))
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Transport: a.transport(false), Timeout: 30 * time.Second, CheckRedirect: redirectPolicy(10, true)}).Do(req)
	if err != nil {
		return errors.New(errText(err))
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s", method, req.URL.Host, resp.Status)
	}
	return nil
}
