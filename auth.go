package main

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
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
		u := a.resolve(r)
		switch {
		case u == nil:
			if a.cfg.Load().Auth.User != "" {
				w.Header().Set("WWW-Authenticate", `Basic realm="][• up", charset="UTF-8"`)
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		case u.Role == none:
			http.Error(w, "forbidden", http.StatusForbidden)
		default:
			h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
		}
	})
}

func (a *App) resolve(r *http.Request) *User {
	c := a.cfg.Load()
	if name, pw, ok := r.BasicAuth(); ok {
		if c.Auth.User == "" || subtle.ConstantTimeCompare([]byte(name), []byte(c.Auth.User))&subtle.ConstantTimeCompare([]byte(pw), []byte(c.Auth.Password)) != 1 {
			return nil
		}
		return &User{Name: name, Role: admin}
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip, err := netip.ParseAddr(host)
	if err != nil || !slices.ContainsFunc(c.TrustedProxies, func(p netip.Prefix) bool { return p.Contains(ip.Unmap()) }) {
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

// need answers 403 and returns false when the user's role is below min.
func need(w http.ResponseWriter, r *http.Request, min role) bool {
	if userOf(r).Role >= min {
		return true
	}
	writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
	return false
}
