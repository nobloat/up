package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("UP_LOG") == "" {
		slog.SetDefault(slog.New(slog.DiscardHandler))
	}
	os.Exit(m.Run())
}

type pipeNet struct {
	mu sync.Mutex
	ls map[string]*pipeListener
}

type pipeListener struct {
	addr string
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *pipeListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *pipeListener) Addr() net.Addr { return pipeAddr(l.addr) }

func (n *pipeNet) listen(addr string) *pipeListener {
	l := &pipeListener{addr: addr, ch: make(chan net.Conn), done: make(chan struct{})}
	n.mu.Lock()
	n.ls[addr] = l
	n.mu.Unlock()
	return l
}

func (n *pipeNet) dial(ctx context.Context, _, addr string) (net.Conn, error) {
	n.mu.Lock()
	l := n.ls[addr]
	n.mu.Unlock()
	refused := &net.OpError{Op: "dial", Net: "pipe", Err: errors.New("connection refused")}
	if l == nil {
		return nil, refused
	}
	c1, c2 := net.Pipe()
	select {
	case l.ch <- c2:
		return c1, nil
	case <-l.done:
		return nil, refused
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type env struct {
	t       *testing.T
	dir     string
	net     *pipeNet
	client  *http.Client
	stop    func()
	started bool
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, dir: t.TempDir(), net: &pipeNet{ls: map[string]*pipeListener{}}}
	e.client = &http.Client{Transport: &http.Transport{DialContext: e.net.dial, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(func() {
		if e.stop != nil {
			e.stop()
		}
		if e.started {
			time.Sleep(10 * time.Second)
		}
	})
	return e
}

func (e *env) cfgPath() string { return e.dir }
func (e *env) csvPath(name string) string {
	return filepath.Join(e.dir, "data", strings.ReplaceAll(name, ".", "_")+".csv")
}

func (e *env) cfg(rest string) string {
	return fmt.Sprintf(`{"data_dir": %q, "base_url": "https://pp.example/", "auth": {"user": "admin", "password": "pw"}, %s}`, filepath.Join(e.dir, "data"), rest)
}

func (e *env) writeConfig(cfg string) {
	filepath.WalkDir(e.dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".conf") {
			os.Remove(p)
		}
		return nil
	})
	write := func(name string, m map[string]any) {
		var b strings.Builder
		confLines(&b, "", m)
		p := filepath.Join(e.dir, name)
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
			e.t.Fatal(err)
		}
	}
	var c map[string]any
	dec := json.NewDecoder(strings.NewReader(cfg))
	dec.UseNumber()
	if dec.Decode(&c) != nil {
		if err := os.WriteFile(filepath.Join(e.dir, "up.conf"), []byte(cfg), 0o600); err != nil {
			e.t.Fatal(err)
		}
		return
	}
	mons, okM := c["monitors"].([]any)
	notifiers, okN := c["notifiers"].(map[string]any)
	pages, okP := c["status_pages"].([]any)
	if (!okM && c["monitors"] != nil) || (!okN && c["notifiers"] != nil) || (!okP && c["status_pages"] != nil) {
		os.WriteFile(filepath.Join(e.dir, "up.conf"), []byte("monitors = nope\n"), 0o600)
		return
	}
	for _, k := range []string{"monitors", "notifiers", "status_pages"} {
		delete(c, k)
	}
	write("up.conf", c)
	for name, n := range notifiers {
		write(name+".notifier.conf", n.(map[string]any))
	}
	for _, v := range mons {
		m := v.(map[string]any)
		name, _ := m["name"].(string)
		delete(m, "name")
		if t, ok := m["type"].(string); ok {
			if block, ok := m[t].(map[string]any); ok {
				delete(m, t)
				maps.Copy(m, block)
			}
		}
		write(name+".monitor.conf", m)
	}
	for _, v := range pages {
		p := v.(map[string]any)
		slug, _ := p["slug"].(string)
		delete(p, "slug")
		write(slug+".page.conf", p)
	}
}

func confLines(b *strings.Builder, prefix string, v any) {
	str := func(v any) string {
		if s, ok := v.(string); ok && json.Valid([]byte(s)) {
			q, _ := json.Marshal(s)
			return string(q)
		}
		return fmt.Sprint(v)
	}
	switch v := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			confLines(b, prefix+k+".", v[k])
		}
	case []any:
		var items []string
		for _, i := range v {
			items = append(items, str(i))
		}
		fmt.Fprintf(b, "%s = %s\n", strings.TrimSuffix(prefix, "."), strings.Join(items, ", "))
	default:
		fmt.Fprintf(b, "%s = %s\n", strings.TrimSuffix(prefix, "."), str(v))
	}
}

func (e *env) start(cfg string) {
	e.t.Helper()
	e.started = true
	e.writeConfig(cfg)
	l := e.net.listen("up:80")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, e.cfgPath(), l, e.net.dial) }()
	e.stop = func() {
		cancel()
		if err := <-done; err != nil {
			e.t.Error("run:", err)
		}
		e.stop = nil
	}
	synctest.Wait()
}

func (e *env) serve(addr string, h http.HandlerFunc, cert *tls.Certificate) {
	var ln net.Listener = e.net.listen(addr)
	if cert != nil {
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{*cert}})
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	e.t.Cleanup(func() { srv.Close() })
}

