package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

type HTTPConfig struct {
	Method           string            `json:"method,omitempty"`
	URL              string            `json:"url"`
	Headers          map[string]string `json:"headers,omitempty"`
	BasicAuth        *Credentials      `json:"basic_auth,omitempty"`
	Body             string            `json:"body,omitempty"`
	AcceptedStatus   []int             `json:"accepted_status,omitempty"`
	BodyRegex        string            `json:"body_regex,omitempty"`
	JSONMatch        *JSONMatch        `json:"json_match,omitempty"`
	SkipVerify       bool              `json:"skip_verify,omitempty"`
	MaxRedirects     *int              `json:"max_redirects"`
	CertMinValidDays int               `json:"cert_min_valid_days,omitempty"`

	bodyRegex *regexp.Regexp
	equals    any
}

type JSONMatch struct {
	Path   string          `json:"path"`
	Equals json.RawMessage `json:"equals"`
}

type httpMonitor struct{}

func (httpMonitor) validate(c *MonitorConfig) error { return c.HTTP.validate() }

func (h *HTTPConfig) validate() error {
	if h.Method == "" {
		h.Method = "GET"
	}
	if !slices.Contains([]string{"GET", "POST", "HEAD"}, h.Method) {
		return fmt.Errorf("method must be GET, POST or HEAD, got %q", h.Method)
	}
	if err := checkURL(h.URL); err != nil {
		return err
	}
	for _, s := range h.AcceptedStatus {
		if s < 100 || s > 599 {
			return fmt.Errorf("invalid accepted status %d", s)
		}
	}
	if h.BodyRegex != "" {
		re, err := regexp.Compile(h.BodyRegex)
		if err != nil {
			return err
		}
		h.bodyRegex = re
	}
	if j := h.JSONMatch; j != nil {
		if j.Path == "" || json.Unmarshal(j.Equals, &h.equals) != nil {
			return errors.New("json_match needs a path and a JSON equals value")
		}
	}
	if h.MaxRedirects == nil {
		h.MaxRedirects = new(10)
	}
	if *h.MaxRedirects < 0 || h.CertMinValidDays < 0 {
		return errors.New("max_redirects and cert_min_valid_days must be >= 0")
	}
	return nil
}

func (httpMonitor) run(ctx context.Context, m *Monitor) {
	runChecks(ctx, m, func(ctx context.Context, cfg *MonitorConfig) (bool, bool, string, []sample) {
		ok, fatal, reason, _ := check(ctx, m.app, cfg.HTTP, cfg.Timeout.D())
		return ok, fatal, reason, nil
	})
}

func runChecks(ctx context.Context, m *Monitor, check func(context.Context, *MonitorConfig) (ok, fatal bool, reason string, samples []sample)) {
	var delay time.Duration
	for {
		m.setNext(time.Now().Add(delay))
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		case <-m.wake:
		}
		cfg := m.config()
		m.startCheck()
		begin := time.Now()
		ok, fatal, reason, samples := check(ctx, cfg)
		if ctx.Err() != nil {
			return
		}
		m.record(checkResult{begin, time.Since(begin).Round(time.Millisecond), ok, reason, samples}, fatal)
		delay = m.retryDelay()
	}
}

func redirectPolicy(limit int, private bool) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) > limit {
			return fmt.Errorf("more than %d redirects", limit)
		}
		previous, first := via[len(via)-1].URL, via[0].URL
		if previous.Scheme == "https" && req.URL.Scheme != "https" {
			return errors.New("HTTPS redirect downgrade refused")
		}
		if private && (req.URL.Scheme != first.Scheme || !strings.EqualFold(req.URL.Host, first.Host)) {
			return errors.New("cross-origin redirect with private request data refused")
		}
		return nil
	}
}

func check(ctx context.Context, a *App, h *HTTPConfig, timeout time.Duration) (ok, fatal bool, reason string, body []byte) {
	req, err := http.NewRequestWithContext(ctx, h.Method, h.URL, strings.NewReader(h.Body))
	if err != nil {
		return false, false, errText(err), nil
	}
	for k, v := range h.Headers {
		req.Header.Set(k, v)
		if strings.EqualFold(k, "Host") {
			req.Host = v
		}
	}
	if h.BasicAuth != nil {
		req.SetBasicAuth(h.BasicAuth.User, h.BasicAuth.Password)
	}
	private := h.BasicAuth != nil || len(h.Headers) > 0 || h.Body != "" || req.URL.User != nil || req.URL.RawQuery != ""
	client := &http.Client{Transport: a.transport(h.SkipVerify), Timeout: timeout,
		CheckRedirect: redirectPolicy(*h.MaxRedirects, private)}
	resp, err := client.Do(req)
	if err != nil {
		return false, false, errText(err), nil
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, false, err.Error(), nil
	}
	if h.CertMinValidDays > 0 && resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		days := int(time.Until(resp.TLS.PeerCertificates[0].NotAfter).Hours() / 24)
		if days < h.CertMinValidDays {
			return false, true, fmt.Sprintf("cert expires in %dd", days), nil
		}
	}
	status := "HTTP " + strconv.Itoa(resp.StatusCode)
	if h.AcceptedStatus == nil && resp.StatusCode/100 != 2 || h.AcceptedStatus != nil && !slices.Contains(h.AcceptedStatus, resp.StatusCode) {
		return false, false, status, nil
	}
	if h.bodyRegex != nil && !h.bodyRegex.Match(body) {
		return false, false, "body does not match " + h.BodyRegex, nil
	}
	if j := h.JSONMatch; j != nil {
		var v any
		if err := json.Unmarshal(body, &v); err != nil {
			return false, false, "invalid JSON: " + err.Error(), nil
		}
		got, found := jsonPath(v, j.Path)
		if !found || !reflect.DeepEqual(got, h.equals) {
			g, _ := json.Marshal(got)
			return false, false, fmt.Sprintf("%s is %s, want %s", j.Path, g, j.Equals), nil
		}
	}
	return true, false, status, body
}

func errText(err error) string {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return ue.Err.Error()
	}
	return err.Error()
}

func jsonPath(v any, path string) (any, bool) {
	for _, k := range strings.Split(path, ".") {
		switch t := v.(type) {
		case map[string]any:
			var ok bool
			if v, ok = t[k]; !ok {
				return nil, false
			}
		case []any:
			i, err := strconv.Atoi(k)
			if err != nil || i < 0 || i >= len(t) {
				return nil, false
			}
			v = t[i]
		default:
			return nil, false
		}
	}
	return v, true
}
