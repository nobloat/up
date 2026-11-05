package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type manifest struct {
	App     string      `json:"app"`
	Version string      `json:"version"`
	Created time.Time   `json:"created"`
	Since   *time.Time  `json:"since,omitempty"`
	Files   []fileEntry `json:"files"`
}

type fileEntry struct {
	Path   string    `json:"path"`
	Size   int64     `json:"size"`
	Mtime  time.Time `json:"mtime"`
	SHA256 string    `json:"sha256"`

	src string
}

// skipped: temp files, caches and the transaction record are never backed up.
func skipped(name string) bool {
	return strings.HasPrefix(name, ".tmp-") || strings.HasSuffix(name, ".cache.json") || name == ".transaction.json"
}

func hashFile(p string) (int64, string, error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return n, hex.EncodeToString(h.Sum(nil)), err
}

// exportFiles lists config/<name> for every file of the config dir (not recursive, as loadConfig reads it) and data/<rel> for every file of the data dir.
func exportFiles(cfgDir, dataDir string) ([]fileEntry, error) {
	var files []fileEntry
	add := func(name, src string, d fs.DirEntry) error {
		if !d.Type().IsRegular() || skipped(d.Name()) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		size, sum, err := hashFile(src)
		if err != nil {
			return err
		}
		files = append(files, fileEntry{Path: name, Size: size, Mtime: fi.ModTime().UTC(), SHA256: sum, src: src})
		return nil
	}
	des, err := os.ReadDir(cfgDir)
	if err != nil {
		return nil, err
	}
	for _, d := range des {
		if err := add("config/"+d.Name(), filepath.Join(cfgDir, d.Name()), d); err != nil {
			return nil, err
		}
	}
	err = filepath.WalkDir(dataDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dataDir, p)
		if err != nil {
			return err
		}
		return add("data/"+filepath.ToSlash(rel), p, d)
	})
	slices.SortFunc(files, func(a, b fileEntry) int { return strings.Compare(a.Path, b.Path) })
	return files, err
}

// writeExport writes a tar.gz: manifest.json listing every file, then the files changed since since (all when zero).
// The caller holds a.store so no write lands in between. The gzip trailer is only written on success, so a failed export never reads as complete.
func writeExport(w io.Writer, cfgDir, dataDir string, since time.Time) error {
	files, err := exportFiles(cfgDir, dataDir)
	if err != nil {
		return err
	}
	m := manifest{App: "up", Version: version(), Created: time.Now().UTC(), Files: files}
	if !since.IsZero() {
		m.Since = &since
	}
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	mb = append(mb, '\n')
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(mb)), ModTime: m.Created}); err != nil {
		return err
	}
	if _, err := tw.Write(mb); err != nil {
		return err
	}
	// file clocks are coarser than time.Now (a tick of a few ms): a second of overlap re-sends a few unchanged files instead of missing a changed one
	cut := since.Add(-time.Second)
	for _, f := range files {
		if !since.IsZero() && !f.Mtime.After(cut) {
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Name: f.Path, Mode: 0o600, Size: f.Size, ModTime: f.Mtime}); err != nil {
			return err
		}
		src, err := os.Open(f.src)
		if err != nil {
			return err
		}
		_, err = io.CopyN(tw, src, f.Size)
		src.Close()
		if err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// apiExport streams the config and data dirs as one consistent tar.gz; ?since=<a manifest's created> sends only what changed.