func (e *env) do(method, path, user, body string, hdr ...string) (*http.Response, string) {
	e.t.Helper()
	req, _ := http.NewRequest(method, "http://up"+path, strings.NewReader(body))
	if user != "" {
		req.SetBasicAuth(user, "pw")
	}
	for i := 0; i < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (e *env) get(path string) string {
	e.t.Helper()
	resp, body := e.do("GET", path, "admin", "")
	if resp.StatusCode != 200 {
		e.t.Fatalf("GET %s: %d %s", path, resp.StatusCode, body)
	}
	return body
}

func (e *env) post(path, body string, hdr ...string) (*http.Response, string) {
	e.t.Helper()
	resp, b := e.do("POST", path, "admin", body, hdr...)
	synctest.Wait()
	return resp, b
}

func (e *env) view(name string) monitorView {
	e.t.Helper()
	var v monitorView
	if err := json.Unmarshal([]byte(e.get("/api/monitors/"+name)), &v); err != nil {
		e.t.Fatal(err)
	}
	return v
}

func (e *env) wantState(name, state string) monitorView {
	e.t.Helper()
	v := e.view(name)
	if v.State != state {
		e.t.Fatalf("%s: state %s (%s), want %s", name, v.State, v.Reason, state)
	}
	return v
}

func (e *env) rows(name string) []row {
	e.t.Helper()
	rows, err := readLog(e.csvPath(name))
	if err != nil {
		e.t.Fatal(err)
	}
	return rows
}

func (e *env) states(name string) []string {
	var s []string
	for _, r := range e.rows(name) {
		if r.Event == "state" {
			s = append(s, r.State)
		}
	}
	return s
}

func sleep(d time.Duration) {
	time.Sleep(d)
	synctest.Wait()
}

func genCert(t *testing.T, notAfter time.Time) *tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter,
		DNSNames: []string{"tls-target"}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestCheckExitCodes(t *testing.T) {
	e := newEnv(t)
	tmpl := filepath.Join(e.dir, "ok.html")
	os.WriteFile(tmpl, []byte(`{{.Title}}`), 0o600)
	bad := filepath.Join(e.dir, "bad.html")
	os.WriteFile(bad, []byte(`{{.Title`), 0o600)
	mon := `{"name": "a", "type": "http", "interval": "1m", "http": {"url": "http://x"}}`
	cases := map[string]struct {
		cfg  string
		want int
	}{
		"valid": {e.cfg(`"monitors": [` + mon + `], "status_pages": [{"slug": "s", "tag": "x", "template": "ok.html"}],
			"notifiers": {"n": {"type": "smtp", "host": "h", "from": "f", "to": ["t"]}}`), 0},
		"no base_url":       {fmt.Sprintf(`{"data_dir": %q, "auth": {"user": "a", "password": "b"}}`, e.dir), 1},
		"bad base_url":      {fmt.Sprintf(`{"data_dir": %q, "base_url": "pp.example", "auth": {"user": "a", "password": "b"}}`, e.dir), 1},
		"unknown field":     {e.cfg(`"monitors": [], "bogus": 1`), 1},
		"unknown nested":    {e.cfg(`"monitors": [{"name": "a", "type": "http", "interval": "1m", "http": {"url": "http://x", "bogus": 1}}]`), 1},
		"bad duration":      {e.cfg(`"evaluation_interval": "5 minutes"`), 1},
		"no auth":           {fmt.Sprintf(`{"data_dir": %q}`, e.dir), 1},
		"unknown notifier":  {e.cfg(`"monitors": [{"name": "a", "type": "http", "interval": "1m", "notifiers": ["x"], "http": {"url": "http://x"}}]`), 1},
		"bad name":          {e.cfg(`"monitors": [{"name": "A/b", "type": "http", "interval": "1m", "http": {"url": "http://x"}}]`), 1},
		"dotted name":       {e.cfg(`"monitors": [{"name": "app01.test.zeitkapsl.at", "type": "http", "interval": "1m", "http": {"url": "http://x"}}]`), 0},
		"dot dot name":      {e.cfg(`"monitors": [{"name": "..", "type": "http", "interval": "1m", "http": {"url": "http://x"}}]`), 1},
		"leading dot name":  {e.cfg(`"monitors": [{"name": ".a", "type": "http", "interval": "1m", "http": {"url": "http://x"}}]`), 1},
		"both type blocks":  {e.cfg(`"monitors": [{"name": "a", "type": "http", "interval": "1m", "http": {"url": "http://x"}, "push": {"token": "0123456789abcdef"}}]`), 1},
		"no type block":     {e.cfg(`"monitors": [{"name": "a", "type": "push", "interval": "1m"}]`), 1},
		"bad regex":         {e.cfg(`"monitors": [{"name": "a", "type": "http", "interval": "1m", "http": {"url": "http://x", "body_regex": "("}}]`), 1},
		"bad template":      {e.cfg(`"status_pages": [{"slug": "s", "tag": "x", "template": "bad.html"}]`), 1},
		"bad notifier type": {e.cfg(`"notifiers": {"n": {"type": "sms"}}`), 1},
		"ilert valid":       {e.cfg(`"notifiers": {"n": {"type": "ilert", "integration_key": "k"}}`), 0},
		"ilert no key":      {e.cfg(`"notifiers": {"n": {"type": "ilert"}}`), 1},
		"ilert reminder":    {e.cfg(`"notifiers": {"n": {"type": "ilert", "integration_key": "k", "reminder_interval": "1h"}}`), 1},
		"bad method":        {e.cfg(`"monitors": [{"name": "a", "type": "http", "interval": "1m", "http": {"url": "http://x", "method": "PUT"}}]`), 1},
		"short push token":  {e.cfg(`"monitors": [{"name": "a", "type": "push", "interval": "1m", "push": {"token": "short"}}]`), 1},
		"http grace":        {e.cfg(`"monitors": [{"name": "a", "type": "http", "interval": "1m", "grace": "1m", "http": {"url": "http://x"}}]`), 1},
		"metric grace":      {e.cfg(`"monitors": [{"name": "a", "type": "metric", "interval": "1m", "grace": "1m", "metric": {"url": "http://x", "metric": "up", "value": ">= 0"}}]`), 1},
		"promql grace":      {e.cfg(`"monitors": [{"name": "a", "type": "promql", "interval": "1m", "grace": "1m", "promql": {"url": "http://x", "query": "up", "value": ">= 0"}}]`), 1},
		"push grace":        {e.cfg(`"monitors": [{"name": "a", "type": "push", "interval": "1m", "grace": "1m", "push": {"token": "0123456789abcdef"}}]`), 0},
		"same push token": {e.cfg(`"monitors": [{"name": "a", "type": "push", "interval": "1m", "push": {"token": "0123456789abcdef"}},
			{"name": "b", "type": "push", "interval": "1m", "push": {"token": "0123456789abcdef"}}]`), 1},
		"not key value":   {`{`, 1},
		"metric valid":    {e.cfg(`"monitors": [{"name": "a", "type": "metric", "interval": "1m", "metric": {"url": "http://x", "metric": "up", "value": ">= 0"}}]`), 0},
		"metric no value": {e.cfg(`"monitors": [{"name": "a", "type": "metric", "interval": "1m", "metric": {"url": "http://x", "metric": "up", "value": "1"}}]`), 1},
		"metric bad op":   {e.cfg(`"monitors": [{"name": "a", "type": "metric", "interval": "1m", "metric": {"url": "http://x", "metric": "up", "value": "== 1"}}]`), 1},
		"metric no name":  {e.cfg(`"monitors": [{"name": "a", "type": "metric", "interval": "1m", "metric": {"url": "http://x", "value": "> 1"}}]`), 1},
		"promql valid":    {e.cfg(`"monitors": [{"name": "a", "type": "promql", "interval": "1m", "promql": {"url": "http://x", "query": "up", "value": ">= 1"}}]`), 0},
		"promql no query": {e.cfg(`"monitors": [{"name": "a", "type": "promql", "interval": "1m", "promql": {"url": "http://x", "value": ">= 1"}}]`), 1},
	}
	for name, c := range cases {
		e.writeConfig(c.cfg)
		if got := cli([]string{"-check", "-config", e.cfgPath()}); got != c.want {
			t.Errorf("%s: exit %d, want %d", name, got, c.want)
		}
	}
	if got := cli([]string{"-check", "-config", filepath.Join(e.dir, "missing")}); got != 1 {
		t.Errorf("missing dir: exit %d", got)
	}
}

func TestConfFiles(t *testing.T) {
	e := newEnv(t)
	write := func(name, content string) {
		p := filepath.Join(e.dir, name)
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("up.conf", fmt.Sprintf(`# global
data_dir = %s
base_url = https://pp.example/
auth.user = admin
auth.password = pw # not a comment
heartbeat.urls = http://a/, http://b/
`, filepath.Join(e.dir, "data")))
	write("mail.notifier.conf", "type = smtp\nhost = h\nfrom = f\nto = a@x, b@x\nport = 25\n")
	write("prod/api.monitor.conf", `type = http
interval = 1m
tags = public, prod
notifiers = mail
  # indented comment
url = http://x/?a=b
headers.X-Api-Key = k=1
accepted_status = 200, 204
json_match.path = status
json_match.equals = "1"
max_redirects = 0
`)
	write("job.monitor.conf", "type = push\ninterval = 1h\ntoken = 0123456789abcdef\n")
	write("public.page.conf", "tag = public\ntemplate = page.html\n")
	write("page.html", `{{.Title}}`)
	write("ignored.txt", "not a config")
	cfg, err := loadConfig(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.Monitors[1].HTTP
	if len(cfg.Monitors) != 2 || cfg.Monitors[0].Name != "job" || cfg.Monitors[0].Push.Token != "0123456789abcdef" || cfg.Monitors[1].Name != "prod/api" ||
		!slices.Equal(cfg.Monitors[1].Tags, []string{"public", "prod"}) || h.URL != "http://x/?a=b" || h.Headers["X-Api-Key"] != "k=1" ||
		!slices.Equal(h.AcceptedStatus, []int{200, 204}) || h.equals != "1" || *h.MaxRedirects != 0 ||
		cfg.Auth.Password != "pw # not a comment" || len(cfg.Heartbeat.URLs) != 2 ||
		!slices.Equal(cfg.Notifiers["mail"].To, []string{"a@x", "b@x"}) || cfg.Notifiers["mail"].Port != 25 ||
		cfg.StatusPages[0].Slug != "public" || cfg.StatusPages[0].tmpl == nil {
		t.Fatalf("loaded %+v %+v", cfg, h)
	}
	if len(cfg.errs) != 0 {
		t.Fatalf("errs %v", cfg.errs)
	}

	write("broken.monitor.conf", "type = http\n")
	write("job.monitor.conf", "type = push\ninterval = 1h\ntoken = 0123456789abcdef\nnotifiers = mail, nope\n")
	cfg, err = loadConfig(e.dir)
	if err != nil || len(cfg.Monitors) != 2 || !slices.Equal(cfg.Monitors[0].Notifiers, []string{"mail"}) || len(cfg.errs) != 2 {
		t.Fatalf("partial load: %v %+v %v", err, cfg.Monitors, cfg.errs)
	}
	os.Remove(filepath.Join(e.dir, "broken.monitor.conf"))
	write("job.monitor.conf", "type = push\ninterval = 1h\ntoken = 0123456789abcdef\n")
	for name, c := range map[string][2]string{
		"no equals":      {"c.monitor.conf", "type = http\ninterval\n"},
		"duplicate key":  {"c.monitor.conf", "type = http\ninterval = 1m\ninterval = 2m\nurl = http://x\n"},
		"unknown key":    {"c.monitor.conf", "type = http\ninterval = 1m\nurl = http://x\nbogus = 1\n"},
		"other block":    {"c.monitor.conf", "type = http\ninterval = 1m\nurl = http://x\npush.token = 0123456789abcdef\n"},
		"name key":       {"c.monitor.conf", "type = http\nname = d\ninterval = 1m\nurl = http://x\n"},
		"bad int":        {"c.monitor.conf", "type = http\ninterval = 1m\nurl = http://x\naccepted_status = 200, ok\n"},
		"bad name":       {"C.monitor.conf", "type = http\ninterval = 1m\nurl = http://x\n"},
		"global in page": {"p.page.conf", "tag = x\nlisten = :1\n"},
		"nested slug":    {"a/b.page.conf", "tag = x\n"},
		"no kind":        {"c.conf", "tag = x\n"},
		"unknown kind":   {"c.pages.conf", "tag = x\n"},
		"old layout":     {"monitors/c.conf", "type = push\ninterval = 1h\ntoken = 0123456789abcdef\n"},
	} {
		write(c[0], c[1])
		if got := cli([]string{"-check", "-config", e.dir}); got != 1 {
			t.Errorf("%s: exit %d, want 1", name, got)
		}
		os.RemoveAll(filepath.Join(e.dir, c[0]))
	}
	if got := cli([]string{"-check", "-config", e.dir}); got != 0 {
		t.Errorf("cleaned up: exit %d", got)
	}
}

func TestHTTPChecks(t *testing.T) {
	cases := []struct {
		name    string
		http    string
		extra   string
		handler http.HandlerFunc
		ok      bool
		reason  string
	}{
		{name: "get 200", handler: func(w http.ResponseWriter, r *http.Request) {}, ok: true, reason: "HTTP 200"},
		{name: "post body", http: `, "method": "POST", "body": "ping"`, handler: func(w http.ResponseWriter, r *http.Request) {
			if b, _ := io.ReadAll(r.Body); r.Method != "POST" || string(b) != "ping" {
				w.WriteHeader(400)
			}
		}, ok: true},
		{name: "head", http: `, "method": "HEAD"`, handler: func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "HEAD" {
				w.WriteHeader(400)
			}
		}, ok: true},
		{name: "headers", http: `, "headers": {"X-Foo": "bar"}`, handler: func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Foo") != "bar" {
				w.WriteHeader(400)
			}
		}, ok: true},
		{name: "basic auth", http: `, "basic_auth": {"user": "u", "password": "p"}`, handler: func(w http.ResponseWriter, r *http.Request) {
			if u, p, _ := r.BasicAuth(); u != "u" || p != "p" {
				w.WriteHeader(401)
			}
		}, ok: true},
		{name: "default 204", handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }, ok: true, reason: "HTTP 204"},
		{name: "default 500", handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }, reason: "HTTP 500"},
		{name: "accepted 500", http: `, "accepted_status": [500]`, handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }, ok: true},
		{name: "not accepted 204", http: `, "accepted_status": [200]`, handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }, reason: "HTTP 204"},
		{name: "regex match", http: `, "body_regex": "^status: o+k$"`, handler: func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "status: ok") }, ok: true},
		{name: "regex mismatch", http: `, "body_regex": "ok"`, handler: func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "fail") }, reason: "body does not match"},
		{name: "json string", http: `, "json_match": {"path": "status.db", "equals": "ok"}`, handler: func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"status": {"db": "ok"}}`)
		}, ok: true},
		{name: "json number", http: `, "json_match": {"path": "items.1.n", "equals": 2}`, handler: func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"items": [{"n": 1}, {"n": 2.0}]}`)
		}, ok: true},
		{name: "json bool", http: `, "json_match": {"path": "up", "equals": true}`, handler: func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"up": true}`)
		}, ok: true},
		{name: "json mismatch", http: `, "json_match": {"path": "status.db", "equals": "ok"}`, handler: func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"status": {"db": "down"}}`)
		}, reason: `status.db is "down", want "ok"`},
		{name: "json type mismatch", http: `, "json_match": {"path": "n", "equals": "1"}`, handler: func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"n": 1}`)
		}},
		{name: "json missing", http: `, "json_match": {"path": "a.b", "equals": 1}`, handler: func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"a": 1}`)
		}},
		{name: "redirects followed", handler: redirects, ok: true},
		{name: "too many redirects", http: `, "max_redirects": 1`, handler: redirects, reason: "more than 1 redirects"},
		{name: "no redirects", http: `, "max_redirects": 0`, handler: redirects, reason: "more than 0 redirects"},
		{name: "timeout", extra: `, "timeout": "1s"`, handler: func(w http.ResponseWriter, r *http.Request) { time.Sleep(2 * time.Second) }, reason: "deadline exceeded"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				e.serve("target:80", c.handler, nil)
				e.start(e.cfg(`"monitors": [{"name": "m", "type": "http", "interval": "1m"` + c.extra + `, "http": {"url": "http://target/a"` + c.http + `}}]`))
				sleep(time.Minute)
				expectCheck(t, e.view("m"), c.ok, c.reason)
			})
		})
	}
}

