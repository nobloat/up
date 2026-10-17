package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"
)

type webhookNotifier struct{}

type webhookField struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short"`
}

type webhookAttachment struct {
	Fallback  string         `json:"fallback"`
	Color     string         `json:"color"`
	Title     string         `json:"title"`
	TitleLink string         `json:"title_link"`
	Fields    []webhookField `json:"fields"`
}

var stateColors = map[string]string{green: "#22c55e", yellow: "#eab308", red: "#ef4444"}

func (webhookNotifier) validate(n *NotifierConfig) error { return checkURL(n.URL) }

func (webhookNotifier) send(a *App, n *NotifierConfig, msg message) error {
	attach := func(v monitorView, state, reason string) webhookAttachment {
		return webhookAttachment{
			Fallback: v.Name + ": " + state + " (" + reason + ") " + v.URL, Color: stateColors[v.State],
			Title: v.Name, TitleLink: v.URL,
			Fields: []webhookField{
				{"State", state, true},
				{"Since", v.Since.UTC().Format(time.RFC3339), true},
				{"Reason", reason, false},
				{"Type", v.Type, true},
				{"Tags", strings.Join(v.Tags, ", "), true},
			},
		}
	}
	var atts []webhookAttachment
	for _, c := range msg.Changes {
		i := slices.IndexFunc(msg.All, func(v monitorView) bool { return v.Name == c.Name })
		atts = append(atts, attach(msg.All[i], dot(c.Old)+" → "+dot(c.New), c.Reason))
	}
	if len(msg.Changes) == 0 {
		for _, v := range msg.Red {
			atts = append(atts, attach(v, dot(v.State), v.Reason))
		}
	}
	icon := "green"
	if len(msg.Changes) == 0 || slices.ContainsFunc(msg.Changes, func(c change) bool { return c.New == red }) {
		icon = "red"
	}
	body, _ := json.Marshal(map[string]any{"text": "up: " + msg.subject(), "attachments": atts,
		"username": "up", "icon_url": a.cfg.Load().BaseURL + "/static/icon-" + icon + ".png"})
	return a.httpSend(http.MethodPost, n.URL, n.Headers, body)
}
