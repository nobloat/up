package main

import (
	"bytes"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

var (
	staticFS, _ = fs.Sub(webFS, "web/static")
	funcs       = template.FuncMap{"version": version, "favicon": favicon, "rel": rel, "linkify": linkify}
	pages       = map[string]*template.Template{
		"list":    template.Must(template.New("").Funcs(funcs).ParseFS(webFS, "web/layout.html", "web/list.html", "web/footer.html")),
		"monitor": template.Must(template.New("").Funcs(funcs).ParseFS(webFS, "web/layout.html", "web/monitor.html", "web/footer.html")),
	}
	statusTmpl = template.Must(template.New("status.html").Funcs(funcs).ParseFS(webFS, "web/status.html", "web/footer.html"))
)

func (a *App) handler() http.Handler {
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.Handler { return a.auth(h) }
	mux.Handle("GET /{$}", auth(a.ui))
	mux.Handle("GET /monitor/{name...}", auth(a.ui))
	mux.Handle("GET /events", auth(a.events))
	mux.Handle("GET /api/monitors", auth(a.apiList))
	mux.Handle("GET /api/monitors/{name...}", auth(a.apiMonitor))
	mux.Handle("POST /api/monitors/{path...}", auth(a.apiAction))
	mux.HandleFunc("/push/{path...}", a.handlePush)
	mux.HandleFunc("/api/push/{token}", a.handleKumaPush)
	mux.HandleFunc("/ping/{token}", a.handleHealthchecksPing)
	mux.HandleFunc("/ping/{token}/{signal}", a.handleHealthchecksPing)
	mux.HandleFunc("GET /status/{slug}", a.statusPage)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticFS)))
	return http.NewCrossOriginProtection().Handler(mux)
}