func TestMetricChecks(t *testing.T) {
	metrics := `# HELP queue_depth Jobs waiting.
# TYPE queue_depth gauge
queue_depth{queue="mail",host="a"} 3
queue_depth{queue="img",note="a \"q\", {x}=1"} 12 1712345678000
temp_celsius 21.5
queue_depth_total 99
broken{x="1" 5
nan_metric NaN
`
	cases := []struct {
		name   string
		metric string
		ok     bool
		reason string
	}{
		{name: "gt", metric: `"metric": "temp_celsius", "value": "> 20"`, ok: true, reason: "temp_celsius is 21.5 (> 20)"},
		{name: "gt fails", metric: `"metric": "temp_celsius", "value": "> 21.5"`, reason: "temp_celsius is 21.5, want > 21.5"},
		{name: "ge", metric: `"metric": "temp_celsius", "value": ">= 21.5"`, ok: true},
		{name: "lt", metric: `"metric": "temp_celsius", "value": "< 22"`, ok: true},
		{name: "le fails", metric: `"metric": "temp_celsius", "value": "<= 21"`},
		{name: "labels", metric: `"metric": "queue_depth", "labels": {"queue": "mail"}, "value": "< 10"`, ok: true, reason: `queue_depth{queue="mail"} is 3 (< 10)`},
		{name: "escaped label", metric: `"metric": "queue_depth", "labels": {"note": "a \"q\", {x}=1"}, "value": "> 10"`, ok: true},
		{name: "all series must pass", metric: `"metric": "queue_depth", "value": "< 10"`, reason: `queue="img"}`},
		{name: "all series pass", metric: `"metric": "queue_depth", "value": "< 20"`, ok: true, reason: "queue_depth: 2 series < 20"},
		{name: "not found", metric: `"metric": "queue", "value": "> 0"`, reason: "queue not found"},
		{name: "label not found", metric: `"metric": "queue_depth", "labels": {"queue": "x"}, "value": "> 0"`, reason: `queue_depth{queue="x"} not found`},
		{name: "nan fails", metric: `"metric": "nan_metric", "value": "< 1"`, reason: "is NaN"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				e.serve("target:80", func(w http.ResponseWriter, r *http.Request) {
					io.WriteString(w, metrics)
				}, nil)
				e.start(e.cfg(`"monitors": [{"name": "m", "type": "metric", "interval": "1m", "metric": {"url": "http://target/metrics", ` + c.metric + `}}]`))
				sleep(time.Minute)
				expectCheck(t, e.view("m"), c.ok, c.reason)
			})
		})
	}
	t.Run("auth", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			e.serve("target:80", func(w http.ResponseWriter, r *http.Request) {
				if u, p, _ := r.BasicAuth(); u != "u" || p != "basic-secret" || r.Header.Get("X-Api-Key") != "header-secret" {
					w.WriteHeader(401)
				}
				io.WriteString(w, metrics)
			}, nil)
			m := `{"name": "%s", "type": "metric", "interval": "1m", "metric": {"url": "http://target/metrics", "metric": "temp_celsius", "value": "> 0"%s}}`
			e.start(e.cfg(`"monitors": [` + fmt.Sprintf(m, "auth", `, "basic_auth": {"user": "u", "password": "basic-secret"}, "headers": {"X-Api-Key": "header-secret"}`) +
				`, ` + fmt.Sprintf(m, "noauth", "") + `]`))
			sleep(time.Minute)
			expectCheck(t, e.view("auth"), true, "temp_celsius is 21.5")
			expectCheck(t, e.view("noauth"), false, "HTTP 401")
			if b := e.get("/monitor/auth"); strings.Contains(b, "secret") || !strings.Contains(b, "<th>headers.X-Api-Key</th><td><code>"+masked) {
				t.Fatal("metric secrets not masked on the detail page")
			}
		})
	})
}

func TestPromQLChecks(t *testing.T) {
	responses := map[string]string{
		`up{job="api"}`: `{"status":"success","data":{"resultType":"vector","result":[` +
			`{"metric":{"__name__":"up","job":"api","instance":"a"},"value":[1712345678.1,"1"]},` +
			`{"metric":{"__name__":"up","job":"api","instance":"b"},"value":[1712345678.1,"0"]}]}}`,
		`sum(up)`: `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1712345678.1,"3"]}]}}`,
		`scalar`:  `{"status":"success","data":{"resultType":"scalar","result":[1712345678.1,"2.5"]}}`,
		`empty`:   `{"status":"success","data":{"resultType":"vector","result":[]}}`,
		`up[5m]`:  `{"status":"success","data":{"resultType":"matrix","result":[]}}`,
		`nan`:     `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1712345678.1,"NaN"]}]}}`,
	}
	cases := []struct {
		name, query, op string
		value           float64
		ok              bool
		reason          string
	}{
		{"one series", "sum(up)", ">=", 3, true, "value is 3 (>= 3)"},
		{"one series fails", "sum(up)", ">", 3, false, "value is 3, want > 3"},
		{"all series must pass", `up{job="api"}`, ">=", 1, false, `up{instance="b",job="api"} is 0, want >= 1`},
		{"all series pass", `up{job="api"}`, ">=", 0, true, "2 series >= 0"},
		{"scalar", "scalar", "<", 3, true, "value is 2.5 (< 3)"},
		{"empty", "empty", ">", 0, false, "no series"},
		{"matrix", "up[5m]", ">", 0, false, `result type "matrix"`},
		{"nan fails", "nan", "<", 1, false, "is NaN"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				e.serve("prom:80", func(w http.ResponseWriter, r *http.Request) {
					if u, p, _ := r.BasicAuth(); u != "u" || p != "basic-secret" || r.URL.Path != "/prefix/api/v1/query" {
						w.WriteHeader(401)
					}
					io.WriteString(w, responses[r.FormValue("query")])
				}, nil)
				e.start(e.cfg(fmt.Sprintf(`"monitors": [{"name": "m", "type": "promql", "interval": "1m", "promql": {"url": "http://prom/prefix/",
					"basic_auth": {"user": "u", "password": "basic-secret"}, "query": %q, "value": "%s %g"}}]`, c.query, c.op, c.value)))
				sleep(time.Minute)
				v := e.view("m")
				expectCheck(t, v, c.ok, c.reason)
				b := e.get("/monitor/m")
				if strings.Contains(b, "basic-secret") {
					t.Fatal("promql password not masked on the detail page")
				}
				if !strings.Contains(b, "<th>value</th><td><code>"+template.HTMLEscapeString(fmt.Sprintf("%s %g", c.op, c.value))) || strings.Contains(b, "<th>method</th>") {
					t.Fatalf("op and value not merged or method shown: %s", b)
				}
				if c.name == "all series must pass" && (len(v.LastCheck.Samples) != 2 || v.LastCheck.Samples[1].Labels["instance"] != "b" || v.LastCheck.Samples[1].OK ||
					!strings.Contains(b, `<thead><tr><th colspan="3"><code>up{job=&#34;api&#34;}</code></th></tr></thead>`) || !strings.Contains(b, `<code>instance=&#34;b&#34;</code></td><td class="num"><code>0</code>`) ||
					!strings.Contains(b, " in 0s")) {
					t.Fatalf("samples %+v in api or detail page: %s", v.LastCheck.Samples, b)
				}
				if c.name == "nan fails" && (len(v.LastCheck.Samples) != 1 || !math.IsNaN(float64(v.LastCheck.Samples[0].Value))) {
					t.Fatalf("NaN sample %+v", v.LastCheck.Samples)
				}
			})
		})
	}
}

func expectCheck(t *testing.T, v monitorView, ok bool, reason string) {
	t.Helper()
	if v.LastCheck == nil {
		t.Fatal("no check ran")
	}
	if v.LastCheck.OK != ok || !strings.Contains(v.LastCheck.Reason, reason) {
		t.Fatalf("got ok=%v %q, want ok=%v %q", v.LastCheck.OK, v.LastCheck.Reason, ok, reason)
	}
	if want := map[bool]string{true: green, false: red}[ok]; v.State != want {
		t.Fatalf("state %s, want %s", v.State, want)
	}
}

func redirects(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/a":
		http.Redirect(w, r, "/b", http.StatusFound)
	case "/b":
		http.Redirect(w, r, "/c", http.StatusFound)
	}
}

func TestTLSChecks(t *testing.T) {
	cases := []struct {
		name, http string
		validDays  int
		ok         bool
		reason     string
	}{
		{"self-signed rejected", ``, 300, false, "certificate"},
		{"skip verify", `, "skip_verify": true`, 300, true, "HTTP 200"},
		{"cert valid long enough", `, "skip_verify": true, "cert_min_valid_days": 14`, 30, true, "HTTP 200"},
		{"cert expires soon", `, "skip_verify": true, "cert_min_valid_days": 14`, 5, false, "cert expires in 5d"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				cert := genCert(t, time.Now().Add(time.Duration(c.validDays)*24*time.Hour+time.Hour))
				e.serve("tls-target:443", func(http.ResponseWriter, *http.Request) {}, cert)

				e.start(e.cfg(`"monitors": [{"name": "m", "type": "http", "interval": "1m", "retry_count": 3, "http": {"url": "https://tls-target/"` + c.http + `}}]`))
				sleep(time.Minute)
				if c.reason == "certificate" {
					if v := e.view("m"); v.State != yellow || !strings.Contains(v.Reason, c.reason) {
						t.Fatalf("got %s %q", v.State, v.Reason)
					}
					return
				}
				expectCheck(t, e.view("m"), c.ok, c.reason)
				if !strings.Contains(c.reason, "cert expires") {
					return
				}

				e.serve("tls-target:443", func(http.ResponseWriter, *http.Request) {}, genCert(t, time.Now().AddDate(1, 0, 0)))
				e.post("/api/monitors/m/check", "")
				if v := e.view("m"); v.State != green {
					t.Fatalf("state %s %q", v.State, v.Reason)
				}
				e.serve("tls-target:443", func(http.ResponseWriter, *http.Request) {}, cert)
				e.post("/api/monitors/m/check", "")
				if v := e.view("m"); v.State != red {
					t.Fatalf("state %s %q", v.State, v.Reason)
				}
			})
		})
	}
}

