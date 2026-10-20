package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"net/http"
)

type ilertNotifier struct{}

func (ilertNotifier) validate(n *NotifierConfig) error {
	if n.IntegrationKey == "" {
		return errors.New("integration_key is required")
	}
	if n.ReminderInterval != 0 {
		return errors.New("reminder_interval is not supported, ilert escalates on its own")
	}
	n.URL = cmp.Or(n.URL, "https://api.ilert.com/api/events")
	return checkURL(n.URL)
}

func (ilertNotifier) send(a *App, n *NotifierConfig, msg message) error {
	var errs []error
	for _, c := range msg.Changes {
		event, summary := "ALERT", c.Name+" is down: "+c.Reason
		if c.New != red {
			if c.Old != red {
				continue
			}
			event, summary = "RESOLVE", c.Name+" recovered: "+c.Reason
		}
		body, _ := json.Marshal(map[string]any{"integrationKey": n.IntegrationKey, "eventType": event, "alertKey": c.Name,
			"summary": summary, "details": c.Old + " -> " + c.New + " (" + c.Reason + ")",
			"links": []map[string]string{{"href": c.URL, "text": "up: " + c.Name}}})
		errs = append(errs, a.httpSend(http.MethodPost, n.URL, n.Headers, body))
	}
	return errors.Join(errs...)
}
