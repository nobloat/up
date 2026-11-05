package main

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
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
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

var (
	staticFS, _ = fs.Sub(webFS, "web/static")
	funcs       = template.FuncMap{"rel": rel, "linkify": linkify, "st": stClass, "evicon": evIcon, "upper": strings.ToUpper, "tsf": func(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05 MST") }}
	// every page: nobloat/ui's partials first, then up's shared parts, then the page's "main"
	pages = func() map[string]*template.Template {
		out := map[string]*template.Template{}
		for _, name := range []string{"list", "monitor", "edit", "pages", "settings", "error"} {
			out[name] = template.Must(template.New("").Funcs(funcs).ParseFS(webFS, "web/partials.html", "web/common.html", "web/"+name+".html"))
		}
		return out
	}()
	statusTmpl = template.Must(template.New("").Funcs(funcs).ParseFS(webFS, "web/partials.html", "web/status.html"))
)

func (a *App) handler() http.Handler {
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.Handler { return a.auth(h) }
	mux.Handle("GET /{$}", auth(a.ui))
	mux.Handle("GET /monitor/{name}", auth(a.ui))
	mux.Handle("GET /pages", auth(a.pagesList))
	mux.Handle("GET /settings", auth(a.settings))
	mux.Handle("GET /new", auth(a.editForm))
	mux.Handle("GET /edit/{kind}/{name}", auth(a.editForm))
	mux.Handle("POST /edit", auth(a.editSave))
	mux.Handle("GET /events", auth(a.events))
	mux.Handle("GET /api/me", auth(a.apiMe))
	mux.Handle("GET /api/monitors", auth(a.apiList))
	mux.Handle("GET /api/monitors/{name}", auth(a.apiMonitor))
	mux.Handle("POST /api/monitors/{name}/{action}", auth(a.apiAction))
	mux.Handle("GET /api/export", auth(a.apiExport))
	mux.HandleFunc("/push/{name}/{token}", a.handlePush)
	mux.HandleFunc("/api/push/{token}", a.handleKumaPush)
	mux.HandleFunc("/ping/{token}", a.handleHealthchecksPing)
	mux.HandleFunc("/ping/{token}/{signal}", a.handleHealthchecksPing)
	mux.HandleFunc("GET /status/{slug}", a.statusPage)
	mux.HandleFunc("GET /lang/{l}", setLang)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticFS)))
	mux.Handle("/", auth(func(w http.ResponseWriter, r *http.Request) { a.errPage(w, r, http.StatusNotFound, "") }))
	return http.NewCrossOriginProtection().Handler(a.recoverer(mux))
}

// visible returns the monitor when u may see it, else nil.
func (a *App) visible(u *User, name string) *Monitor {
	if m := a.monitor(name); m != nil && u.sees(m.config()) {
		return m
	}
	return nil
}