func TestProxy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		var seen atomic.Value
		e.serve("proxy:3128", func(w http.ResponseWriter, r *http.Request) { seen.Store(r.URL.String()) }, nil)
		e.start(e.cfg(`"proxy": "http://proxy:3128", "monitors": [{"name": "m", "type": "http", "interval": "1m", "http": {"url": "http://target/x"}}]`))
		sleep(time.Minute)
		e.wantState("m", green)
		if seen.Load() != "http://target/x" {
			t.Fatalf("proxy saw %v", seen.Load())
		}
	})
}

func TestTransitions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		var status atomic.Int32
		status.Store(200)
		var mu sync.Mutex
		var hits []time.Time
		e.serve("target:80", func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits = append(hits, time.Now())
			mu.Unlock()
			w.WriteHeader(int(status.Load()))
		}, nil)
		e.start(e.cfg(`"monitors": [{"name": "m", "type": "http", "interval": "1m", "retry_count": 2, "retry_interval": "10s", "http": {"url": "http://target/"}}]`))
		e.wantState("m", green)

		status.Store(500)
		e.post("/api/monitors/m/check", "")
		e.wantState("m", yellow)
		sleep(10 * time.Second)
		e.wantState("m", yellow)
		sleep(10 * time.Second)
		if v := e.wantState("m", red); v.Reason != "HTTP 500" {
			t.Fatalf("reason %q", v.Reason)
		}
		sleep(time.Minute)
		mu.Lock()
		var gaps []time.Duration
		for i := 2; i < len(hits); i++ {
			gaps = append(gaps, hits[i].Sub(hits[i-1]))
		}
		mu.Unlock()
		if !slices.Equal(gaps, []time.Duration{10 * time.Second, 10 * time.Second, time.Minute}) {
			t.Fatalf("check gaps %v", gaps)
		}
		e.wantState("m", red)

		status.Store(200)
		e.post("/api/monitors/m/check", "")
		e.wantState("m", green)
		if got := e.states("m"); !slices.Equal(got, []string{red, green, yellow, red, green}) {
			t.Fatalf("logged states %v", got)
		}
	})
}

func TestNewMonitorRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		var chat atomic.Int32
		e.serve("chat:80", func(http.ResponseWriter, *http.Request) { chat.Add(1) }, nil)
		e.serve("target:80", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }, nil)
		e.start(e.cfg(`"evaluation_interval": "5s", "notifiers": {"chat": {"type": "webhook", "url": "http://chat/"}},
			"monitors": [{"name": "m", "type": "http", "interval": "1m", "retry_count": 2, "retry_interval": "10s", "notifiers": ["chat"], "http": {"url": "http://target/"}}]`))

		e.wantState("m", yellow)
		sleep(9 * time.Second)
		e.wantState("m", yellow)
		if n := chat.Load(); n != 0 {
			t.Fatalf("%d notifications for an unconfirmed failure", n)
		}
		sleep(11 * time.Second)
		e.wantState("m", red)
		sleep(5 * time.Second)
		if n := chat.Load(); n != 1 {
			t.Fatalf("%d notifications, want 1 (new -> red)", n)
		}
		if got := e.states("m"); !slices.Equal(got, []string{red, yellow, red}) {
			t.Fatalf("states %v", got)
		}
	})
}

func TestNotifierFailureRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		var down atomic.Bool
		var got atomic.Value
		down.Store(true)
		e.serve("chat:80", func(w http.ResponseWriter, r *http.Request) {
			if down.Load() {
				w.WriteHeader(500)
				return
			}
			b, _ := io.ReadAll(r.Body)
			got.Store(string(b))
		}, nil)
		e.serve("target:80", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }, nil)
		e.start(e.cfg(`"evaluation_interval": "5s", "notifiers": {"chat": {"type": "webhook", "url": "http://chat/"}},
			"monitors": [{"name": "m", "type": "http", "interval": "1h", "notifiers": ["chat"], "http": {"url": "http://target/"}}]`))
		e.post("/api/monitors/m/check", "")
		e.wantState("m", red)
		sleep(6 * time.Second)
		if !strings.Contains(e.get("/"), "notifier <b>chat</b> failing since") {
			t.Fatal("no failure banner")
		}
		down.Store(false)
		sleep(5 * time.Second)
		if b, _ := got.Load().(string); !strings.Contains(b, "new → 🔴") {
			t.Fatalf("red change not redelivered: %q", b)
		}
		if strings.Contains(e.get("/"), "failing since") {
			t.Fatal("failure banner after recovery")
		}
	})
}

func TestNewPushMonitorSilent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		var chat atomic.Int32
		e.serve("chat:80", func(http.ResponseWriter, *http.Request) { chat.Add(1) }, nil)
		e.start(e.cfg(`"evaluation_interval": "10s", "notifiers": {"chat": {"type": "webhook", "url": "http://chat/", "reminder_interval": "20s"}},
			"monitors": [{"name": "p", "type": "push", "interval": "1h", "notifiers": ["chat"], "push": {"token": "0123456789abcdef"}}]`))
		sleep(time.Minute)
		if n := chat.Load(); n != 0 {
			t.Fatalf("%d notifications for a never-checked monitor", n)
		}
		sleep(time.Hour)
		if n := chat.Load(); n == 0 {
			t.Fatal("no notification after the missed push")
		}
	})
}

func TestPush(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.start(e.cfg(`"monitors": [
			{"name": "backup", "type": "push", "interval": "1h", "grace": "10m", "retry_count": 1, "retry_interval": "5m", "push": {"token": "0123456789abcdef"}},
			{"name": "web", "type": "http", "interval": "1h", "http": {"url": "http://target/"}}]`))
		push := func(method, path, body string) int {
			resp, _ := e.do(method, path, "", body)
			synctest.Wait()
			return resp.StatusCode
		}
		for _, p := range []string{"/push/backup/wrong", "/push/nope/0123456789abcdef", "/push/web/0123456789abcdef"} {
			if code := push("GET", p, ""); code != 404 {
				t.Fatalf("%s: %d", p, code)
			}
		}
		e.wantState("backup", red)
		if b := e.get("/monitor/backup"); strings.Contains(b, "0123456789abcdef") || !strings.Contains(b, "<th>token</th><td><code>"+masked) {
			t.Fatal("push token not masked on the detail page")
		}
		if code := push("GET", "/push/backup/0123456789abcdef", ""); code != 200 {
			t.Fatal(code)
		}
		e.wantState("backup", green)
		push("POST", "/push/backup/0123456789abcdef", "{\n  \"files\": 3\n}")
		push("POST", "/push/backup/0123456789abcdef", `done, "ok"`)
		if code := push("POST", "/push/backup/0123456789abcdef", strings.Repeat("x", 4097)); code != 413 {
			t.Fatalf("large payload: %d", code)
		}
		if code := push("POST", "/push/backup/0123456789abcdef", strings.Repeat("x", 4096)); code != 200 {
			t.Fatalf("4 KB payload: %d", code)
		}
		var msgs []string
		for _, r := range e.rows("backup") {
			if r.Event == "push" {
				msgs = append(msgs, r.Message)
			}
		}
		if want := []string{"(no payload)", `{"files":3}`, `"done, \"ok\""`, `"` + strings.Repeat("x", 4096) + `"`}; !slices.Equal(msgs, want) {
			t.Fatalf("push rows %q", msgs)
		}

		push("GET", "/push/backup/0123456789abcdef?status=down", "")
		if v := e.wantState("backup", yellow); v.Reason != "reported down" {
			t.Fatal(v.Reason)
		}
		push("POST", "/push/backup/0123456789abcdef?status=down", "")
		e.wantState("backup", red)
		push("GET", "/push/backup/0123456789abcdef?status=up", "")
		e.wantState("backup", green)

		sleep(70*time.Minute - time.Second)
		e.wantState("backup", green)
		sleep(time.Second)
		if v := e.wantState("backup", yellow); v.Reason != "no push for 1h10m0s" {
			t.Fatal(v.Reason)
		}
		sleep(5 * time.Minute)
		e.wantState("backup", red)
		push("GET", "/push/backup/0123456789abcdef", "")
		e.wantState("backup", green)
	})
}

func TestPushCompat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		cfg := e.cfg(`"monitors": [
			{"name": "kuma", "type": "push", "interval": "1h", "push": {"token": "kuma-token-0123456789"}},
			{"name": "hc", "type": "push", "interval": "1h", "push": {"token": "5d0e2a3c-1b4f-4f7e-9a8b-0c1d2e3f4a5b"}}]`)
		e.start(cfg)
		push := func(method, path, body string) (int, string) {
			resp, b := e.do(method, path, "", body)
			synctest.Wait()
			return resp.StatusCode, b
		}
		reason := func(name, state, want string) {
			t.Helper()
			if v := e.wantState(name, state); v.Reason != want {
				t.Fatalf("%s: reason %q, want %q", name, v.Reason, want)
			}
		}

		if code, b := push("GET", "/api/push/wrong-token-0123456789", ""); code != 404 || !strings.Contains(b, `"ok":false`) {
			t.Fatalf("unknown token: %d %s", code, b)
		}
		if code, b := push("GET", "/api/push/kuma-token-0123456789?status=up&msg=OK&ping=12", ""); code != 200 || strings.TrimSpace(b) != `{"ok":true}` {
			t.Fatalf("kuma push: %d %s", code, b)
		}
		reason("kuma", green, "push received: OK")
		push("GET", "/api/push/kuma-token-0123456789", "")
		reason("kuma", green, "push received")
		push("GET", "/api/push/kuma-token-0123456789?status=down&msg=disk%20full", "")
		reason("kuma", red, "reported down: disk full")
		if code, _ := push("GET", "/api/push/kuma-token-0123456789?status=maybe", ""); code != 400 {
			t.Fatalf("bad status: %d", code)
		}

		hc := "/ping/5d0e2a3c-1b4f-4f7e-9a8b-0c1d2e3f4a5b"
		if code, b := push("GET", "/ping/5d0e2a3c-0000-0000-0000-000000000000", ""); code != 404 || !strings.Contains(b, "not found") {
			t.Fatalf("unknown uuid: %d %s", code, b)
		}
		if code, b := push("HEAD", hc, ""); code != 200 {
			t.Fatalf("hc ping: %d %s", code, b)
		}
		reason("hc", green, "push received")
		push("POST", hc+"/start", "")
		push("POST", hc+"/log", "step 1 done")
		reason("hc", green, "push received")
		push("POST", hc+"/fail", "boom")
		reason("hc", red, "reported down")
		push("POST", hc+"/0", strings.Repeat("x", 5000))
		reason("hc", green, "push received (exit 0)")
		push("GET", hc+"/3", "")
		reason("hc", red, "reported down (exit 3)")
		for _, s := range []string{"/256", "/-1", "/nope"} {
			if code, _ := push("GET", hc+s, ""); code != 400 {
				t.Fatalf("%s: %d", s, code)
			}
		}
		var got []string
		for _, r := range e.rows("hc") {
			if r.Event == "push" || r.Event == "signal" {
				got = append(got, r.Event+" "+r.Message)
			}
		}
		want := []string{"push (no payload)", "signal start: (no payload)", `signal log: "step 1 done"`, `push "boom"`,
			`push "` + strings.Repeat("x", 4096) + `"`, "push (no payload)"}
		if !slices.Equal(got, want) {
			t.Fatalf("hc audit rows %q", got)
		}

		push("GET", hc, "")
		sleep(time.Hour - time.Minute)
		push("GET", hc+"/start", "")
		e.stop()
		e.start(cfg)
		sleep(time.Minute)
		reason("hc", red, "no push for 1h0m0s")
	})
}