// ponytail: writes (audit appends, conf saves) wait for the whole download; fine for a few MB, stage to a temp file if it grows
func (a *App) apiExport(w http.ResponseWriter, r *http.Request) {
	if !need(w, r, admin) {
		return
	}
	var since time.Time
	if s := r.URL.Query().Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_since"})
			return
		}
		since = t
	}
	http.NewResponseController(w).SetWriteDeadline(time.Time{})
	a.store.Lock()
	defer a.store.Unlock()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="up-`+time.Now().Format("2006-01-02")+`.tgz"`)
	if err := writeExport(w, a.path, a.dataDir, since); err != nil {
		slog.Error("export", "err", err)
	}
}

func cliExport(args []string) int {
	fs := flag.NewFlagSet("up export", flag.ContinueOnError)
	base := fs.String("url", os.Getenv("UP_URL"), "URL of the running up (env UP_URL)")
	user := fs.String("user", os.Getenv("UP_USER"), "basic auth user (env UP_USER)")
	pw := fs.String("password", os.Getenv("UP_PASSWORD"), "basic auth password (env UP_PASSWORD)")
	since := fs.String("since", "", "only files changed since this time (a previous manifest's created)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *base == "" || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: up export -url <url> [-user u -password p] [-since <time>] > backup.tgz")
		return 2
	}
	u := strings.TrimRight(*base, "/") + "/api/export"
	if *since != "" {
		u += "?since=" + url.QueryEscape(*since)
	}
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *user != "" {
		req.SetBasicAuth(*user, *pw)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		fmt.Fprintf(os.Stderr, "%s: %s %s\n", u, resp.Status, strings.TrimSpace(string(b)))
		return 1
	}
	if _, err := io.Copy(os.Stdout, resp.Body); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func cliRestore(args []string) int {
	fs := flag.NewFlagSet("up restore", flag.ContinueOnError)
	data := fs.String("data", "", "data directory to restore into, must be empty or missing")
	cfg := fs.String("config", "", "config directory to restore the config into (skipped when not given)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *data == "" || fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: up restore -data <dir> [-config <dir>] <full.tgz> [<incremental.tgz>...]")
		return 2
	}
	n, errs := restoreArchives(*data, *cfg, fs.Args())
	for _, err := range errs {
		fmt.Fprintln(os.Stderr, err)
	}
	if len(errs) > 0 {
		return 1
	}
	fmt.Printf("restored %d files from %d archives into %s, checked\n", n, fs.NArg(), *data)
	return 0
}

// restoreArchives applies a full export and its increments in order, verifies the last manifest and runs check.
func restoreArchives(data, cfg string, archives []string) (int, []error) {
	if des, err := os.ReadDir(data); err == nil && len(des) > 0 {
		return 0, []error{fmt.Errorf("%s is not empty", data)}
	}
	if err := os.MkdirAll(data, 0o700); err != nil {
		return 0, []error{err}
	}
	if cfg != "" {
		if err := os.MkdirAll(cfg, 0o700); err != nil {
			return 0, []error{err}
		}
	}
	ours := map[string]bool{} // config files this restore wrote; older ones are never overwritten
	var m *manifest
	for _, a := range archives {
		var err error
		if m, err = applyArchive(a, data, cfg, ours); err != nil {
			return 0, []error{fmt.Errorf("%s: %w", a, err)}
		}
	}
	var errs []error
	n := 0
	for _, f := range m.Files {
		p, ok := localPath(f.Path, data, cfg)
		if !ok || p == "" {
			continue
		}
		size, sum, err := hashFile(p)
		if err != nil || size != f.Size || sum != f.SHA256 {
			errs = append(errs, fmt.Errorf("%s: does not match the manifest (%v)", f.Path, err))
		}
		n++
	}
	errs = append(errs, checkData(data)...)
	if cfg != "" {
		c, err := loadConfig(cfg)
		if err != nil {
			return n, append(errs, err)
		}
		errs = append(errs, c.errs...)
	}
	return n, errs
}

// localPath maps an archive path to the restore target; "" for config files without -config, ok false for anything unsafe.
func localPath(name, data, cfg string) (string, bool) {
	if path.Clean(name) != name || !filepath.IsLocal(filepath.FromSlash(name)) || strings.Contains(name, `\`) {
		return "", false
	}
	if rest, ok := strings.CutPrefix(name, "data/"); ok {
		return filepath.Join(data, filepath.FromSlash(rest)), true
	}
	if rest, ok := strings.CutPrefix(name, "config/"); ok && !strings.Contains(rest, "/") {
		if cfg == "" {
			return "", true
		}
		return filepath.Join(cfg, rest), true
	}
	return "", false
}

func applyArchive(archive, data, cfg string, ours map[string]bool) (*manifest, error) {
	f, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	h, err := tr.Next()
	if err != nil || h.Name != "manifest.json" {
		return nil, errors.New("manifest.json must come first")
	}
	var m manifest
	dec := json.NewDecoder(tr)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest.json: %w", err)
	}
	if m.App != "up" {
		return nil, fmt.Errorf("manifest.json: app %q, want up", m.App)
	}
	keep := map[string]bool{}
	for _, e := range m.Files {
		p, ok := localPath(e.Path, data, cfg)
		if !ok {
			return nil, fmt.Errorf("manifest.json: unsafe path %q", e.Path)
		}
		keep[p] = true
	}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		p, ok := localPath(h.Name, data, cfg)
		if !ok || h.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("unsafe entry %q", h.Name)
		}
		if p == "" {
			continue
		}
		if strings.HasPrefix(h.Name, "config/") {
			if _, err := os.Lstat(p); err == nil && !ours[p] {
				return nil, fmt.Errorf("%s exists, not overwriting it", p)
			}
			ours[p] = true
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return nil, err
		}
		out, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, err
		}
		_, err = io.Copy(out, tr)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, err
		}
	}
	// files gone from the manifest were deleted after the previous archive
	err = filepath.WalkDir(data, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !keep[p] {
			err = os.Remove(p)
		}
		return err
	})
	for p := range ours {
		if !keep[p] {
			delete(ours, p)
			err = errors.Join(err, os.Remove(p))
		}
	}
	return &m, err
}