// views lists the monitors u sees; a nil u is the public status page.
func (a *App) views(u *User, tags []string, state, q, name string, withUptime bool) []monitorView {
	cfg := a.cfg.Load()
	var out []monitorView
	for i, mc := range cfg.Monitors {
		m := a.monitor(mc.Name)
		if m == nil || u != nil && !u.sees(mc) {
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
	vs := a.views(userOf(r), r.URL.Query()["tag"], "", "", r.FormValue("name"), true)
	for i := range vs {
		vs[i].LastCheck = nil
	}
	if vs == nil {
		vs = []monitorView{}
	}
	writeJSON(w, http.StatusOK, vs)
}

func (a *App) apiMe(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	tags := u.Tags
	if u.Role >= admin || slices.Contains(tags, "*") {
		tags = []string{"*"}
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": u.Name, "role": u.Role.String(), "tags": append([]string{}, tags...)})
}

func (a *App) apiMonitor(w http.ResponseWriter, r *http.Request) {
	m := a.visible(userOf(r), r.PathValue("name"))
	if m == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	v := m.view()
	v.Uptime = a.uptime(m)
	writeJSON(w, http.StatusOK, v)
}

func (a *App) apiAction(w http.ResponseWriter, r *http.Request) {
	name, action := r.PathValue("name"), r.PathValue("action")
	m := a.visible(userOf(r), name)
	if m == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if !need(w, r, editor) {
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

// page is the value of every UI page: the fields nobloat/ui's partials.html needs (see its header), then up's own.
type page struct {
	Lang, Title, Here, Version string
	Err, Notice                string // flash messages
	Brand                      brand
	Nav                        []navLink
	Crumbs                     []crumb
	Search, SearchAPI          string
	User, UserURL              string
	LogoutURL, SettingsURL     string
	SessionEnd, SessionRenew   string
	Texts                      map[string]string

	U           *User
	AccountURL  string
	Red         int
	Q, State    string
	Tag         []string
	Tags        []string
	Monitors    []monitorView
	M           *monitorView
	Log         []row
	Cfg         *MonitorConfig
	StatusPages []*StatusPageConfig
	PageRows    []pageRow
	Overall     string
	Total       int
	States      []stateChip
	Failing     []failure
	Edit        bool   // may check, pause and resume
	EditKind    string // monitor or page
	EditName    string
	EditPages   []*StatusPageConfig // status pages the user may edit
	EditNew     bool
	Conf, Rev   string
	ConfErr     string
	Templates   []confTemplate
	AllowedTags []string
	AnyTag      bool // AllowedTags doesn't apply
	Code        int  // error pages
	ErrTitle    string
	ErrText     string
	Ref         string
	S           *StatusPage
}

type brand struct{ App, Char, Name, Color, ColorDark, Logo, LogoDark, Icon, Design string }

type navLink struct {
	Href, Label, Key string
	On               bool
}

type crumb struct{ Href, Label string }

type pageRow struct {
	*StatusPageConfig
	State string
	N     int
	Edit  bool
}

func (p page) T(key string, args ...any) string { return T(p.Lang, key, args...) }

// BaseTitle: the title without the failing count, which app.js keeps current.
func (p page) BaseTitle() string { title, _, _ := strings.Cut(p.Title, " ("); return title }

// D: a state light's class and word.
func (p page) D(state string) light {
	return light{stClass(state), p.T("state." + cmp.Or(state, "unknown"))}
}

type light struct{ Class, Label string }

// stClass maps up's states to nobloat/ui's status classes.
func stClass(state string) string {
	return cmp.Or(map[string]string{green: "ok", yellow: "pend", red: "bad"}[state], "off")
}

func evIcon(event string) string {
	return cmp.Or(map[string]string{"paused": "pause", "resumed": "play", "push": "arrow-up", "signal": "send", "check": "refresh", "start": "power", "stop": "x"}[event], "info")
}

// ID: the monitor's ticket key, MON-<NAME>.
func (v monitorView) ID() string { return "MON-" + strings.ToUpper(v.Name) }

// dress fills the fields of the shared layout; title is a text key, args fill it.
func (a *App) dress(p *page, r *http.Request, here, title string, args ...any) {
	cfg := a.cfg.Load()
	p.Lang, p.Here, p.Version, p.Texts = langOf(r), here, version(), jsTexts(langOf(r))
	p.Title = p.T(title, args...)
	if p.Red > 0 {
		p.Title += " (" + p.T("title.failing", p.Red) + ")"
	}
	p.Brand = brand{App: "up", Char: "•", Color: cfg.BrandColor, ColorDark: cfg.BrandColorDark, Icon: favicon(p.Overall)}
	if cfg.Design == "paper" {
		p.Brand.Design = "paper"
	}
	p.Search, p.SettingsURL, p.Q = "/", "/settings", cmp.Or(p.Q, r.FormValue("q"))
	if p.U == nil {
		p.U = userOf(r)
	}
	if p.U != nil && p.U.SSO {
		p.User, p.UserURL, p.LogoutURL, p.AccountURL = p.U.Name, cfg.AccountURL, cfg.LogoutURL, cfg.AccountURL
	}
	pages := strings.HasPrefix(here, "/pages") || strings.HasPrefix(here, "/edit/page/") || strings.HasPrefix(here, "/new?kind=page")
	p.Nav = []navLink{
		{"/", p.T("nav.monitors"), "m", !pages && (here == "/" || strings.HasPrefix(here, "/?") || strings.HasPrefix(here, "/monitor/") || strings.HasPrefix(here, "/edit/") || strings.HasPrefix(here, "/new"))},
		{"/pages", p.T("nav.pages"), "s", pages},
	}
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
		k, _, ok := strings.Cut(p, "=")
		if dk, err := url.QueryUnescape(k); ok && (err != nil || secretName(dk)) { // decoded: the target sees to%6ben as token
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

func (a *App) page(u *User, q url.Values, vs []monitorView) page {
	cfg := a.cfg.Load()
	p := page{Q: q.Get("q"), State: q.Get("state"), Tag: q["tag"], StatusPages: cfg.StatusPages, Edit: u.Role >= editor, U: u}
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
	for _, sp := range cfg.StatusPages {
		if u.Role >= editor && a.mayEdit(u, "page", sp.Slug) {
			p.EditPages = append(p.EditPages, sp)
		}
	}
	a.mu.Lock()
	for _, n := range slices.Sorted(maps.Keys(a.failing)) {
		if u.Role >= admin && a.cfg.Load().Notifiers[n] != nil {
			p.Failing = append(p.Failing, a.failing[n])
		}
	}
	a.mu.Unlock()
	return p
}

func render(w http.ResponseWriter, name string, p page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages[name].ExecuteTemplate(w, "nb-layout", p); err != nil {
		slog.Error("render", "page", name, "err", err)
	}
}

func (a *App) ui(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	if q := strings.ToLower(strings.TrimSpace(r.FormValue("q"))); r.URL.Path == "/" && strings.HasPrefix(q, "mon-") { // an exact id opens the monitor
		if m := a.visible(u, strings.TrimPrefix(q, "mon-")); m != nil {
			http.Redirect(w, r, "/monitor/"+m.name, http.StatusSeeOther)
			return
		}
	}
	name, p := a.uiPage(u, r.URL.Path, r.URL.Query())
	if name == "" {
		a.errPage(w, r, http.StatusNotFound, T(langOf(r), "kind.monitor")+" "+strings.TrimPrefix(r.URL.Path, "/monitor/"))
		return
	}
	a.dressUI(&p, r, r.URL.RequestURI(), name)
	render(w, name, p)
}

// dressUI: title and breadcrumbs of the list and the detail page.
func (a *App) dressUI(p *page, r *http.Request, here, name string) {
	if name == "list" {
		a.dress(p, r, here, "title.monitors")
		return
	}
	a.dress(p, r, here, "title.monitor", p.M.Name)
	p.Crumbs = []crumb{{"/", p.T("nav.monitors")}, {"", p.M.Name}}
}

func (a *App) uiPage(u *User, path string, q url.Values) (name string, p page) {
	vs := a.views(u, nil, "", "", "", false)
	p = a.page(u, q, vs)
	if path == "/" {
		p.States = a.chips(vs, p.Tag, p.Q, &p.Total)
		p.Monitors = filterViews(vs, p.Tag, p.State, p.Q)
		return "list", p
	}
	mn, ok := strings.CutPrefix(path, "/monitor/")
	m := a.visible(u, mn)
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

func (a *App) pagesList(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	p := a.page(u, nil, a.views(u, nil, "", "", "", false))
	for _, sp := range p.StatusPages {
		vs := a.views(nil, []string{sp.Tag}, "", "", "", false)
		p.PageRows = append(p.PageRows, pageRow{sp, worst(vs), len(vs), slices.Contains(p.EditPages, sp)})
	}
	a.dress(&p, r, r.URL.RequestURI(), "title.pages")
	render(w, "pages", p)
}

func (a *App) settings(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	p := a.page(u, nil, a.views(u, nil, "", "", "", false))
	p.AllowedTags, p.AnyTag = userTags(u)
	a.dress(&p, r, r.URL.RequestURI(), "title.settings")
	p.AccountURL, p.LogoutURL = a.cfg.Load().AccountURL, a.cfg.Load().LogoutURL
	render(w, "settings", p)
}

// errPage answers code with a page in the app's layout; what names the missing object of a 404, or the panic of a 500.
func (a *App) errPage(w http.ResponseWriter, r *http.Request, code int, what string) {
	u := userOf(r)
	if u == nil {
		u = &User{}
	}
	p := page{U: u}
	if u.Role > none {
		p = a.page(u, nil, a.views(u, nil, "", "", "", false))
	}
	p.Code = code
	a.dress(&p, r, r.URL.RequestURI(), "err."+strconv.Itoa(code)+".code")
	switch code {
	case http.StatusNotFound:
		p.ErrTitle, p.ErrText = p.T("err.404.title", cmp.Or(what, r.URL.Path)), p.T("err.404.text")
	case http.StatusForbidden:
		p.ErrTitle, p.ErrText = p.T("err.403.title"), p.T("err.403.text", cmp.Or(u.Name, "?"), u.Role)
	default:
		p.Ref = "ERR-" + strings.ToUpper(rand.Text()[:8])
		p.ErrTitle, p.ErrText = p.T("err.500.title"), p.T("err.500.text")
		slog.Error("internal error", "ref", p.Ref, "path", r.URL.Path, "err", what)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := pages["error"].ExecuteTemplate(w, "nb-layout", p); err != nil {
		slog.Error("render", "page", "error", "err", err)
	}
}

// recoverer answers a panic with the 500 page and its reference in the log.
func (a *App) recoverer(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil && v != http.ErrAbortHandler {
				a.errPage(w, r, http.StatusInternalServerError, fmt.Sprint(v))
			}
		}()
		h.ServeHTTP(w, r)
	})
}

// wantsJSON: autosave and API clients, as opposed to a browser posting a form.
func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// needUI is need for pages: below min, a browser gets the 403 page.
func (a *App) needUI(w http.ResponseWriter, r *http.Request, min role) bool {
	if userOf(r).Role >= min || wantsJSON(r) {
		return need(w, r, min)
	}
	a.errPage(w, r, http.StatusForbidden, "")
	return false
}

type confTemplate struct{ Kind, Type, Conf string }

var kindNames = map[string]string{"monitor": "monitor", "page": "status page"}

// confTemplates starts a new file per monitor type and a status page; below admin, tags start as the user's own, since one is required.
func confTemplates(u *User) []confTemplate {
	tags, tag := "# tags = prod, public", "public"
	if allowed, all := userTags(u); !all {
		tags, tag = "tags = "+strings.Join(allowed, ", "), ""
		if len(allowed) > 0 {
			tag = allowed[0]
		}
	}
	common := "# description = what this checks\n" + tags + "\n# notifiers = ops-chat\n# retry_count = 2\n"
	return []confTemplate{
		{"monitor", "http", "type = http\ninterval = 1m\nurl = https://example.com/\n" + common + "# status = 200-299\n# body = regex the body must match\n# cert_min_valid_days = 7\n"},
		{"monitor", "metric", "type = metric\ninterval = 1m\nurl = https://prometheus.example.com/metrics\nmetric = prometheus_ready\nvalue = >= 1\n" + common},
		{"monitor", "promql", "type = promql\ninterval = 5m\nurl = https://prometheus.example.com\nquery = 100 * (1 - avg by (instance) (rate(node_cpu_seconds_total{mode=\"idle\"}[5m])))\nvalue = < 80\n" + common},
		{"monitor", "push", "type = push\ninterval = 24h\ngrace = 1h\ntoken = " + rand.Text() + "\n" + common},
		{"page", "status page", "title = Example Status\ntag = " + tag + "\n# template = minimal.html (a .html file next to up.conf)\n"},
	}
}

var listOf = map[string]string{"monitor": "monitors", "page": "pages"}

func (a *App) editPage(r *http.Request, kind, name string, create bool) page {
	u := userOf(r)
	p := a.page(u, nil, a.views(u, nil, "", "", "", false))
	p.EditKind, p.EditName, p.EditNew, p.Templates = kind, name, create, confTemplates(u)
	p.AllowedTags, p.AnyTag = a.allowedTags(u, kind, name)
	list, back := "/", "/monitor/"+name
	if kind == "page" {
		list, back = "/pages", "/status/"+name
	}
	here := r.URL.RequestURI()
	if r.Method != http.MethodGet {
		here = "/edit/" + kind + "/" + name
		if create {
			here = "/new?kind=" + kind
		}
	}
	if create {
		a.dress(&p, r, here, "title.new."+kind)
		p.Crumbs = []crumb{{list, p.T("nav." + listOf[kind])}, {"", p.Title}}
		return p
	}
	a.dress(&p, r, here, "title.edit."+kind, name)
	p.Crumbs = []crumb{{list, p.T("nav." + listOf[kind])}, {back, name}, {"", p.T("btn.edit")}}
	return p
}

// confRev: the version of a config file, so autosave notices a change made meanwhile.
func confRev(conf string) string {
	h := sha256.Sum256([]byte(conf))
	return hex.EncodeToString(h[:6])
}

func (a *App) editForm(w http.ResponseWriter, r *http.Request) {
	if !a.needUI(w, r, editor) {
		return
	}
	kind, name := cmp.Or(r.PathValue("kind"), r.FormValue("kind"), "monitor"), r.PathValue("name")
	create := name == ""
	if kindNames[kind] == "" || !create && (!slugRe.MatchString(name) || !a.mayEdit(userOf(r), kind, name)) { // before any file access: path values are unescaped, %2F included
		a.errPage(w, r, http.StatusNotFound, T(langOf(r), "kind."+kind)+" "+name)
		return
	}
	if create {
		name = r.FormValue("name") // prefill: /new?kind=monitor&name=api&type=push
	}
	p := a.editPage(r, kind, name, create)
	i := slices.IndexFunc(p.Templates, func(t confTemplate) bool {
		return t.Kind == kind && (kind == "page" || t.Type == cmp.Or(r.FormValue("type"), "http"))
	})
	p.Conf = p.Templates[max(i, 0)].Conf
	if !create {
		b, err := os.ReadFile(a.confFile(kind, name))
		if err != nil {
			a.errPage(w, r, http.StatusNotFound, T(langOf(r), "kind."+kind)+" "+name)
			return
		}
		p.Conf = string(b)
		p.Rev = confRev(p.Conf)
	}
	render(w, "edit", p)
}

func (a *App) editSave(w http.ResponseWriter, r *http.Request) {
	if !a.needUI(w, r, editor) {
		return
	}
	u, kind, name, conf := userOf(r), cmp.Or(r.FormValue("kind"), "monitor"), r.FormValue("name"), strings.ReplaceAll(r.FormValue("conf"), "\r\n", "\n")
	create, del := r.FormValue("new") != "", r.FormValue("delete") != ""
	var err error
	if rev := r.FormValue("rev"); rev != "" && !create && !del && kindNames[kind] != "" && slugRe.MatchString(name) {
		// ponytail: compared before saveConf, outside its lock; a save in between still wins
		if b, rerr := os.ReadFile(a.confFile(kind, name)); rerr == nil && confRev(string(b)) != rev {
			err = errors.New(T(langOf(r), "edit.changed"))
		}
	}
	if err == nil {
		err = a.saveConf(u, kind, name, conf, create, del)
	}
	if err != nil {
		if wantsJSON(r) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "field": "conf"})
			return
		}
		p := a.editPage(r, kind, name, create)
		p.Conf, p.ConfErr, p.Rev = conf, err.Error(), r.FormValue("rev")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		if err := pages["edit"].ExecuteTemplate(w, "nb-layout", p); err != nil {
			slog.Error("render", "page", "edit", "err", err)
		}
		return
	}
	a.reload(a.ctx)
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]string{"rev": confRev(conf)})
		return
	}
	back := map[string]string{"monitor": "/monitor/", "page": "/status/"}[kind] + name
	if del {
		back = map[string]string{"monitor": "/", "page": "/pages"}[kind]
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (a *App) confFile(kind, name string) string {
	return filepath.Join(a.path, name+"."+kind+".conf")
}

// userTags: the tags u is limited to, none at all when u.Tags is empty; all is any tag.
func userTags(u *User) (tags []string, all bool) {
	if u.Role >= admin || slices.Contains(u.Tags, "*") {
		return nil, true
	}
	return u.Tags, false
}

// loadedTags: the tags of the loaded monitor or status page; ok is false when it isn't loaded, e.g. a broken file.
func (a *App) loadedTags(kind, name string) (tags []string, ok bool) {
	if kind == "page" {
		for _, p := range a.cfg.Load().StatusPages {
			if p.Slug == name {
				return []string{p.Tag}, true
			}
		}
		return nil, false
	}
	if m := a.monitor(name); m != nil {
		return m.config().Tags, true
	}
	return nil, false
}

// allowedTags: the tags u may save on name, their own plus the ones it already has; all is any tag.
func (a *App) allowedTags(u *User, kind, name string) (tags []string, all bool) {
	own, all := userTags(u)
	if all {
		return nil, true
	}
	old, _ := a.loadedTags(kind, name)
	tags = slices.Concat(own, old)
	slices.Sort(tags)
	return slices.Compact(tags), false
}

// mayEdit: admins edit every file, editors what shares a tag with them; a file that failed to load has no tags, so only admins.
func (a *App) mayEdit(u *User, kind, name string) bool {
	tags, ok := a.loadedTags(kind, name)
	return u.Role >= admin || u.Role >= editor && ok && u.sees(&MonitorConfig{Tags: tags})
}

// saveConf validates conf like a reload would, then replaces the file atomically; a monitor's audit log stays on delete.
// Below admin, the saved tags must be the user's own or ones the file already had, so nothing moves out of their reach or onto other teams' lists and status pages.
func (a *App) saveConf(u *User, kind, name, conf string, create, del bool) error {
	label := kindNames[kind]
	if label == "" {
		return fmt.Errorf("unknown kind %q", kind)
	}
	if !slugRe.MatchString(name) {
		return fmt.Errorf("name %q must match [a-z0-9][a-z0-9.-]*", name)
	}
	path := a.confFile(kind, name)
	_, err := os.Stat(path)
	exists := err == nil
	switch {
	case create && exists:
		return fmt.Errorf("%s %s already exists", label, name)
	case exists && !a.mayEdit(u, kind, name):
		return fmt.Errorf("%s %s: not found", label, name)
	case del:
		a.store.RLock()
		defer a.store.RUnlock()
		if err := os.Remove(path); err != nil {
			return err
		}
		return syncDir(a.path)
	}
	// atomic write: a synced temp file in the same dir (no .conf suffix: invisible to reloads), validated, renamed over path, dir synced
	f, err := os.CreateTemp(a.path, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.WriteString(conf); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	clean := func(err error) error { return errors.New(strings.ReplaceAll(err.Error(), tmp, filepath.Base(path))) }
	var tags []string
	cfg := a.cfg.Load()
	if kind == "page" {
		p := &StatusPageConfig{Slug: name}
		if err := decodeConf(tmp, p, nil); err != nil {
			return clean(err)
		}
		// the template is rendered publicly: a path could publish any file, up.conf and its passwords included
		if t := p.Template; t != "" && (filepath.Base(t) != t || filepath.Ext(t) != ".html") {
			return errors.New("template: give the name of a .html file next to up.conf")
		}
		if err := p.validate(a.path); err != nil {
			return fmt.Errorf("status page %s: %w", name, err)
		}
		tags = []string{p.Tag}
	} else {
		m, err := parseMonitor(tmp, name)
		if err != nil {
			return clean(err)
		}
		for _, n := range m.Notifiers {
			if cfg.Notifiers[n] == nil {
				return fmt.Errorf("unknown notifier %q", n)
			}
		}
		for _, o := range cfg.Monitors {
			if m.Push != nil && o.Push != nil && o.Name != name && o.Push.Token == m.Push.Token {
				return errors.New("push.token is already used by another monitor")
			}
		}
		tags = m.Tags
	}
	if allowed, all := a.allowedTags(u, kind, name); !all {
		if len(allowed) == 0 {
			return errors.New("tags: you have no access tags, ask an admin")
		}
		if len(tags) == 0 {
			return fmt.Errorf("tags: give at least one of %s", strings.Join(allowed, ", "))
		}
		for _, t := range tags {
			if !slices.Contains(allowed, t) {
				return fmt.Errorf("tag %q is not allowed, use %s", t, strings.Join(allowed, ", "))
			}
		}
	}
	a.store.RLock()
	defer a.store.RUnlock()
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(a.path) // the rename itself survives a power cut
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
	q, u := r.URL.Query(), userOf(r)
	path, tags, search := q.Get("path"), q["tag"], q.Get("q")
	detail, isDetail := strings.CutPrefix(path, "/monitor/")
	vq := maps.Clone(q)
	delete(vq, "path")
	here := path
	if len(vq) > 0 {
		here += "?" + vq.Encode()
	}
	shows := func(name string) bool {
		m := a.visible(u, name)
		if m == nil && u.Role < admin { // a removed monitor's events only reach admins
			return false
		}
		if path == "" || isDetail {
			return path == "" || name == detail
		}
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
		if name, p := a.uiPage(u, path, q); name != "" {
			a.dressUI(&p, r, here, name)
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
				if name, p := a.uiPage(u, path, q); name != "" {
					a.dressUI(&p, r, here, name)
					send("main", tmpl(name, "main", p))
				}
			case path == "/":
				vs := a.views(u, nil, "", "", "", false)
				p, changed := a.page(u, q, vs), false
				a.dress(&p, r, here, "title.monitors")
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
	vs := a.views(nil, []string{sp.Tag}, "", "", "", false)
	data.State = worst(vs)
	for _, v := range vs {
		data.Monitors = append(data.Monitors, StatusMonitor{v.Name, v.Description, v.State, v.Paused, v.PauseReason})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if sp.tmpl != nil {
		if err := sp.tmpl.Execute(w, data); err != nil {
			slog.Error("render status page", "slug", sp.Slug, "err", err)
		}
		return
	}
	p := page{S: &data, Overall: data.State}
	a.dress(&p, r, r.URL.RequestURI(), "title.status", data.Title)
	p.Nav, p.Search, p.SettingsURL, p.User, p.LogoutURL = nil, "", "", "", "" // public: no app navigation
	if err := statusTmpl.ExecuteTemplate(w, "nb-bare", p); err != nil {
		slog.Error("render status page", "slug", sp.Slug, "err", err)
	}
}