func fakeSMTP(e *env, addr string) chan string {
	msgs := make(chan string, 10)
	l := e.net.listen(addr)
	e.t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				tp := textproto.NewConn(c)
				tp.PrintfLine("220 fake")
				for {
					line, err := tp.ReadLine()
					if err != nil {
						return
					}
					switch strings.ToUpper(strings.Fields(line + " x")[0]) {
					case "DATA":
						tp.PrintfLine("354 go")
						b, _ := tp.ReadDotBytes()
						msgs <- string(b)
						tp.PrintfLine("250 ok")
					case "QUIT":
						tp.PrintfLine("221 bye")
						return
					default:
						tp.PrintfLine("250 ok")
					}
				}
			}()
		}
	}()
	return msgs
}

func TestIlert(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		var status atomic.Int32
		status.Store(200)
		e.serve("target:80", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(int(status.Load())) }, nil)
		var mu sync.Mutex
		var events []string
		e.serve("ilert:80", func(w http.ResponseWriter, r *http.Request) {
			var ev struct {
				IntegrationKey, EventType, AlertKey, Summary, Details string
				Links                                                 []struct{ Href, Text string }
			}
			json.NewDecoder(r.Body).Decode(&ev)
			mu.Lock()
			events = append(events, fmt.Sprintf("%s %s %s %s | %s | %s %s", r.Method, ev.IntegrationKey, ev.EventType, ev.AlertKey, ev.Summary, ev.Links[0].Href, ev.Details))
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		}, nil)
		take := func() []string {
			mu.Lock()
			defer mu.Unlock()
			ev := events
			events = nil
			return ev
		}
		e.start(e.cfg(`"evaluation_interval": "1m",
			"notifiers": {"il": {"type": "ilert", "url": "http://ilert/api/events", "integration_key": "il-key"}},
			"monitors": [{"name": "m", "type": "http", "interval": "1m", "notifiers": ["il"], "http": {"url": "http://target/"}}]`))
		sleep(time.Minute)
		if ev := take(); len(ev) != 0 {
			t.Fatalf("events for new -> green: %q", ev)
		}
		status.Store(503)
		sleep(2 * time.Minute)
		if ev := take(); !slices.Equal(ev, []string{"POST il-key ALERT m | m is down: HTTP 503 | https://pp.example/monitor/m green -> red (HTTP 503)"}) {
			t.Fatalf("alert: %q", ev)
		}
		sleep(time.Hour)
		if ev := take(); len(ev) != 0 {
			t.Fatalf("reminder sent: %q", ev)
		}
		status.Store(200)
		sleep(2 * time.Minute)
		if ev := take(); !slices.Equal(ev, []string{"POST il-key RESOLVE m | m recovered: HTTP 200 | https://pp.example/monitor/m red -> green (HTTP 200)"}) {
			t.Fatalf("resolve: %q", ev)
		}
	})
}

func TestNotifications(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		status := map[string]*atomic.Int32{"a": {}, "b": {}, "c": {}}
		for _, s := range status {
			s.Store(200)
		}
		e.serve("target:80", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(int(status[strings.Trim(r.URL.Path, "/")].Load()))
		}, nil)
		var mu sync.Mutex
		var chat []string
		e.serve("chat:80", func(w http.ResponseWriter, r *http.Request) {
			var m struct {
				Text        string
				Username    string
				IconURL     string `json:"icon_url"`
				Attachments []webhookAttachment
			}
			json.NewDecoder(r.Body).Decode(&m)
			lines := []string{m.Text + " [" + m.Username + " " + m.IconURL + "]"}
			for _, a := range m.Attachments {
				f := map[string]string{}
				for _, x := range a.Fields {
					f[x.Title] = x.Value
				}
				lines = append(lines, fmt.Sprintf("%s %s %s %s (%s) [%s] [%s] %s", a.Title, a.TitleLink, a.Color, f["State"], f["Reason"], f["Type"], f["Tags"], f["Since"]))
			}
			mu.Lock()
			chat = append(chat, strings.Join(lines, "\n"))
			mu.Unlock()
		}, nil)
		mails := fakeSMTP(e, "mail:25")
		takeChat := func() []string {
			mu.Lock()
			defer mu.Unlock()
			c := chat
			chat = nil
			return c
		}
		mon := func(name string, retries int) string {
			return fmt.Sprintf(`{"name": %q, "type": "http", "interval": "1m", "retry_count": %d, "tags": ["prod", "web"], "notifiers": ["chat", "mail"], "http": {"url": "http://target/%s"}}`, name, retries, name)
		}
		e.start(e.cfg(`"evaluation_interval": "1m",
			"notifiers": {
				"chat": {"type": "webhook", "url": "http://chat/hook", "reminder_interval": "1h"},
				"mail": {"type": "smtp", "host": "mail", "port": 25, "tls": "none", "from": "pp@example.com", "to": ["ops@example.com"]}},
			"monitors": [` + mon("a", 0) + `,` + mon("b", 0) + `,` + mon("c", 100000) + `]`))

		sleep(time.Minute)
		got := takeChat()
		if len(got) != 1 || !strings.HasPrefix(got[0], "up: 3 change(s) [up https://pp.example/static/icon-green.png]\n") ||
			!strings.Contains(got[0], "\na https://pp.example/monitor/a #22c55e new → 🟢 (HTTP 200) [http] [prod, web] 2000-01-01T00:00:") ||
			!strings.Contains(got[0], "\nb https://pp.example/monitor/b ") || !strings.Contains(got[0], "\nc https://pp.example/monitor/c ") {
			t.Fatalf("chat %q", got)
		}
		select {
		case m := <-mails:
			if !strings.Contains(m, "Subject: [up] 3 change(s)") || !strings.Contains(m, "To: ops@example.com") || !strings.Contains(m, "a: 🟢 (HTTP 200) https://pp.example/monitor/a\n") {
				t.Fatalf("mail %q", m)
			}
		default:
			t.Fatal("no mail")
		}

		status["c"].Store(500)
		sleep(5 * time.Minute)
		e.wantState("c", yellow)
		if got := takeChat(); got != nil {
			t.Fatalf("chat %q", got)
		}

		status["a"].Store(500)
		sleep(2 * time.Minute)
		e.wantState("a", red)
		if got := takeChat(); len(got) != 1 || !strings.HasPrefix(got[0], "up: 1 change(s) [up https://pp.example/static/icon-red.png]\na https://pp.example/monitor/a #ef4444 🟢 → 🔴 (HTTP 500) [http] [prod, web] ") {
			t.Fatalf("chat %q", got)
		}
		sleep(time.Hour)
		if got := takeChat(); len(got) != 1 || !strings.HasPrefix(got[0], "up: 1 still 🔴 [up https://pp.example/static/icon-red.png]\na https://pp.example/monitor/a #ef4444 🔴 (HTTP 500) [http] [prod, web] ") {
			t.Fatalf("reminder %q", got)
		}

		e.post("/api/monitors/a/pause", `{"reason": "maint"}`, "Content-Type", "application/json")
		e.post("/api/monitors/b/pause", "", "Content-Type", "application/json")
		status["a"].Store(200)
		status["b"].Store(500)
		sleep(2 * time.Hour)
		e.wantState("a", green)
		e.wantState("b", red)
		if got := takeChat(); got != nil {
			t.Fatalf("paused monitors notified: %q", got)
		}

		status["a"].Store(500)
		sleep(2 * time.Minute)
		e.post("/api/monitors/a/resume", "")
		e.post("/api/monitors/b/resume", "")
		sleep(time.Minute)
		if got := takeChat(); len(got) != 1 || !strings.HasPrefix(got[0], "up: 1 change(s) [up https://pp.example/static/icon-red.png]\nb https://pp.example/monitor/b #ef4444 🟢 → 🔴 (HTTP 500) ") {
			t.Fatalf("after resume %q", got)
		}
	})
}

func TestRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.serve("target:80", func(http.ResponseWriter, *http.Request) {}, nil)
		mon := func(name string) string {
			return fmt.Sprintf(`{"name": %q, "type": "http", "interval": "1h", "http": {"url": "http://target/"}}`, name)
		}
		e.start(e.cfg(`"monitors": [` + mon("a") + `]`))
		sleep(time.Hour)
		e.wantState("a", green)
		e.post("/api/monitors/a/pause", `{"reason": "maintenance until 14:00"}`, "Content-Type", "application/json")
		e.stop()
		if r := e.rows("a"); r[len(r)-1].Event != "stop" {
			t.Fatalf("last row %+v", r[len(r)-1])
		}

		sleep(time.Hour)
		e.start(e.cfg(`"monitors": [` + mon("a") + `,` + mon("b") + `]`))
		v := e.wantState("a", green)
		if !v.Paused || v.PauseReason != "maintenance until 14:00" || v.PausedSince.IsZero() || v.LastCheck == nil {
			t.Fatalf("restored %+v", v)
		}
		if got := e.states("a"); !slices.Equal(got, []string{red, green}) {
			t.Fatalf("a states %v", got)
		}
		if got := e.states("b"); !slices.Equal(got, []string{red, green}) {
			t.Fatalf("b states %v", got)
		}

		if u := v.Uptime; u == nil || u.Pct() != "100.00%" || u.DowntimeSeconds != 0 {
			t.Fatalf("uptime %+v", u)
		}
	})
}

