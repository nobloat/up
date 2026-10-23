package main

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func main() { os.Exit(cli(os.Args[1:])) }

var release string

var version = sync.OnceValue(func() string {
	v := cmp.Or(release, "main")
	if bi, ok := debug.ReadBuildInfo(); ok {
		rev, dirty := "", false
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value[:min(7, len(s.Value))]
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if rev != "" {
			if dirty {
				rev += "+dirty"
			}
			v += " (" + rev + ")"
		}
	}
	return v
})

func cli(args []string) int {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	path := fs.String("config", "config", "config directory")
	check := fs.Bool("check", false, "validate the config and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfig(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *check {
		for _, err := range cfg.errs {
			fmt.Fprintln(os.Stderr, err)
		}
		return min(len(cfg.errs), 1)
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
	}()
	if err := run(ctx, *path, ln, nil); err != nil {
		slog.Error("up", "err", err)
		return 1
	}
	return 0
}

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

type App struct {
	path    string
	dataDir string
	dial    dialFunc
	ctx     context.Context
	cfg     atomic.Pointer[Config]

	mu       sync.Mutex
	monitors map[string]*Monitor
	failing  map[string]failure

	subMu sync.Mutex
	subs  map[*sub]struct{}
}

func run(ctx context.Context, path string, ln net.Listener, dial dialFunc) error {
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	cfg.logErrs()
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	a := &App{path: path, dataDir: cfg.DataDir, dial: dial, ctx: ctx,
		monitors: map[string]*Monitor{}, failing: map[string]failure{}, subs: map[*sub]struct{}{}}

	slog.Info("up started", "version", version(), "url", listenURL(ln.Addr().String()), "monitors", len(cfg.Monitors))
	if err := a.apply(ctx, cfg); err != nil {
		ln.Close()
		return err
	}

	srv := &http.Server{Handler: a.handler(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	var wg sync.WaitGroup
	wg.Go(func() { a.evalLoop(ctx) })
	wg.Go(func() { a.heartbeatLoop(ctx) })
	wg.Go(func() { a.reloadLoop(ctx) })

	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	slog.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
	wg.Wait()
	a.mu.Lock()
	for _, m := range a.monitors {
		m.retire("up stopped")
	}
	a.mu.Unlock()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	slog.Info("up stopped")
	return err
}

func (a *App) apply(ctx context.Context, cfg *Config) error {
	if old := a.cfg.Load(); old != nil && (old.Listen != cfg.Listen || old.DataDir != cfg.DataDir) {
		slog.Warn("listen and data_dir changes need a restart")
	}
	why := "added by config reload"
	if a.cfg.Load() == nil {
		why = "up " + version() + " started"
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	logs := map[string][]row{}
	for _, mc := range cfg.Monitors {
		if a.monitors[mc.Name] == nil {
			rows, err := readLog(monitorPath(a.dataDir, mc.Name))
			if err != nil {
				return fmt.Errorf("monitor %s: reading audit log: %w", mc.Name, err)
			}
			logs[mc.Name] = rows
		}
	}
	a.cfg.Store(cfg)
	keep := map[string]bool{}
	for _, mc := range cfg.Monitors {
		keep[mc.Name] = true
		m := a.monitors[mc.Name]
		if m == nil {
			m = newMonitor(a, mc, why, logs[mc.Name])
			a.monitors[mc.Name] = m
			m.start(ctx)
			continue
		}
		typeChanged := m.config().Type != mc.Type
		if typeChanged {
			m.halt()
		}
		m.mu.Lock()
		m.cfg = mc
		m.mu.Unlock()
		if typeChanged {
			m.start(ctx)
		} else if mc.Type == "push" {
			m.checkNow()
		}
	}
	for name, m := range a.monitors {
		if !keep[name] {
			m.retire("removed by config reload")
			delete(a.monitors, name)
		}
	}
	return nil
}

func (a *App) monitor(name string) *Monitor {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.monitors[name]
}

type event struct {
	Type      string `json:"type"`
	Monitor   string `json:"monitor"`
	State     string `json:"state"`
	Old       string `json:"old,omitempty"`
	Important bool   `json:"important,omitempty"`
}

type sub struct {
	mu     sync.Mutex
	events []event
	dirty  map[string]bool
	wake   chan struct{}
}

func (a *App) publish(e event) {
	a.subMu.Lock()
	defer a.subMu.Unlock()
	for s := range a.subs {
		s.mu.Lock()
		if len(s.events) < 256 {
			s.events = append(s.events, e)
		}
		s.dirty[e.Monitor] = true
		s.mu.Unlock()
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

func (s *sub) take() ([]event, map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	evs, dirty := s.events, s.dirty
	s.events, s.dirty = nil, map[string]bool{}
	return evs, dirty
}

func (a *App) subscribe() *sub {
	s := &sub{dirty: map[string]bool{}, wake: make(chan struct{}, 1)}
	a.subMu.Lock()
	a.subs[s] = struct{}{}
	a.subMu.Unlock()
	return s
}

func (a *App) unsubscribe(s *sub) {
	a.subMu.Lock()
	delete(a.subs, s)
	a.subMu.Unlock()
}

func listenURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr + "/"
	}
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

func (a *App) transport(skipVerify bool) *http.Transport {
	t := &http.Transport{DialContext: a.dial, DisableKeepAlives: true,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: skipVerify}}
	if p := a.cfg.Load().proxy; p != nil {
		t.Proxy = http.ProxyURL(p)
	}
	return t
}