func (a *App) auth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := a.cfg.Load().Auth
		u, p, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(c.User))&subtle.ConstantTimeCompare([]byte(p), []byte(c.Password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="][• up", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (a *App) views(tags []string, state, q, name string, withUptime bool) []monitorView {
	cfg := a.cfg.Load()
	var out []monitorView
	for i, mc := range cfg.Monitors {
		m := a.monitor(mc.Name)
		if m == nil {
			continue
		}
		v := m.view()
		v.Pos = i
		if !matches(v, tags, state, q) || name != "" && v.Name != name {
			continue
		}
		if withUptime {
			v.Uptime = a.uptime(m)
		}
		out = append(out, v)
	}
	return out
}

func matches(v monitorView, tags []string, state, q string) bool {
	return !slices.ContainsFunc(tags, func(t string) bool { return !slices.Contains(v.Tags, t) }) &&
		(state == "" || state == "paused" && v.Paused || state != "paused" && v.State == state) &&
		(q == "" || strings.Contains(v.Name, strings.ToLower(q)))
}

func filterViews(vs []monitorView, tags []string, state, q string) []monitorView {
	return slices.DeleteFunc(slices.Clone(vs), func(v monitorView) bool { return !matches(v, tags, state, q) })
}

func (a *App) uptime(m *Monitor) *uptime {
	rows, err := readLog(m.path)
	if err != nil {
		slog.Error("reading audit log", "monitor", m.name, "err", err)
	}
	u := computeUptime(rows, time.Now())
	return &u
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (a *App) apiList(w http.ResponseWriter, r *http.Request) {
	vs := a.views(r.URL.Query()["tag"], "", "", r.FormValue("name"), true)
	for i := range vs {
		vs[i].LastCheck = nil
	}
	if vs == nil {
		vs = []monitorView{}
	}
	writeJSON(w, http.StatusOK, vs)
}

func (a *App) apiMonitor(w http.ResponseWriter, r *http.Request) {
	m := a.monitor(r.PathValue("name"))
	if m == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	v := m.view()
	v.Uptime = a.uptime(m)
	writeJSON(w, http.StatusOK, v)
}

func (a *App) apiAction(w http.ResponseWriter, r *http.Request) {
	name, action := splitLast(r.PathValue("path"))
	m := a.monitor(name)
	if m == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	form := ct == "application/x-www-form-urlencoded" || ct == "multipart/form-data"
	switch action {
	case "check":
		if m.config().Type == "push" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "push monitors are checked by their pushes"})
			return
		}
		m.manualCheck()
	case "pause":
		body := struct {
			Reason string `json:"reason"`
			For    string `json:"for"`
			Until  string `json:"until"`
		}{r.FormValue("reason"), r.FormValue("for"), r.FormValue("until")}
		if ct == "application/json" {
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil && err != io.EOF {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		}
		until, err := pauseEnd(body.For, body.Until)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		m.setPaused(true, body.Reason, until)
	case "resume":
		m.setPaused(false, "", time.Time{})
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if form {
		back := "/"
		if u, err := url.Parse(r.Referer()); err == nil && u.Path != "" {
			back = u.RequestURI()
		}
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	writeJSON(w, http.StatusOK, m.view())
}

func pauseEnd(d, t string) (time.Time, error) {
	switch {
	case d != "" && t != "":
		return time.Time{}, errors.New("give either for or until, not both")
	case d != "":
		dur, err := parseDuration(d)
		if err != nil || dur <= 0 {
			return time.Time{}, fmt.Errorf("for: want a positive duration like 30m, 2h or 7d, got %q", d)
		}
		return time.Now().Add(dur), nil
	case t != "":
		end, err := time.Parse(time.RFC3339, t)
		if err != nil {
			return time.Time{}, fmt.Errorf("until: want an RFC3339 time like 2026-01-02T15:04:05Z, got %q", t)
		}
		if !end.After(time.Now()) {
			return time.Time{}, fmt.Errorf("until: %s is in the past", t)
		}
		return end, nil
	}
	return time.Time{}, nil
}

func splitLast(p string) (string, string) {
	dir, last := path.Split(p)
	return strings.TrimSuffix(dir, "/"), last
}

type page struct {
	Red         int
	Q, State    string
	Tag         []string
	Tags        []string
	Monitors    []monitorView
	M           *monitorView
	Log         []row
	Cfg         *MonitorConfig
	StatusPages []*StatusPageConfig
	Overall     string
	Total       int
	States      []stateChip
	Failing     []failure
}

var urlRe = regexp.MustCompile(`https?://[^\s<>"']*[^\s<>"'.,:;!?)\]]`)

func linkify(s string) template.HTML {
	var b strings.Builder
	last := 0
	for _, m := range urlRe.FindAllStringIndex(s, -1) {
		u := template.HTMLEscapeString(s[m[0]:m[1]])
		b.WriteString(template.HTMLEscapeString(s[last:m[0]]) + `<a href="` + u + `" rel="noreferrer">` + u + `</a>`)
		last = m[1]
	}
	b.WriteString(template.HTMLEscapeString(s[last:]))
	return template.HTML(b.String())
}

func rel(t time.Time, next bool) string {
	d := time.Until(t).Round(time.Second)
	short := func(d time.Duration) string {
		switch {
		case d < time.Minute:
			return fmt.Sprintf("%ds", int(d.Seconds()))
		case d < time.Hour:
			return fmt.Sprintf("%dm", int(d.Minutes()))
		case d < 48*time.Hour:
			return fmt.Sprintf("%dh", int(d.Hours()))
		}
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	switch {
	case d == 0 || d < 0 && next:
		return "now"
	case d < 0:
		return "−" + short(-d)
	}
	return "+" + short(d)
}

func favicon(state string) string {
	if state == green || state == "" {
		return "/static/favicon.svg"
	}
	return "/static/favicon-" + state + ".svg"
}

func worst(vs []monitorView) string {
	w := green
	for _, v := range vs {
		if !v.Paused && (v.State == red || v.State == yellow && w == green) {
			w = v.State
		}
	}
	return w
}

type rowData struct {
	monitorView
	Page page
}

func (p page) Row(v monitorView) rowData { return rowData{v, p} }

func (p page) Config() []kv { return configRows(p.Cfg) }

type kv struct{ Key, Value string }

func (k kv) IsURL() bool { return checkURL(k.Value) == nil }

const masked = "••••••"

func configRows(c *MonitorConfig) []kv {
	var block any
	switch c.Type {
	case "http":
		block = maskHTTP(*c.HTTP)
	case "metric":
		mc := *c.Metric
		mc.HTTPConfig = *maskHTTP(mc.HTTPConfig)
		block = &mc
	case "promql":
		pc := *c.PromQL
		pc.HTTPConfig = *maskHTTP(pc.HTTPConfig)
		pc.Method = ""
		block = &pc
	case "push":
		block = &PushConfig{Token: masked}
	}
	b, _ := json.Marshal(block)
	var out []kv
	flatten(json.NewDecoder(bytes.NewReader(b)), "", &out)
	out = slices.DeleteFunc(out, func(r kv) bool {
		return r.Key == "method" && r.Value == "GET" || r.Key == "max_redirects" && r.Value == "10"
	})
	return out
}

func secretName(k string) bool {
	k = strings.ToLower(k)
	return slices.ContainsFunc([]string{"auth", "token", "key", "secret", "cookie", "pass", "sig"}, func(s string) bool { return strings.Contains(k, s) })
}

func maskHTTP(h HTTPConfig) *HTTPConfig {
	if h.BasicAuth != nil {
		h.BasicAuth = &Credentials{h.BasicAuth.User, masked}
	}
	h.Headers = maps.Clone(h.Headers)
	for k := range h.Headers {
		if secretName(k) {
			h.Headers[k] = masked
		}
	}
	if secretName(h.Body) {
		h.Body = masked
	}
	h.URL = maskURL(h.URL)
	return &h
}

func maskURL(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return masked
	}
	q := strings.Split(u.RawQuery, "&")
	for i, p := range q {
		if k, _, ok := strings.Cut(p, "="); ok && secretName(k) {
			q[i] = k + "=" + masked
		}
	}
	u.RawQuery = strings.Join(q, "&")
	_, pw := u.User.Password()
	if pw {
		u.User = url.User(u.User.Username())
	}
	if s = u.String(); pw {
		s = strings.Replace(s, "@", ":"+masked+"@", 1)
	}
	return s
}

func flatten(dec *json.Decoder, key string, out *[]kv) {
	t, _ := dec.Token()
	switch t {
	case json.Delim('{'):
		for dec.More() {
			k, _ := dec.Token()
			flatten(dec, strings.TrimPrefix(key+"."+k.(string), "."), out)
		}
		dec.Token()
	case json.Delim('['):
		var vals []string
		for dec.More() {
			var v json.RawMessage
			dec.Decode(&v)
			vals = append(vals, string(v))
		}
		dec.Token()
		*out = append(*out, kv{key, "[" + strings.Join(vals, ", ") + "]"})
	default:
		*out = append(*out, kv{key, fmt.Sprint(t)})
	}
}

type tagLink struct {
	Name, URL string
	On        bool
}

func (p page) PagesFor(tags []string) []*StatusPageConfig {
	return slices.DeleteFunc(slices.Clone(p.StatusPages), func(sp *StatusPageConfig) bool { return !slices.Contains(tags, sp.Tag) })
}

func (p page) TagLinks(tags []string) []tagLink {
	var out []tagLink
	for _, t := range tags {
		out = append(out, tagLink{t, p.Link("tag", t), slices.Contains(p.Tag, t)})
	}
	return out
}

type stateChip struct {
	State string
	N     int
}

func (p page) Link(key, val string) string {
	v := url.Values{"tag": p.Tag}
	for k, cur := range map[string]string{"q": p.Q, "state": p.State} {
		if cur != "" {
			v.Set(k, cur)
		}
	}
	switch {
	case key == "tag" && slices.Contains(v[key], val):
		v[key] = slices.DeleteFunc(slices.Clone(v[key]), func(t string) bool { return t == val })
	case key == "tag":
		v.Add(key, val)
	case val == "" || v.Get(key) == val:
		v.Del(key)
	default:
		v.Set(key, val)
	}
	if len(v["tag"]) == 0 {
		v.Del("tag")
	}
	if len(v) == 0 {
		return "/"
	}
	return "/?" + v.Encode()
}

func (a *App) page(q url.Values, vs []monitorView) page {
	p := page{Q: q.Get("q"), State: q.Get("state"), Tag: q["tag"], StatusPages: a.cfg.Load().StatusPages}
	tags := map[string]bool{}
	p.Overall = worst(vs)
	for _, v := range vs {
		if v.State == red && !v.Paused {
			p.Red++
		}
		for _, t := range v.Tags {
			tags[t] = true
		}
	}
	p.Tags = slices.Sorted(maps.Keys(tags))
	a.mu.Lock()
	for _, n := range slices.Sorted(maps.Keys(a.failing)) {
		if a.cfg.Load().Notifiers[n] != nil {
			p.Failing = append(p.Failing, a.failing[n])
		}
	}
	a.mu.Unlock()
	return p
}

func render(w http.ResponseWriter, name string, p page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages[name].ExecuteTemplate(w, "layout", p); err != nil {
		slog.Error("render", "page", name, "err", err)
	}
}

func (a *App) ui(w http.ResponseWriter, r *http.Request) {
	name, p := a.uiPage(r.URL.Path, r.URL.Query())
	if name == "" {
		http.NotFound(w, r)
		return
	}
	render(w, name, p)
}

func (a *App) uiPage(path string, q url.Values) (name string, p page) {
	vs := a.views(nil, "", "", "", false)
	p = a.page(q, vs)
	if path == "/" {
		p.States = a.chips(vs, p.Tag, p.Q, &p.Total)
		p.Monitors = filterViews(vs, p.Tag, p.State, p.Q)
		return "list", p
	}
	mn, ok := strings.CutPrefix(path, "/monitor/")
	m := a.monitor(mn)
	if !ok || m == nil {
		return "", p
	}
	v := m.view()
	v.Uptime = a.uptime(m)
	p.M = &v
	p.Cfg = m.config()
	rows, _ := readLog(m.path)
	slices.Reverse(rows)
	p.Log = rows[:min(len(rows), 200)]
	return "monitor", p
}

func (a *App) chips(vs []monitorView, tags []string, q string, total *int) []stateChip {
	n := map[string]int{}
	for _, v := range filterViews(vs, tags, "", q) {
		n[v.State]++
		if v.Paused {
			n["paused"]++
		}
		*total++
	}
	return []stateChip{{green, n[green]}, {yellow, n[yellow]}, {red, n[red]}, {"paused", n["paused"]}}
}

func (a *App) events(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	path, tags, search := q.Get("path"), q["tag"], q.Get("q")
	detail, isDetail := strings.CutPrefix(path, "/monitor/")
	shows := func(name string) bool {
		if path == "" || isDetail {
			return path == "" || name == detail
		}
		m := a.monitor(name)
		return m != nil && matches(m.view(), tags, "", search)
	}
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Time{})
	rc.SetWriteDeadline(time.Time{})
	s := a.subscribe()
	defer a.unsubscribe(s)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	var b bytes.Buffer
	send := func(typ, data string) {
		if typ != "" {
			b.WriteString("event: " + typ + "\n")
		}
		b.WriteString("data: " + sseLines.Replace(data) + "\n\n")
	}
	tmpl := func(page, name string, data any) string {
		var out strings.Builder
		if err := pages[page].ExecuteTemplate(&out, name, data); err != nil {
			slog.Error("render", "page", page, "err", err)
		}
		return out.String()
	}
	shown := map[string]bool{}
	if path != "" {
		if name, p := a.uiPage(path, q); name != "" {
			send("main", tmpl(name, "main", p))
			for _, v := range p.Monitors {
				shown[v.Name] = true
			}
		}
	}
	for {
		_, err := w.Write(b.Bytes())
		b.Reset()
		if err != nil || rc.Flush() != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-a.ctx.Done():
			return
		case <-s.wake:
			evs, dirty := s.take()
			for _, e := range evs {
				if shows(e.Monitor) && (path == "" || e.Important) {
					j, _ := json.Marshal(e)
					send("", string(j))
				}
			}
			switch {
			case isDetail && dirty[detail]:
				if name, p := a.uiPage(path, q); name != "" {
					send("main", tmpl(name, "main", p))
				}
			case path == "/":
				vs := a.views(nil, "", "", "", false)
				p, changed := a.page(q, vs), false
				for _, v := range filterViews(vs, tags, "", search) {
					if on := matches(v, nil, p.State, ""); dirty[v.Name] && (on || shown[v.Name]) {
						html := ""
						if on {
							html = tmpl("list", "row", p.Row(v))
						}
						shown[v.Name] = on
						row, _ := json.Marshal(map[string]string{"name": v.Name, "html": html})
						send("row", string(row))
						changed = true
					}
				}
				if changed {
					p.States = a.chips(vs, p.Tag, p.Q, &p.Total)
					head, _ := json.Marshal(map[string]any{"red": p.Red, "state": p.Overall, "html": tmpl("list", "chips", p)})
					send("head", string(head))
				}
			}
		case <-time.After(30 * time.Second):
			b.WriteString(": ping\n\n")
		}
	}
}

var sseLines = strings.NewReplacer("\r\n", "\ndata: ", "\r", "\ndata: ", "\n", "\ndata: ")

type StatusPage struct {
	Title    string
	State    string
	Monitors []StatusMonitor
}

type StatusMonitor struct {
	Name        string
	Description string
	State       string
	Paused      bool
	PauseReason string
}

func (a *App) statusPage(w http.ResponseWriter, r *http.Request) {
	cfg := a.cfg.Load()
	i := slices.IndexFunc(cfg.StatusPages, func(p *StatusPageConfig) bool { return p.Slug == r.PathValue("slug") })
	if i < 0 {
		http.NotFound(w, r)
		return
	}
	sp := cfg.StatusPages[i]
	data := StatusPage{Title: sp.Title}
	vs := a.views([]string{sp.Tag}, "", "", "", false)
	data.State = worst(vs)
	for _, v := range vs {
		data.Monitors = append(data.Monitors, StatusMonitor{v.Name, v.Description, v.State, v.Paused, v.PauseReason})
	}
	t := statusTmpl
	if sp.tmpl != nil {
		t = sp.tmpl
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.Execute(w, data); err != nil {
		slog.Error("render status page", "slug", sp.Slug, "err", err)
	}
}
