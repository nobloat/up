package main

import (
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"runtime/debug"
	"slices"
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
	if len(args) > 0 {
		switch args[0] {
		case "export":
			return cliExport(args[1:])
		case "restore":
			return cliRestore(args[1:])
		}
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfig(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *check {
		cfg.errs = append(cfg.errs, checkData(cfg.DataDir)...)
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
	dial    dialFunc // SMTP, heartbeats and the configured proxy
	guarded dialFunc // monitors and webhooks: never loopback, link-local or up itself
	self    listener
	ctx     context.Context
	cfg     atomic.Pointer[Config]

	store sync.RWMutex // every write to the data and config dirs holds RLock, an export holds Lock for a consistent cut

	mu       sync.Mutex
	monitors map[string]*Monitor
	failing  map[string]failure

	reloadMu sync.Mutex
	lastFP   string

	subMu sync.Mutex
	subs  map[*sub]struct{}

	guesses guesses
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
	a := &App{path: path, dataDir: cfg.DataDir, ctx: ctx,
		monitors: map[string]*Monitor{}, failing: map[string]failure{}, subs: map[*sub]struct{}{}}
	a.setDial(dial, ln.Addr())

	slog.Info("up started", "version", version(), "url", listenURL(ln.Addr().String()), "monitors", len(cfg.Monitors))
	if err := a.apply(ctx, cfg); err != nil {
		ln.Close()
		return err
	}
	a.lastFP = a.fingerprint()

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

	logs := map[string]*history{}
	for _, mc := range cfg.Monitors {
		if a.monitors[mc.Name] == nil {
			h, err := a.loadHistory(monitorPath(a.dataDir, mc.Name))
			if err != nil {
				return fmt.Errorf("monitor %s: reading audit log: %w", mc.Name, err)
			}
			logs[mc.Name] = h
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
		old := m.config()
		typeChanged := old.Type != mc.Type
		was, _ := json.Marshal(old)
		now, _ := json.Marshal(mc)
		if typeChanged {
			m.halt()
		}
		m.mu.Lock()
		m.cfg = mc
		m.mu.Unlock()
		if typeChanged {
			m.start(ctx)
		} else if string(was) != string(now) {
			m.checkNow() // a new URL or interval applies now, not after the old interval
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

// transport for one request; guarded refuses loopback, link-local and up's own listener, except through the configured proxy, which decides on its own.
func (a *App) transport(skipVerify, guarded bool) *http.Transport {
	t := &http.Transport{DialContext: a.dial, DisableKeepAlives: true,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: skipVerify}}
	if guarded {
		t.DialContext = a.guarded
	}
	if p := a.cfg.Load().proxy; p != nil {
		t.Proxy, t.DialContext = http.ProxyURL(p), a.dial
	}
	return t
}

// listener: up's own address. A monitor aimed at it would pass CSRF protection and, seen from a trusted proxy address, sign in with its own Remote-* headers.
type listener struct {
	port int
	ips  []netip.Addr
}

// setDial sets both dialers from dial (nil: the network); tests pass their own, used unguarded.
// ponytail: up's addresses are read at startup; one added later is not refused (loopback always is)
func (a *App) setDial(dial dialFunc, addr net.Addr) {
	a.dial, a.guarded = dial, dial
	if dial == nil {
		a.dial = (&net.Dialer{}).DialContext
		a.guarded = (&net.Dialer{Control: a.refuse}).DialContext
	}
	ta, ok := addr.(*net.TCPAddr)
	if !ok {
		return
	}
	a.self.port = ta.Port
	if !ta.IP.IsUnspecified() {
		a.self.ips = []netip.Addr{ta.AddrPort().Addr().Unmap()}
		return
	}
	addrs, _ := net.InterfaceAddrs()
	for _, ad := range addrs {
		if n, ok := ad.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok {
				a.self.ips = append(a.self.ips, ip.Unmap())
			}
		}
	}
}

// refuse checks every address the guarded dialer connects to, after DNS and on each redirect. Private networks stay allowed: monitors check the containers next to up.
func (a *App) refuse(network, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	ip := ap.Addr().Unmap().WithZone("")
	switch {
	case ip.IsLoopback() || ip.IsUnspecified():
		return fmt.Errorf("refused: %s is a loopback address", ip)
	case ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast():
		return fmt.Errorf("refused: %s is a link-local address", ip)
	case int(ap.Port()) == a.self.port && slices.Contains(a.self.ips, ip):
		return errors.New("refused: that is up itself")
	}
	return nil
}