func TestPauseUntil(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.serve("target:80", func(http.ResponseWriter, *http.Request) {}, nil)
		cfg := e.cfg(`"monitors": [{"name": "a", "type": "http", "interval": "1h", "http": {"url": "http://target/"}},
			{"name": "p", "type": "push", "interval": "1h", "push": {"token": "0123456789abcdef"}}]`)
		e.start(cfg)
		ct := []string{"Content-Type", "application/json"}
		for _, body := range []string{`{"for": "1h", "until": "2099-01-01T00:00:00Z"}`, `{"for": "-1h"}`, `{"for": "-1d"}`, `{"for": "1d-2h"}`, `{"for": "1w"}`, `{"until": "2000-01-01T00:00:00Z"}`, `{"until": "14:00"}`} {
			if resp, b := e.post("/api/monitors/a/pause", body, ct...); resp.StatusCode != 400 || e.view("a").Paused {
				t.Fatalf("%s: %d %s", body, resp.StatusCode, b)
			}
		}

		e.post("/api/monitors/a/pause", `{"reason": "deploy", "for": "2h"}`, ct...)
		if v := e.view("a"); !v.Paused || !v.PausedUntil.Equal(time.Now().Add(2*time.Hour)) {
			t.Fatalf("paused %+v", v)
		}
		if b := e.get("/monitor/a"); !strings.Contains(b, " · until ") {
			t.Fatal("detail lacks the end of the pause")
		}
		sleep(2*time.Hour - time.Second)
		if !e.view("a").Paused {
			t.Fatal("ended early")
		}
		sleep(time.Second)
		if v := e.view("a"); v.Paused || !v.PausedUntil.IsZero() {
			t.Fatalf("not ended %+v", v)
		}
		if r := e.rows("a"); r[len(r)-1].Event != "resumed" || r[len(r)-1].Message != "pause ended, was paused: deploy" {
			t.Fatalf("last row %+v", r[len(r)-1])
		}

		e.post("/api/monitors/a/pause", `{"for": "1d12h"}`, ct...)
		if v := e.view("a"); !v.PausedUntil.Equal(time.Now().Add(36 * time.Hour)) {
			t.Fatalf("days %+v", v)
		}

		e.post("/api/monitors/a/pause", `{"for": "1h"}`, ct...)
		e.post("/api/monitors/a/pause", `{"reason": "open end"}`, ct...)
		sleep(2 * time.Hour)
		if v := e.view("a"); !v.Paused || v.PauseReason != "open end" {
			t.Fatalf("old end applied %+v", v)
		}

		until := time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339)
		e.post("/api/monitors/a/pause", `{"until": "`+until+`"}`, ct...)
		e.stop()
		sleep(time.Hour)
		e.start(cfg)
		if v := e.view("a"); !v.Paused || v.PausedUntil.UTC().Format(time.RFC3339) != until {
			t.Fatalf("restored %+v", v)
		}
		e.stop()
		sleep(3 * time.Hour)
		e.start(cfg)
		if e.view("a").Paused {
			t.Fatal("expired pause restored")
		}

		if resp, _ := e.post("/api/monitors/p/check", ""); resp.StatusCode != 400 {
			t.Fatalf("push check %d", resp.StatusCode)
		}
		if strings.Contains(e.get("/monitor/p"), "/api/monitors/p/check") || strings.Contains(e.get("/"), "/api/monitors/p/check") {
			t.Fatal("push monitor offers check now")
		}
	})
}

func near(a, b float64) bool { return a-b < 0.01 && b-a < 0.01 }

func TestUptime(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }
	rows := []row{
		{at(0), "start", "", ""},
		{at(0), "state", red, reasonNew},
		{at(1), "state", green, "HTTP 200"},
		{at(3), "state", yellow, "HTTP 500"},
		{at(4), "push", "", ""},
		{at(6), "start", "", ""},
		{at(7), "state", red, "x"},
		{at(8), "stop", "", ""},
		{at(9), "start", "", ""},
		{at(10), "state", green, ""},
	}

	u := computeUptime(rows, at(12))
	if u.Percent == nil || !near(*u.Percent, 400.0/6) || u.DowntimeSeconds != 2*3600 || !u.Since.Equal(t0) || u.Pct() != "66.67%" || u.Down() != "2h 0m" {
		t.Fatalf("uptime %+v %s %s", u, u.Pct(), u.Down())
	}
	if u := computeUptime(nil, at(12)); u.Percent != nil || u.Pct() != "–" || u.Down() != "0s" {
		t.Fatalf("empty log %+v", u)
	}
	for d, want := range map[time.Duration]string{50 * time.Second: "50s", 250 * time.Second: "4m 10s", 26*time.Hour + 30*time.Minute: "1d 2h"} {
		if got := (uptime{DowntimeSeconds: int64(d.Seconds())}).Down(); got != want {
			t.Errorf("Down(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestUptimeNewMonitor(t *testing.T) {

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rows := []row{{t0, "start", "", ""}, {t0, "state", red, reasonNew}, {t0.Add(time.Minute), "stop", "", ""},
		{t0.Add(2 * time.Minute), "start", "", ""}, {t0.Add(3 * time.Minute), "state", green, "HTTP 418"}}
	if u := computeUptime(rows, t0.Add(time.Hour)); u.Pct() != "100.00%" || u.DowntimeSeconds != 0 {
		t.Fatalf("uptime %+v", u)
	}
}

func TestAuditFormat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.start(e.cfg(`"monitors": [{"name": "p", "type": "push", "interval": "1h", "push": {"token": "0123456789abcdef"}}]`))
		e.do("POST", "/push/p/0123456789abcdef", "", `{"msg": "a,b \"c\""}`)
		synctest.Wait()
		e.post("/api/monitors/p/pause", "reason=x, y", "Content-Type", "application/x-www-form-urlencoded")
		e.post("/api/monitors/p/resume", "")
		e.post("/api/monitors/p/pause", "")
		e.do("GET", "/push/p/0123456789abcdef", "", "")
		synctest.Wait()
		e.stop()
		f, _ := os.Open(e.csvPath("p"))
		defer f.Close()
		recs, err := csv.NewReader(f).ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		ts := time.Now().UTC().Format(time.RFC3339)
		want := [][]string{auditHeader, {ts, "start", "", "up " + version() + " started"}, {ts, "state", "red", "new monitor"},
			{ts, "push", "", `{"msg":"a,b \"c\""}`}, {ts, "state", "green", "push received"}, {ts, "paused", "", "x, y"},
			{ts, "resumed", "", "resumed, was paused: x, y"}, {ts, "paused", "", "no reason given"}, {ts, "push", "", "(no payload)"},
			{ts, "stop", "", "up stopped"}}
		if fmt.Sprint(recs) != fmt.Sprint(want) {
			t.Fatalf("csv\n%q\nwant\n%q", recs, want)
		}
		for _, r := range recs[1:] {
			if r[3] == "" {
				t.Fatalf("row without message: %q", r)
			}
		}
		if r := restore(e.rows("p")); !r.paused || r.pauseReason != "" {
			t.Fatalf("restored %+v", r)
		}
	})
}

func TestReload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.serve("target:80", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/down" {
				w.WriteHeader(500)
			}
		}, nil)
		mon := func(name, path string) string {
			return fmt.Sprintf(`{"name": %q, "type": "http", "interval": "1m", "http": {"url": "http://target/%s"}}`, name, path)
		}
		names := func() []string {
			var vs []monitorView
			json.Unmarshal([]byte(e.get("/api/monitors")), &vs)
			var n []string
			for _, v := range vs {
				n = append(n, v.Name)
			}
			return n
		}
		e.start(e.cfg(`"monitors": [` + mon("a", "up") + `]`))
		sleep(time.Minute)
		e.wantState("a", green)

		e.writeConfig(e.cfg(`"monitors": [` + mon("b", "up") + `]`))
		sleep(reloadInterval)
		if n := names(); !slices.Equal(n, []string{"b"}) {
			t.Fatalf("monitors %v", n)
		}
		if r := e.rows("a"); r[len(r)-1].Event != "stop" || r[len(r)-1].Message != "removed by config reload" {
			t.Fatalf("a not stopped: %+v", r[len(r)-1])
		}
		if r := e.rows("b"); r[0].Event != "start" || r[0].Message != "added by config reload" {
			t.Fatalf("b start row %+v", r[0])
		}
		if got := e.states("b"); !slices.Equal(got, []string{red, green}) {
			t.Fatalf("b states %v", got)
		}

		e.writeConfig(`{"monitors": "nope"}`)
		sleep(reloadInterval)
		if n := names(); !slices.Equal(n, []string{"b"}) {
			t.Fatalf("monitors %v", n)
		}

		sleep(time.Minute)
		e.wantState("b", green)
		e.writeConfig(e.cfg(`"monitors": [` + mon("a", "up") + `,` + mon("b", "down") + `]`))
		sleep(reloadInterval)
		e.wantState("a", green)
		e.wantState("b", green)
		e.post("/api/monitors/b/check", "")
		e.wantState("b", red)
		if got := e.states("a"); !slices.Equal(got, []string{red, green}) {
			t.Fatalf("a states %v", got)
		}
	})
}

func TestDottedName(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.serve("target:80", func(w http.ResponseWriter, r *http.Request) {}, nil)
		e.start(e.cfg(`"monitors": [{"name": "app01.test.example.at", "type": "http", "interval": "1m", "http": {"url": "http://target/"}}]`))
		e.wantState("app01.test.example.at", green)
		if _, err := os.Stat(filepath.Join(e.dir, "data", "app01_test_example_at.csv")); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(e.get("/monitor/app01.test.example.at"), "app01.test.example.at") {
			t.Fatal("detail page")
		}
	})
}

func TestSlashName(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.serve("target:80", func(w http.ResponseWriter, r *http.Request) {}, nil)
		e.start(e.cfg(`"monitors": [
			{"name": "prod/api", "type": "http", "interval": "1m", "http": {"url": "http://u:urlpw@target/x?api_key=qsecret&format=json"}},
			{"name": "prod/down", "type": "http", "interval": "1m", "http": {"url": "http://nohost/?token=qsecret2"}},
			{"name": "jobs/backup", "type": "push", "interval": "1h", "push": {"token": "0123456789abcdef"}}]`))
		e.wantState("prod/api", green)
		if _, err := os.Stat(filepath.Join(e.dir, "data", "prod+api.csv")); err != nil {
			t.Fatal(err)
		}
		b := e.get("/monitor/prod/api")
		for _, want := range []string{"<th>interval</th><td><code>1m</code>", "<th>notifiers</th><td><code>nobody</code>", `action="/api/monitors/prod/api/check"`, "format=json", "u:" + masked + "@target", "api_key=" + masked} {
			if !strings.Contains(b, want) {
				t.Fatalf("detail lacks %q: %s", want, b)
			}
		}
		if strings.Contains(b, "urlpw") || strings.Contains(b, "qsecret") || strings.Contains(b, "<th>name</th>") {
			t.Fatalf("detail leaks a secret or shows generic options: %s", b)
		}
		e.post("/api/monitors/prod/api/pause", "")
		if !e.view("prod/api").Paused {
			t.Fatal("not paused")
		}
		if v := e.wantState("prod/down", red); v.Reason == "" || strings.Contains(v.Reason, "qsecret2") {
			t.Fatalf("reason %q", v.Reason)
		}
		if resp, _ := e.do("GET", "/push/jobs/backup/0123456789abcdef", "", ""); resp.StatusCode != 200 {
			t.Fatal(resp.StatusCode)
		}
		synctest.Wait()
		e.wantState("jobs/backup", green)
	})
}

