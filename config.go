package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const reloadInterval = 5 * time.Second

var (
	nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*(/[a-z0-9][a-z0-9.-]*)*$`)
	slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)
)

type Config struct {
	Listen             string                     `json:"listen"`
	BaseURL            string                     `json:"base_url"`
	DataDir            string                     `json:"data_dir"`
	Auth               Credentials                `json:"auth"`
	Proxy              string                     `json:"proxy"`
	EvaluationInterval Duration                   `json:"evaluation_interval"`
	Heartbeat          *Heartbeat                 `json:"heartbeat"`
	Notifiers          map[string]*NotifierConfig `json:"-"`
	Monitors           []*MonitorConfig           `json:"-"`
	StatusPages        []*StatusPageConfig        `json:"-"`

	proxy *url.URL
	errs  []error
}

type Credentials struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

type Heartbeat struct {
	URLs     []string `json:"urls"`
	Interval Duration `json:"interval"`
}

type NotifierConfig struct {
	Type             string            `json:"type"`
	URL              string            `json:"url"`
	Headers          map[string]string `json:"headers"`
	Host             string            `json:"host"`
	Port             int               `json:"port"`
	TLS              string            `json:"tls"`
	User             string            `json:"user"`
	Password         string            `json:"password"`
	From             string            `json:"from"`
	To               []string          `json:"to"`
	SubjectPrefix    string            `json:"subject_prefix"`
	IntegrationKey   string            `json:"integration_key"`
	ReminderInterval Duration          `json:"reminder_interval"`
}

type MonitorConfig struct {
	Name          string        `json:"-"`
	Type          string        `json:"type"`
	Description   string        `json:"description,omitempty"`
	Tags          []string      `json:"tags,omitempty"`
	Interval      Duration      `json:"interval"`
	RetryCount    int           `json:"retry_count"`
	RetryInterval Duration      `json:"retry_interval"`
	Timeout       Duration      `json:"timeout"`
	Grace         Duration      `json:"grace,omitempty"`
	Notifiers     []string      `json:"notifiers,omitempty"`
	HTTP          *HTTPConfig   `json:"http,omitempty"`
	Push          *PushConfig   `json:"push,omitempty"`
	Metric        *MetricConfig `json:"metric,omitempty"`
	PromQL        *PromQLConfig `json:"promql,omitempty"`
}

type StatusPageConfig struct {
	Slug     string `json:"-"`
	Title    string `json:"title"`
	Tag      string `json:"tag"`
	Template string `json:"template"`

	tmpl *template.Template
}

type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := parseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func parseDuration(s string) (time.Duration, error) {
	days, rest, ok := strings.Cut(s, "d")
	n, err := strconv.Atoi(days)
	if !ok || err != nil || n < 0 {
		return time.ParseDuration(s)
	}
	d := time.Duration(n) * 24 * time.Hour
	if rest == "" {
		return d, nil
	}
	r, err := time.ParseDuration(rest)
	if err != nil || r < 0 {
		return 0, fmt.Errorf("time: invalid duration %q", s)
	}
	return d + r, nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d Duration) String() string {
	s := d.D().String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

func (d Duration) D() time.Duration { return time.Duration(d) }

func set(v reflect.Value, key, val string) error {
	head, rest, _ := strings.Cut(key, ".")
	f, ok := field(v, head)
	if !ok {
		return fmt.Errorf("unknown key %q", key)
	}
	if rest == "" {
		return leaf(f, val)
	}
	if f.Kind() == reflect.Pointer {
		if f.IsNil() {
			f.Set(reflect.New(f.Type().Elem()))
		}
		f = f.Elem()
	}
	switch f.Kind() {
	case reflect.Struct:
		return set(f, rest, val)
	case reflect.Map:
		if f.IsNil() {
			f.Set(reflect.MakeMap(f.Type()))
		}
		e := reflect.New(f.Type().Elem()).Elem()
		if err := leaf(e, val); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		f.SetMapIndex(reflect.ValueOf(rest), e)
		return nil
	}
	return fmt.Errorf("unknown key %q", key)
}

func field(v reflect.Value, name string) (reflect.Value, bool) {
	for i := range v.NumField() {
		sf := v.Type().Field(i)
		if sf.Anonymous {
			if f, ok := field(v.Field(i), name); ok {
				return f, true
			}
			continue
		}
		if tag, _, _ := strings.Cut(sf.Tag.Get("json"), ","); tag == name && sf.IsExported() {
			return v.Field(i), true
		}
	}
	return reflect.Value{}, false
}

func leaf(f reflect.Value, val string) error {
	if f.Kind() == reflect.Slice && f.Type() != reflect.TypeFor[json.RawMessage]() {
		s := reflect.MakeSlice(f.Type(), 0, 0)
		for item := range strings.SplitSeq(val, ",") {
			e := reflect.New(f.Type().Elem()).Elem()
			if err := leaf(e, strings.TrimSpace(item)); err != nil {
				return err
			}
			s = reflect.Append(s, e)
		}
		f.Set(s)
		return nil
	}
	if json.Unmarshal([]byte(val), f.Addr().Interface()) == nil {
		return nil
	}
	q, _ := json.Marshal(val)
	if err := json.Unmarshal(q, f.Addr().Interface()); err != nil {
		if _, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
			return fmt.Errorf("invalid value %q", val)
		}
		return err
	}
	return nil
}

func readConf(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	kv := map[string]string{}
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if _, dup := kv[k]; !ok || k == "" || dup {
			return nil, fmt.Errorf("%s:%d: want a unique key = value", path, i+1)
		}
		kv[k] = strings.TrimSpace(v)
	}
	return kv, nil
}

func decodeConf(path string, v any, fallback func() any) error {
	kv, err := readConf(path)
	if err != nil {
		return err
	}
	rv := reflect.ValueOf(v).Elem()
	if t, ok := kv["type"]; ok {
		if err := set(rv, "type", t); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(kv)) {
		err := set(rv, k, kv[k])
		if err != nil && fallback != nil {
			if fb := fallback(); fb != nil {
				err = set(reflect.ValueOf(fb).Elem(), k, kv[k])
			}
		}
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}

func entities(dir string, errs *[]error) (map[string]map[string]string, error) {
	files := map[string]map[string]string{"monitor": {}, "notifier": {}, "page": {}}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".conf") {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if rel == "up.conf" {
			return nil
		}
		base := strings.TrimSuffix(rel, ".conf")
		i := strings.LastIndex(base, ".")
		name, kind := base[:max(i, 0)], base[i+1:]
		if files[kind] == nil {
			*errs = append(*errs, fmt.Errorf("%s: want <name>.monitor.conf, <name>.notifier.conf or <slug>.page.conf", rel))
			return nil
		}
		files[kind][name] = p
		return nil
	})
	return files, err
}

func loadConfig(dir string) (*Config, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	c := Config{Notifiers: map[string]*NotifierConfig{}}
	if err := decodeConf(filepath.Join(dir, "up.conf"), &c, nil); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	all, err := entities(dir, &c.errs)
	if err != nil {
		return nil, err
	}
	skip := func(err error) { c.errs = append(c.errs, err) }
	for _, name := range slices.Sorted(maps.Keys(all["notifier"])) {
		n := &NotifierConfig{}
		if err := decodeConf(all["notifier"][name], n, nil); err != nil {
			skip(err)
			continue
		}
		if err := n.validate(); err != nil {
			skip(fmt.Errorf("notifier %s: %w", name, err))
			continue
		}
		c.Notifiers[name] = n
	}
	files := all["monitor"]
	tokens := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		m := &MonitorConfig{Name: name}
		block := func() any {
			if f, ok := field(reflect.ValueOf(m).Elem(), m.Type); ok && f.Kind() == reflect.Pointer && f.Type().Elem().Kind() == reflect.Struct {
				if f.IsNil() {
					f.Set(reflect.New(f.Type().Elem()))
				}
				return f.Interface()
			}
			return nil
		}
		if err := decodeConf(files[name], m, block); err != nil {
			skip(err)
			continue
		}
		block()
		if !nameRe.MatchString(name) {
			skip(fmt.Errorf("monitor name %q: segments separated by / must match [a-z0-9][a-z0-9.-]*", name))
			continue
		}
		if err := m.validate(); err != nil {
			skip(fmt.Errorf("monitor %s: %w", name, err))
			continue
		}
		if m.Push != nil {
			if tokens[m.Push.Token] {
				skip(fmt.Errorf("monitor %s: push.token must be unique", name))
				continue
			}
			tokens[m.Push.Token] = true
		}
		m.Notifiers = slices.DeleteFunc(m.Notifiers, func(n string) bool {
			if c.Notifiers[n] == nil {
				skip(fmt.Errorf("monitor %s: unknown notifier %q, not notifying it", name, n))
			}
			return c.Notifiers[n] == nil
		})
		c.Monitors = append(c.Monitors, m)
	}
	files = all["page"]
	for _, slug := range slices.Sorted(maps.Keys(files)) {
		p := &StatusPageConfig{Slug: slug}
		if err := decodeConf(files[slug], p, nil); err != nil {
			skip(err)
			continue
		}
		if err := p.validate(dir); err != nil {
			skip(fmt.Errorf("status page %s: %w", slug, err))
			continue
		}
		c.StatusPages = append(c.StatusPages, p)
	}
	return &c, nil
}

func (c *Config) logErrs() {
	for _, err := range c.errs {
		slog.Error("config", "err", err)
	}
}

func (c *Config) validate() error {
	if c.Listen == "" {
		c.Listen = ":7493"
	}
	if c.DataDir == "" {
		return errors.New("data_dir is required")
	}
	if c.Auth.User == "" || c.Auth.Password == "" {
		return errors.New("auth.user and auth.password are required")
	}
	if c.Proxy != "" {
		u, err := url.Parse(c.Proxy)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("invalid proxy %q", c.Proxy)
		}
		c.proxy = u
	}
	if c.EvaluationInterval <= 0 {
		c.EvaluationInterval = Duration(30 * time.Second)
	}
	if hb := c.Heartbeat; hb != nil {
		if len(hb.URLs) == 0 {
			return fmt.Errorf("heartbeat: urls missing")
		}
		for _, u := range hb.URLs {
			if err := checkURL(u); err != nil {
				return fmt.Errorf("heartbeat: %w", err)
			}
		}
		if hb.Interval <= 0 {
			hb.Interval = Duration(time.Minute)
		}
	}
	if err := checkURL(c.BaseURL); err != nil {
		return fmt.Errorf("base_url: %w", err)
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	return nil
}

func (n *NotifierConfig) validate() error {
	t := notifierTypes[n.Type]
	if t == nil {
		return fmt.Errorf("unknown type %q", n.Type)
	}
	return t.validate(n)
}

func (p *StatusPageConfig) validate(dir string) error {
	if !slugRe.MatchString(p.Slug) {
		return fmt.Errorf("slug %q must match [a-z0-9][a-z0-9.-]*", p.Slug)
	}
	if p.Tag == "" {
		return errors.New("tag is required")
	}
	if p.Title == "" {
		p.Title = p.Slug
	}
	if p.Template != "" {
		path := p.Template
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		t, err := template.ParseFiles(path)
		if err != nil {
			return err
		}
		p.tmpl = t
	}
	return nil
}

func (m *MonitorConfig) validate() error {
	if m.Interval <= 0 {
		return errors.New("interval is required")
	}
	if m.RetryCount < 0 {
		return errors.New("retry_count must be >= 0")
	}
	if m.RetryInterval <= 0 {
		m.RetryInterval = m.Interval
	}
	if m.Timeout <= 0 {
		m.Timeout = Duration(10 * time.Second)
	}
	t := monitorTypes[m.Type]
	if t == nil {
		return fmt.Errorf("unknown type %q", m.Type)
	}
	blocks := 0
	for _, set := range []bool{m.HTTP != nil, m.Push != nil, m.Metric != nil, m.PromQL != nil} {
		if set {
			blocks++
		}
	}
	if blocks != 1 {
		return fmt.Errorf("type %q takes only its own options", m.Type)
	}
	if m.Grace != 0 && m.Type != "push" {
		return errors.New("grace is only valid for push monitors")
	}
	return t.validate(m)
}

func checkURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid url %q", s)
	}
	return nil
}

func (a *App) reloadLoop(ctx context.Context) {
	last := a.fingerprint()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(reloadInterval):
		}
		fp := a.fingerprint()
		if fp == last {
			continue
		}
		last = fp
		cfg, err := loadConfig(a.path)
		if err != nil {
			slog.Error("config reload failed, keeping old config", "err", err)
			continue
		}
		cfg.logErrs()
		if err := a.apply(ctx, cfg); err != nil {
			slog.Error("config reload failed, keeping old config", "err", err)
			continue
		}
		slog.Info("config reloaded")
	}
}

func (a *App) fingerprint() string {
	var b strings.Builder
	filepath.WalkDir(a.path, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".conf") {
			if fi, err := d.Info(); err == nil {
				fmt.Fprintf(&b, "%s %d %d\n", p, fi.ModTime().UnixNano(), fi.Size())
			}
		}
		return nil
	})
	return b.String()
}
