package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
)

type MetricConfig struct {
	HTTPConfig
	Metric string            `json:"metric"`
	Labels map[string]string `json:"labels,omitempty"`
	Value  Threshold         `json:"value"`
}

type Threshold struct {
	Op    string
	Value float64
}

func (t *Threshold) UnmarshalText(b []byte) error {
	s := strings.TrimSpace(string(b))
	i := strings.IndexFunc(s, func(r rune) bool { return !strings.ContainsRune("<>=", r) })
	if i < 0 {
		i = len(s)
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s[i:]), 64)
	if metricOps[s[:i]] == nil || err != nil {
		return fmt.Errorf(`value %q: want an op (>, >=, <, <=) and a number, e.g. "< 80"`, s)
	}
	*t = Threshold{s[:i], v}
	return nil
}

func (t Threshold) MarshalText() ([]byte, error) { return []byte(t.String()), nil }
func (t Threshold) String() string               { return fmt.Sprintf("%s %g", t.Op, t.Value) }

var metricOps = map[string]func(a, b float64) bool{
	">":  func(a, b float64) bool { return a > b },
	">=": func(a, b float64) bool { return a >= b },
	"<":  func(a, b float64) bool { return a < b },
	"<=": func(a, b float64) bool { return a <= b },
}

type metricMonitor struct{}

func (metricMonitor) validate(c *MonitorConfig) error {
	mc := c.Metric
	if mc.Metric == "" || mc.Value.Op == "" {
		return errors.New(`metric needs a metric name and value (e.g. "< 80")`)
	}
	return mc.HTTPConfig.validate()
}

func (metricMonitor) run(ctx context.Context, m *Monitor) {
	runChecks(ctx, m, func(ctx context.Context, cfg *MonitorConfig) (bool, bool, string, []sample) {
		mc := cfg.Metric
		ok, fatal, reason, body := check(ctx, m.app, &mc.HTTPConfig, cfg.Timeout.D())
		if !ok {
			return ok, fatal, reason, nil
		}
		return compareMetric(string(body), mc)
	})
}

func compareMetric(body string, mc *MetricConfig) (ok, fatal bool, reason string, samples []sample) {
	for line := range strings.Lines(body) {
		name, labels, val, ok := parseSample(strings.TrimSpace(line))
		if ok && name == mc.Metric && hasLabels(labels, mc.Labels) {
			samples = append(samples, sample{Name: name, Labels: labels, Value: num(val)})
		}
	}
	return compare(selector(mc.Metric, mc.Labels), samples, mc.Value)
}

type sample struct {
	Name   string            `json:"name,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
	Value  num               `json:"value"`
	OK     bool              `json:"ok"`
}

func (s sample) Series() string { return selector(s.Name, s.Labels) }

type num float64

func (n num) MarshalText() ([]byte, error) {
	return strconv.AppendFloat(nil, float64(n), 'g', -1, 64), nil
}

func (n *num) UnmarshalText(b []byte) error {
	f, err := strconv.ParseFloat(string(b), 64)
	*n = num(f)
	return err
}

func (n num) String() string {
	v := float64(n)
	if math.Abs(v) >= 1 {
		return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64)
	}
	return strconv.FormatFloat(v, 'g', 3, 64)
}

func compare(sel string, samples []sample, want Threshold) (ok, fatal bool, reason string, _ []sample) {
	for i, s := range samples {
		samples[i].OK = metricOps[want.Op](float64(s.Value), want.Value)
		if !samples[i].OK && reason == "" {
			reason = fmt.Sprintf("%s is %s, want %s", cmp.Or(s.Series(), "value"), s.Value, want)
		}
	}
	switch {
	case reason != "":
		return false, false, reason, samples
	case len(samples) == 0 && sel == "":
		return false, false, "no series", nil
	case len(samples) == 0:
		return false, false, sel + " not found", nil
	case len(samples) == 1:
		return true, false, fmt.Sprintf("%s is %s (%s)", cmp.Or(sel, "value"), samples[0].Value, want), samples
	case sel == "":
		return true, false, fmt.Sprintf("%d series %s", len(samples), want), samples
	}
	return true, false, fmt.Sprintf("%s: %d series %s", sel, len(samples), want), samples
}

type sampleTable struct {
	Common string
	Rows   []sampleRow
}

type sampleRow struct {
	Labels string
	Value  num
	OK     bool
}

func (r checkResult) Table() *sampleTable {
	if len(r.Samples) == 0 {
		return nil
	}
	first := r.Samples[0]
	name, common := first.Name, maps.Clone(first.Labels)
	for _, s := range r.Samples[1:] {
		if s.Name != name {
			name = ""
		}
		maps.DeleteFunc(common, func(k, v string) bool { return s.Labels[k] != v })
	}
	t := &sampleTable{Common: selector(name, common)}
	for _, s := range r.Samples {
		var l []string
		if name == "" && s.Name != "" {
			l = append(l, s.Name)
		}
		for _, k := range slices.Sorted(maps.Keys(s.Labels)) {
			if _, ok := common[k]; !ok {
				l = append(l, k+"="+strconv.Quote(s.Labels[k]))
			}
		}
		t.Rows = append(t.Rows, sampleRow{strings.Join(l, " "), s.Value, s.OK})
	}
	return t
}

func selector(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	var l []string
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		l = append(l, k+"="+strconv.Quote(labels[k]))
	}
	return name + "{" + strings.Join(l, ",") + "}"
}

func hasLabels(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func parseSample(line string) (name string, labels map[string]string, value float64, ok bool) {
	i := strings.IndexAny(line, "{ \t")
	if i <= 0 || line[0] == '#' {
		return
	}
	name, rest := line[:i], line[i:]
	labels = map[string]string{}
	if rest[0] == '{' {
		rest = rest[1:]
		for {
			rest = strings.TrimLeft(rest, " ,")
			if strings.HasPrefix(rest, "}") {
				rest = rest[1:]
				break
			}
			k, v, found := strings.Cut(rest, "=")
			if !found || !strings.HasPrefix(v, `"`) {
				return
			}
			j := 1
			for ; j < len(v) && v[j] != '"'; j++ {
				if v[j] == '\\' {
					j++
				}
			}
			if j >= len(v) {
				return
			}
			s, err := strconv.Unquote(v[:j+1])
			if err != nil {
				return
			}
			labels[strings.TrimSpace(k)], rest = s, v[j+1:]
		}
	}
	f := strings.Fields(rest)
	if len(f) == 0 {
		return
	}
	value, err := strconv.ParseFloat(f[0], 64)
	return name, labels, value, err == nil
}
