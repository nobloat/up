package main

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"
)

type role int

const (
	none   role = iota
	viewer      // reads the monitors of their tags
	editor      // also checks, pauses and resumes them
	admin       // every monitor, notifier failures
)

var roleNames = map[string]role{"viewer": viewer, "editor": editor, "admin": admin}

func (r role) String() string { return [...]string{"none", "viewer", "editor", "admin"}[r] }

// User is who sent the request: basic auth (always admin) or the IdP headers of a trusted proxy.
type User struct {
	Name string
	Role role
	Tags []string // monitors with any of these tags are visible; "*" is all
	SSO  bool     // from the IdP headers: shown in the header with account and logout links
}

type ctxKey struct{}

func userOf(r *http.Request) *User { u, _ := r.Context().Value(ctxKey{}).(*User); return u }

// sees: admins and "*" see every monitor, everyone else the monitors sharing a tag with them.
func (u *User) sees(mc *MonitorConfig) bool {
	return u.Role >= admin || slices.Contains(u.Tags, "*") || slices.ContainsFunc(mc.Tags, func(t string) bool { return slices.Contains(u.Tags, t) })
}

func (a *App) auth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" && a.guesses.locked(a.clientIP(r)) {
			http.Error(w, "too many failed logins, try again later", http.StatusTooManyRequests)
			return
		}
		u := a.resolve(r)
		switch {
		case u == nil:
			if a.cfg.Load().Auth.User != "" {
				w.Header().Set("WWW-Authenticate", `Basic realm="][• up", charset="UTF-8"`)
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		case u.Role == none && r.Method == http.MethodGet && !wantsJSON(r) && !strings.HasPrefix(r.URL.Path, "/api/"):
			a.errPage(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)), http.StatusForbidden, "")
		case u.Role == none:
			http.Error(w, "forbidden", http.StatusForbidden)
		default:
			h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
		}
	})
}

func (a *App) resolve(r *http.Request) *User {
	c := a.cfg.Load()
	if r.Header.Get("Authorization") != "" { // never falls through to the IdP headers: the proxy lets these requests skip the IdP
		name, pw, ok := r.BasicAuth()
		if !ok || c.Auth.User == "" || subtle.ConstantTimeCompare([]byte(name), []byte(c.Auth.User))&subtle.ConstantTimeCompare([]byte(pw), []byte(c.Auth.Password)) != 1 {
			ip := a.clientIP(r)
			a.guesses.fail(ip)
			slog.Warn("basic auth failed", "client", ip, "user", name)
			return nil
		}
		return &User{Name: name, Role: admin}
	}
	if !a.fromProxy(r) {
		return nil
	}
	u := &User{Name: r.Header.Get("Remote-User"), SSO: true}
	if u.Name == "" {
		return nil
	}
	for g := range strings.SplitSeq(r.Header.Get("Remote-Groups"), ",") {
		if g = strings.TrimSpace(g); g == "" {
			continue
		}
		u.Tags = append(u.Tags, c.Access[g]...)
		for name, groups := range c.Roles {
			if slices.Contains(groups, g) {
				u.Role = max(u.Role, roleNames[name])
			}
		}
	}
	return u
}

// fromProxy: the direct peer is in trusted_proxies.
func (a *App) fromProxy(r *http.Request) bool {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip, err := netip.ParseAddr(host)
	return err == nil && slices.ContainsFunc(a.cfg.Load().TrustedProxies, func(p netip.Prefix) bool { return p.Contains(ip.Unmap()) })
}

// clientIP: the peer, or behind a trusted proxy the address it appended to X-Forwarded-For.
func (a *App) clientIP(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 && a.fromProxy(r) {
		parts := strings.Split(xff[len(xff)-1], ",")
		if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
			return ip
		}
	}
	return host
}

const (
	maxGuesses  = 10 // failed basic auth logins per client
	guessWindow = 10 * time.Minute
)

// guesses counts failed basic auth logins per client IP; at maxGuesses the client gets 429 until its window has passed.
type guesses struct {
	mu sync.Mutex
	m  map[string]strikes
}

type strikes struct {
	n     int
	start time.Time
}

func (g *guesses) locked(ip string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.m[ip]
	return s.n >= maxGuesses && time.Since(s.start) < guessWindow
}

// ponytail: prunes the whole map on every failure; fine for the few clients that get a password wrong
func (g *guesses) fail(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = map[string]strikes{}
	}
	maps.DeleteFunc(g.m, func(_ string, s strikes) bool { return time.Since(s.start) >= guessWindow })
	s := g.m[ip]
	if s.n == 0 {
		s.start = time.Now()
	}
	s.n++
	g.m[ip] = s
}

// need answers 403 and returns false when the user's role is below min.
func need(w http.ResponseWriter, r *http.Request, min role) bool {
	if userOf(r).Role >= min {
		return true
	}
	writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
	return false
}
