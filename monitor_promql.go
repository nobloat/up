package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
)

type PromQLConfig struct {
	HTTPConfig
	Query string    `json:"query"`
	Value Threshold `json:"value"`
}

type promqlMonitor struct{}

func (promqlMonitor) validate(c *MonitorConfig) error {
	pc := c.PromQL
	if pc.Method != "" && pc.Method != "GET" {
		return errors.New("promql always uses GET, remove method")
	}
	if pc.Query == "" || pc.Value.Op == "" {
		return errors.New(`promql needs a query and value (e.g. "< 80")`)
	}
	return pc.HTTPConfig.validate()
}

func (promqlMonitor) run(ctx context.Context, m *Monitor) {
	runChecks(ctx, m, func(ctx context.Context, cfg *MonitorConfig) (bool, bool, string, []sample) {
		pc := cfg.PromQL
		h := pc.HTTPConfig
		u, err := url.JoinPath(h.URL, "api/v1/query")
		if err != nil {
			return false, false, errText(err), nil
		}
		h.URL = u + "?query=" + url.QueryEscape(pc.Query)
		ok, fatal, reason, body := check(ctx, m.app, &h, cfg.Timeout.D())
		if !ok {
			return ok, fatal, reason, nil
		}
		samples, err := parseQueryResult(body)
		if err != nil {
			return false, false, err.Error(), nil
		}
		return compare("", samples, pc.Value)
	})
}

func parseQueryResult(body []byte) ([]sample, error) {
	var resp struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("invalid response: %w", err)
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("query failed: %s", resp.Error)
	}
	var vector []struct {
		Metric map[string]string `json:"metric"`
		Value  [2]any            `json:"value"`
	}
	switch resp.Data.ResultType {
	case "scalar":
		resp.Data.Result = fmt.Appendf(nil, `[{"value": %s}]`, resp.Data.Result)
	case "vector":
	default:
		return nil, fmt.Errorf("result type %q, want vector or scalar", resp.Data.ResultType)
	}
	if err := json.Unmarshal(resp.Data.Result, &vector); err != nil {
		return nil, fmt.Errorf("invalid result: %w", err)
	}
	var out []sample
	for _, r := range vector {
		s, _ := r.Value[1].(string)
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid value %v", r.Value[1])
		}
		name := r.Metric["__name__"]
		delete(r.Metric, "__name__")
		out = append(out, sample{Name: name, Labels: r.Metric, Value: num(v)})
	}
	return out, nil
}