func TestReloadFiles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		dir := e.dir
		mon := func(tag string) []byte {
			return fmt.Appendf(nil, "type = http\ninterval = 1m\ntags = %s\nurl = http://target/\n", tag)
		}
		e.start(e.cfg(`"monitors": [{"name": "a", "type": "http", "interval": "1m", "http": {"url": "http://target/"}}]`))
		e.view("a")

		os.WriteFile(filepath.Join(dir, "b.monitor.conf"), mon("x"), 0o600)
		sleep(reloadInterval)
		e.view("b")

		os.WriteFile(filepath.Join(dir, "b.monitor.conf"), mon("changed"), 0o600)
		sleep(reloadInterval)
		if v := e.view("b"); !slices.Equal(v.Tags, []string{"changed"}) {
			t.Fatalf("tags %v", v.Tags)
		}

		os.Remove(filepath.Join(dir, "a.monitor.conf"))
		sleep(reloadInterval)
		if resp, _ := e.do("GET", "/api/monitors/a", "admin", ""); resp.StatusCode != 404 {
			t.Fatalf("a still there: %d", resp.StatusCode)
		}

		os.WriteFile(filepath.Join(dir, "c.monitor.conf"), []byte("listen = :1\n"), 0o600)
		sleep(reloadInterval)
		e.view("b")
	})
}

func TestAPIAndUI(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.serve("target:80", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }, nil)
		tmpl := filepath.Join(e.dir, "custom.html")
		os.WriteFile(tmpl, []byte(`{{.Title}}:{{range .Monitors}}{{.Name}}={{.State}},{{.Paused}},{{.PauseReason}};{{end}}`), 0o600)
		e.start(e.cfg(`"monitors": [
			{"name": "web", "type": "http", "description": "Shop front page answers", "interval": "1m", "tags": ["public", "prod"], "http": {"url": "http://target/secret-path",
				"basic_auth": {"user": "bob", "password": "hunter2hunter2"}, "headers": {"Authorization": "Bearer zzz", "X-Env": "staging"}}},
			{"name": "internal", "type": "http", "interval": "1m", "tags": ["prod"], "http": {"url": "http://target/"}}],
			"status_pages": [{"slug": "public", "title": "Example Status", "tag": "public"}, {"slug": "custom", "tag": "public", "template": "custom.html"}]`))
		sleep(time.Minute)

		for _, p := range []string{"/", "/monitor/web", "/api/monitors", "/api/monitors/web", "/events"} {
			if resp, _ := e.do("GET", p, "", ""); resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") == "" {
				t.Fatalf("%s without auth: %d", p, resp.StatusCode)
			}
		}
		if resp, _ := e.do("GET", "/", "admin", "", "Authorization", "Basic YWRtaW46d3Jvbmc="); resp.StatusCode != 401 {
			t.Fatalf("wrong password: %d", resp.StatusCode)
		}
		if resp, _ := e.do("POST", "/api/monitors/web/check", "", ""); resp.StatusCode != 401 {
			t.Fatalf("post without auth: %d", resp.StatusCode)
		}

		if b := e.get("/"); !strings.Contains(b, `href="/monitor/web"`) || !strings.Contains(b, `data-red="2"`) || !strings.Contains(b, "<title>][ up (2 failing)</title>") ||
			!strings.Contains(b, `data-state="red"`) || !strings.Contains(b, `href="/static/favicon-red.svg"`) || !strings.Contains(b, `class="mark red"`) {
			t.Fatalf("list: %s", b)
		}
		if b := e.get("/"); strings.Contains(b, `href="/status/`) ||

			!strings.Contains(e.get("/monitor/web"), `<a href="/status/public">Example Status</a>`) || strings.Contains(e.get("/monitor/internal"), `<span class="muted">on `) {
			t.Fatal("status page links missing")
		}
		if b := e.get("/"); strings.Contains(b, "/pause") || !strings.Contains(b, `action="/api/monitors/web/check"`) {
			t.Fatal("list offers pause or lacks check")
		}
		if b := e.get("/monitor/web"); !strings.Contains(b, `action="/api/monitors/web/pause"`) || !strings.Contains(b, `action="/api/monitors/web/check"`) {
			t.Fatal("detail lacks pause or check")
		}
		if b := e.get("/?tag=public"); strings.Contains(b, "/monitor/internal") ||
			!strings.Contains(b, `<a class="tag on" href="/">public</a>`) || !strings.Contains(b, `<a class="tag" href="/?tag=public&amp;tag=prod">prod</a>`) {
			t.Fatalf("tag filter: %s", b)
		}
		if b := e.get("/?tag=public&tag=prod"); strings.Contains(b, "/monitor/internal") || !strings.Contains(b, "/monitor/web") ||
			!strings.Contains(b, `<a class="tag on" href="/?tag=prod">public</a>`) || !strings.Contains(b, `<a class="tag on" href="/?tag=public">prod</a>`) {
			t.Fatalf("tags and: %s", b)
		}
		if b := e.get("/?q=TER&state=red"); strings.Contains(b, "/monitor/web") || !strings.Contains(b, "/monitor/internal") ||
			!strings.Contains(b, `<a class="chip on" href="/?q=TER">`) || !strings.Contains(b, `<a class="chip" href="/?q=TER&amp;state=green">`) {
			t.Fatalf("search filter: %s", b)
		}
		if b := e.get("/?state=green"); strings.Contains(b, `href="/monitor/`) || !strings.Contains(b, `<span class="n">2</span>`) {
			t.Fatalf("state filter: %s", b)
		}
		for _, p := range []string{"/", "/monitor/web"} {
			if b := e.get(p); strings.Contains(b, ">red<") || !strings.Contains(b, `<span class="dot red" role="img" aria-label="red"`) {
				t.Fatalf("%s state rendering: %s", p, b)
			}
		}
		if b := e.get("/monitor/web"); !strings.Contains(b, "HTTP 503") || !strings.Contains(b, "new monitor") || !strings.Contains(b, "uptime <b>0.00%</b>, <b>1m 0s</b> down") {
			t.Fatalf("detail: %s", b)
		}
		for _, p := range []string{"/", "/monitor/web"} {
			if b := e.get(p); !strings.Contains(b, `<a href="https://nobloat.org" rel="noopener">nobloat.org</a>`) || !strings.Contains(b, "· "+version()) {
				t.Fatalf("%s footer: %s", p, b)
			}
		}

		b := e.get("/monitor/web")
		for _, want := range []string{"<th>url</th><td><code><a href=\"http://target/secret-path\" rel=\"noreferrer\">http://target/secret-path</a></code>",
			"<th>timeout</th><td><code>10s</code>", "<th>headers.X-Env</th><td><code>staging</code>",
			"<th>basic_auth.user</th><td><code>bob</code>", "<th>headers.Authorization</th><td><code>" + masked} {
			if !strings.Contains(b, want) {
				t.Fatalf("detail config lacks %q", want)
			}
		}
		if strings.Contains(b, "hunter2") || strings.Contains(b, "Bearer zzz") {
			t.Fatal("detail page leaks a secret")
		}
		if resp, _ := e.do("GET", "/monitor/nope", "admin", ""); resp.StatusCode != 404 {
			t.Fatal(resp.StatusCode)
		}
		for _, icon := range []string{"/static/icon-green.png", "/static/icon-red.png"} {
			if resp, _ := e.do("GET", icon, "", ""); resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" {
				t.Fatalf("%s: %d %s", icon, resp.StatusCode, resp.Header.Get("Content-Type"))
			}
		}
		if resp, b := e.do("GET", "/static/style.css", "", ""); resp.StatusCode != 200 || !strings.Contains(b, `[data-theme="dark"]`) {
			t.Fatal("static")
		}

		var vs []monitorView
		json.Unmarshal([]byte(e.get("/api/monitors?tag=public")), &vs)
		if len(vs) != 1 || vs[0].Name != "web" || vs[0].Reason != "HTTP 503" || vs[0].Uptime == nil || vs[0].Uptime.Pct() != "0.00%" || vs[0].LastCheck != nil {
			t.Fatalf("api list %+v", vs)
		}
		if json.Unmarshal([]byte(e.get("/api/monitors?tag=prod&tag=public")), &vs); len(vs) != 1 || vs[0].Name != "web" {
			t.Fatalf("api tags and %+v", vs)
		}
		json.Unmarshal([]byte(e.get("/api/monitors?name=internal")), &vs)
		if len(vs) != 1 || vs[0].Name != "internal" {
			t.Fatalf("api name filter %+v", vs)
		}
		if v := e.view("web"); v.LastCheck == nil || v.LastCheck.Reason != "HTTP 503" || !v.NextCheck.After(time.Now()) || v.NextCheck.Sub(v.LastCheck.Time) != time.Minute {
			t.Fatalf("api detail %+v", v)
		}
		for _, p := range []string{"/", "/monitor/web"} {
			if b := e.get(p); !regexp.MustCompile(`<time [^>]+ data-rel>(−\d+[smhd]|now)</time> → <time [^>]+ data-rel="next">(&#43;\d+[smhd]|now)</time>`).MatchString(b) {
				t.Fatalf("%s: no last/next check times: %s", p, b)
			}
		}
		if resp, _ := e.do("GET", "/api/monitors/nope", "admin", ""); resp.StatusCode != 404 {
			t.Fatal(resp.StatusCode)
		}

		before := e.view("web").LastCheck.Time
		sleep(time.Second)
		if resp, _ := e.post("/api/monitors/web/check", ""); resp.StatusCode != 200 {
			t.Fatal(resp.StatusCode)
		}
		if after := e.view("web").LastCheck.Time; !after.After(before) {
			t.Fatal("check now did not run")
		}
		if r := e.rows("web"); r[len(r)-1] != (row{time.Now().UTC().Truncate(time.Second), "check", "", "manual check"}) {
			t.Fatalf("no manual check row: %+v", r[len(r)-1])
		}

		form := []string{"Content-Type", "application/x-www-form-urlencoded"}
		resp, _ := e.post("/api/monitors/web/pause", "reason=maint+until+14:00", append(form, "Referer", "http://up/monitor/web?x=1")...)
		if resp.StatusCode != 303 || resp.Header.Get("Location") != "/monitor/web?x=1" {
			t.Fatalf("form post: %d %s", resp.StatusCode, resp.Header.Get("Location"))
		}
		if v := e.view("web"); !v.Paused || v.PauseReason != "maint until 14:00" || !v.PausedSince.Equal(time.Now()) {
			t.Fatalf("paused %+v", v)
		}
		if b := e.get("/monitor/web"); regexp.MustCompile(`\d\d:\d\d(:\d\d)?(</td>|</p>|<span| \(|,)`).MatchString(b) {
			t.Fatalf("timestamp without zone: %s", b)
		}
		if b := e.get("/monitor/web"); strings.Count(b, `<td class="ev">`) != len(e.rows("web")) || strings.Count(b, `<td class="ev"></td>`) != 0 ||
			!strings.Contains(b, `<td class="ev"><span class="ico" role="img" aria-label="paused" title="paused">❚❚`) {
			t.Fatalf("audit log icons: %s", b)
		}
		if b := e.get("/monitor/web"); !strings.Contains(b, `<p class="banner red">HTTP 503`) || !strings.Contains(b, `HTTP 503 <span class="muted">since <time`) ||
			!regexp.MustCompile(`<p class="banner pause">.+ maint until 14:00 <span class="muted">since <time [^>]+>now</time>`).MatchString(b) {
			t.Fatalf("detail banners: %s", b)
		}
		if b := e.get("/"); !strings.Contains(b, `<span class="st is-paused"><span class="dot red"`) {
			t.Fatal("paused monitor's light is not pale")
		}
		if b := e.get("/?state=paused"); !strings.Contains(b, `href="/monitor/web"`) || strings.Contains(b, `href="/monitor/internal"`) ||
			!strings.Contains(b, `<a class="chip on" href="/">`) || !strings.Contains(b, `aria-label="paused" title="paused">❚❚</span><span class="n">1</span>`) {
			t.Fatalf("paused filter: %s", b)
		}
		resp, _ = e.post("/api/monitors/internal/check", "", append(form, "Referer", "http://evil.example/steal")...)
		if resp.Header.Get("Location") != "/steal" {
			t.Fatalf("off-site redirect %s", resp.Header.Get("Location"))
		}
		resp, b = e.post("/api/monitors/internal/pause", `{"reason": "x"}`, "Content-Type", "application/json")
		var v monitorView
		if json.Unmarshal([]byte(b), &v); resp.StatusCode != 200 || !v.Paused || v.PauseReason != "x" {
			t.Fatalf("json pause %d %s", resp.StatusCode, b)
		}
		resp, b = e.post("/api/monitors/internal/resume", "")
		if json.Unmarshal([]byte(b), &v); resp.StatusCode != 200 || v.Paused {
			t.Fatalf("json resume %d %s", resp.StatusCode, b)
		}
		if b := e.get("/"); !strings.Contains(b, `data-red="1"`) {
			t.Fatal("paused monitor counted red")
		}

		for _, h := range [][]string{{"Sec-Fetch-Site", "cross-site"}, {"Origin", "http://evil.example"}} {
			if resp, _ := e.post("/api/monitors/web/resume", "", h...); resp.StatusCode != 403 {
				t.Fatalf("cross-origin %v: %d", h, resp.StatusCode)
			}
		}
		if !e.view("web").Paused {
			t.Fatal("cross-origin post had an effect")
		}

		resp, b = e.do("GET", "/status/public", "", "")
		if resp.StatusCode != 200 || !strings.Contains(b, "Example Status") || !strings.Contains(b, "web") || !strings.Contains(b, "red") ||
			!strings.Contains(b, "paused: maint until 14:00") || strings.Contains(b, "internal") ||
			!strings.Contains(b, `<li class="card red is-paused">`) || !strings.Contains(b, "<p>Shop front page answers</p>") ||
			strings.Contains(b, "503") || strings.Contains(b, "secret-path") || strings.Contains(b, "new monitor") {
			t.Fatalf("status page: %d %s", resp.StatusCode, b)
		}
		if _, b := e.do("GET", "/status/custom", "", ""); b != "custom:web=red,true,maint until 14:00;" {
			t.Fatalf("custom status page %q", b)
		}
		if resp, _ := e.do("GET", "/status/nope", "", ""); resp.StatusCode != 404 {
			t.Fatal(resp.StatusCode)
		}
	})
}

func TestEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		var status atomic.Int32
		status.Store(500)
		e.serve("target:80", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(int(status.Load())) }, nil)
		e.start(e.cfg(`"monitors": [{"name": "m", "type": "http", "interval": "1h", "http": {"url": "http://target/"}}]`))

		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://up/events", nil)
		req.SetBasicAuth("admin", "pw")
		resp, err := e.client.Do(req)
		if err != nil || resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatal(err, resp.StatusCode)
		}
		evs := make(chan event, 100)
		go func() {
			s := bufio.NewScanner(resp.Body)
			for s.Scan() {
				if d, ok := strings.CutPrefix(s.Text(), "data: "); ok {
					var ev event
					json.Unmarshal([]byte(d), &ev)
					evs <- ev
				}
			}
			resp.Body.Close()
			close(evs)
		}()
		next := func() []event {
			synctest.Wait()
			var out []event
			for {
				select {
				case ev := <-evs:
					out = append(out, ev)
				default:
					return out
				}
			}
		}

		e.post("/api/monitors/m/check", "")
		if got := next(); len(got) != 2 || got[0] != (event{Type: "checking", Monitor: "m"}) || got[1] != (event{Type: "check", Monitor: "m", State: red}) {
			t.Fatalf("check events %+v", got)
		}
		status.Store(200)
		e.post("/api/monitors/m/check", "")
		if got := next(); len(got) != 3 || got[2] != (event{Type: "state", Monitor: "m", Old: red, State: green, Important: true}) {
			t.Fatalf("state events %+v", got)
		}
		sleep(2 * time.Minute)
		e.post("/api/monitors/m/pause", "")
		e.post("/api/monitors/m/resume", "")
		if got := next(); len(got) != 2 || got[0].Type != "paused" || got[1].Type != "resumed" {
			t.Fatalf("pause events %+v", got)
		}
		cancel()
		for range evs {
		}
	})
}

func TestEventsPage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		var status atomic.Int32
		status.Store(500)
		e.serve("target:80", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(int(status.Load())) }, nil)
		e.start(e.cfg(`"monitors": [
			{"name": "m", "type": "http", "tags": ["a"], "interval": "1h", "http": {"url": "http://target/"}},
			{"name": "n", "type": "http", "tags": ["b"], "interval": "1h", "http": {"url": "http://target/"}}]`))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stream := func(path string) func() []string {
			req, _ := http.NewRequestWithContext(ctx, "GET", "http://up/events?"+path, nil)
			req.SetBasicAuth("admin", "pw")
			resp, err := e.client.Do(req)
			if err != nil || resp.StatusCode != 200 {
				t.Fatal(err)
			}
			evs := make(chan string, 100)
			go func() {
				defer resp.Body.Close()
				s, typ := bufio.NewScanner(resp.Body), "message"
				s.Buffer(nil, 1<<20)
				for s.Scan() {
					if v, ok := strings.CutPrefix(s.Text(), "event: "); ok {
						typ = v
					} else if v, ok := strings.CutPrefix(s.Text(), "data: "); ok && typ != "" {
						evs <- typ + " " + v[:min(len(v), 40)]
						typ = ""
					} else if s.Text() == "" {
						typ = "message"
					}
				}
			}()
			return func() []string {
				synctest.Wait()
				var out []string
				for {
					select {
					case ev := <-evs:
						out = append(out, ev)
					default:
						return out
					}
				}
			}
		}
		list, red, detail := stream("path=/&tag=a"), stream("path=/&state=red"), stream("path=/monitor/n")
		for _, next := range []func() []string{list, red, detail} {
			if got := next(); len(got) != 1 || !strings.HasPrefix(got[0], "main <main") {
				t.Fatalf("initial %q", got)
			}
		}
		has := func(got []string, prefix string) bool {
			return slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, prefix) })
		}
		mRow := `row {"html":"\u003ctr data-href=\"/monitor/m`
		e.post("/api/monitors/m/check", "")
		if got := list(); !has(got, mRow) || !has(got, "head ") || has(got, "message ") {

			t.Fatalf("list %q", got)
		}
		if got := red(); !has(got, mRow) {
			t.Fatalf("red list %q", got)
		}
		if got := detail(); len(got) != 0 {
			t.Fatalf("detail page of n got events of m: %q", got)
		}
		e.post("/api/monitors/n/check", "")
		if got := list(); len(got) != 0 {
			t.Fatalf("list filtered to tag a got events of n: %q", got)
		}
		if got := detail(); !has(got, "main <main") || has(got, `message {"type":"checking"`) {
			t.Fatalf("detail %q", got)
		}
		status.Store(200)
		e.post("/api/monitors/m/check", "")
		if got := red(); !has(got, `row {"html":"","name":"m"}`) || !has(got, `message {"type":"state","monitor":"m","state":"green","old":"red","important":true}`[:40]) {
			t.Fatalf("red list after m recovered %q", got)
		}
		e.post("/api/monitors/m/check", "")
		if got := red(); has(got, "row ") {
			t.Fatalf("red list got a row it doesn't have %q", got)
		}
	})
}

func TestHeartbeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		var hits, peer atomic.Int32
		e.serve("hc:80", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/ping/uuid" && r.Method == "GET" {
				hits.Add(1)
			}
		}, nil)
		e.serve("peer:80", func(w http.ResponseWriter, r *http.Request) { peer.Add(1) }, nil)
		e.start(e.cfg(`"heartbeat": {"urls": ["http://hc/ping/uuid", "http://peer/push/a/t"], "interval": "1m"}, "monitors": []`))
		sleep(3*time.Minute + time.Second)
		if n, p := hits.Load(), peer.Load(); n != 4 || p != 4 {
			t.Fatalf("%d/%d heartbeats, want 4/4", n, p)
		}
	})
}

func TestRel(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		d    time.Duration
		next bool
		want string
	}{{0, false, "now"}, {-12 * time.Second, false, "−12s"}, {-12 * time.Second, true, "now"}, {4*time.Minute + 30*time.Second, true, "+4m"},
		{3 * time.Hour, false, "+3h"}, {-72 * time.Hour, false, "−3d"}} {
		if got := rel(now.Add(c.d), c.next); got != c.want {
			t.Errorf("rel(%v, %v) = %q, want %q", c.d, c.next, got, c.want)
		}
	}
}

func TestWorst(t *testing.T) {
	v := func(state string, paused bool) monitorView { return monitorView{State: state, Paused: paused} }
	for want, vs := range map[string][]monitorView{
		green:  {v(green, false), v(red, true)},
		yellow: {v(green, false), v(yellow, false), v(red, true)},
		red:    {v(yellow, false), v(red, false), v(green, false)},
	} {
		if got := worst(vs); got != want {
			t.Errorf("worst = %s, want %s", got, want)
		}
	}
	if got := worst(nil); got != green {
		t.Errorf("no monitors: %s", got)
	}
}
