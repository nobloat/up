package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// seed writes a config dir with a push monitor and a data dir with audit logs, a temp file and a cache.
func seed(t *testing.T) (e *env, data string) {
	e = newEnv(t)
	e.writeConfig(e.cfg(`"monitors": [{"name": "p", "type": "push", "interval": "1h", "push": {"token": "0123456789abcdef"}}]`))
	data = filepath.Join(e.dir, "data")
	os.MkdirAll(data, 0o700)
	for _, name := range []string{"p", "q", "gone"} {
		for _, ev := range []string{"start", "stop"} {
			if err := appendRow(e.csvPath(name), row{Time: time.Now(), Event: ev, Message: "x"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	os.WriteFile(filepath.Join(data, ".tmp-1"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(data, "a.cache.json"), []byte("{}"), 0o600)
	os.WriteFile(filepath.Join(e.dir, ".tmp-2"), []byte("x"), 0o600)
	old := time.Now().Add(-time.Hour) // older than the overlap of an incremental export
	for _, d := range []string{e.dir, data} {
		des, _ := os.ReadDir(d)
		for _, de := range des {
			os.Chtimes(filepath.Join(d, de.Name()), old, old)
		}
	}
	return e, data
}

func export(t *testing.T, cfg, data string, since time.Time) (string, manifest, []string) {
	t.Helper()
	var b bytes.Buffer
	if err := writeExport(&b, cfg, data, since); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "x.tgz")
	os.WriteFile(p, b.Bytes(), 0o600)
	gz, err := gzip.NewReader(&b)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	var m manifest
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		if h.Name == "manifest.json" {
			json.NewDecoder(tr).Decode(&m)
		}
	}
	return p, m, names
}

// sums maps every backed-up file of dir to its sha256.
func sums(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	des, _ := os.ReadDir(dir)
	for _, d := range des {
		if !d.IsDir() && !skipped(d.Name()) {
			_, s, err := hashFile(filepath.Join(dir, d.Name()))
			if err != nil {
				t.Fatal(err)
			}
			out[d.Name()] = s
		}
	}
	return out
}

func TestExportRestoreDrill(t *testing.T) {
	e, data := seed(t)
	arch, m, names := export(t, e.dir, data, time.Time{})
	if names[0] != "manifest.json" || len(names) != len(m.Files)+1 || m.App != "up" || m.Since != nil {
		t.Fatalf("entries %q, manifest %+v", names, m)
	}
	for _, n := range names {
		if strings.Contains(n, ".tmp-") || strings.Contains(n, ".cache.json") {
			t.Fatalf("exported %s", n)
		}
	}
	if !slices.Contains(names, "config/up.conf") || !slices.Contains(names, "data/p.csv") || !slices.IsSorted(names[1:]) {
		t.Fatalf("entries %q", names)
	}
	dst, cfg := filepath.Join(t.TempDir(), "data"), filepath.Join(t.TempDir(), "config")
	if got := cli([]string{"restore", "-data", dst, "-config", cfg, arch}); got != 0 {
		t.Fatalf("restore: exit %d", got)
	}
	if !maps.Equal(sums(t, data), sums(t, dst)) || !maps.Equal(sums(t, e.dir), sums(t, cfg)) {
		t.Fatalf("restored data differs: %v %v", sums(t, data), sums(t, dst))
	}
	if got := cli([]string{"restore", "-data", dst, arch}); got != 1 {
		t.Fatalf("restore into a non-empty dir: exit %d", got)
	}
	if _, errs := restoreArchives(t.TempDir(), cfg, []string{arch}); len(errs) == 0 || !strings.Contains(errs[0].Error(), "not overwriting") {
		t.Fatalf("overwrote config: %v", errs)
	}
}

func TestExportIncremental(t *testing.T) {
	e, data := seed(t)
	full, fm, _ := export(t, e.dir, data, time.Time{})
	appendRow(e.csvPath("p"), row{Time: time.Now(), Event: "start", Message: "y"})
	os.Remove(e.csvPath("gone"))
	inc, m, names := export(t, e.dir, data, fm.Created)
	if want := []string{"manifest.json", "data/p.csv"}; !slices.Equal(names, want) || m.Since == nil || !m.Since.Equal(fm.Created) || len(m.Files) != len(fm.Files)-1 {
		t.Fatalf("incremental entries %q, manifest %+v", names, m)
	}
	dst := filepath.Join(t.TempDir(), "data")
	if n, errs := restoreArchives(dst, "", []string{full, inc}); len(errs) > 0 || n != 2 {
		t.Fatal(n, errs)
	}
	if !maps.Equal(sums(t, data), sums(t, dst)) {
		t.Fatalf("restored %v, want %v", sums(t, dst), sums(t, data))
	}
}

func TestRestoreRejectsTraversal(t *testing.T) {
	for _, name := range []string{"data/../x", "/etc/x", "config/a/b", "other/x", `data\..\x`} {
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		tw := tar.NewWriter(gz)
		mb, _ := json.Marshal(manifest{App: "up"})
		tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(mb))})
		tw.Write(mb)
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: 1})
		tw.Write([]byte("x"))
		tw.Close()
		gz.Close()
		p := filepath.Join(t.TempDir(), "x.tgz")
		os.WriteFile(p, b.Bytes(), 0o600)
		if _, errs := restoreArchives(filepath.Join(t.TempDir(), "d"), t.TempDir(), []string{p}); len(errs) == 0 || !strings.Contains(errs[0].Error(), "unsafe") {
			t.Errorf("%s: %v", name, errs)
		}
	}
}

func TestAuditAppendOnly(t *testing.T) {
	e := newEnv(t)
	e.writeConfig(e.cfg(`"monitors": [{"name": "p", "type": "push", "interval": "1h", "push": {"token": "0123456789abcdef"}}]`))
	p := e.csvPath("p")
	os.MkdirAll(filepath.Dir(p), 0o700)
	appendRow(p, row{Time: time.Now(), Event: "start", Message: "a"})
	first, _ := os.ReadFile(p)
	fi1, _ := os.Stat(p)
	appendRow(p, row{Time: time.Now(), Event: "stop", Message: "b"})
	second, _ := os.ReadFile(p)
	fi2, _ := os.Stat(p)
	if !bytes.HasPrefix(second, first) || len(second) <= len(first) || !os.SameFile(fi1, fi2) || len(e.rows("p")) != 2 {
		t.Fatalf("not appended:\n%s\n%s", first, second)
	}
	f, _ := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString("2026-11-05T00:00:00Z,state,red,torn")
	f.Close()
	if _, err := readLog(p); err == nil || !strings.Contains(err.Error(), p+":4: unterminated") {
		t.Fatalf("torn record: %v", err)
	}
	if got := cli([]string{"-check", "-config", e.dir}); got != 1 {
		t.Fatalf("check: exit %d", got)
	}
	if err := run(t.Context(), e.dir, e.net.listen("up:80"), e.net.dial); err == nil || !strings.Contains(err.Error(), "unterminated") {
		t.Fatalf("startup: %v", err)
	}
}

func TestExportNeedsAdmin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.start(e.cfg(`"trusted_proxies": ["10.0.0.0/8"], "role": {"editor": ["up-editor"], "admin": ["up-admin"]}, "monitors": []`))
		if resp, _ := e.do("GET", "/api/export", "", "", "Remote-User", "bob", "Remote-Groups", "up-editor"); resp.StatusCode != 403 {
			t.Fatalf("editor: %d", resp.StatusCode)
		}
		if resp, b := e.do("GET", "/api/export?since=yesterday", "admin", ""); resp.StatusCode != 400 || b != `{"error":"invalid_since"}`+"\n" {
			t.Fatalf("bad since: %d %s", resp.StatusCode, b)
		}
		resp, b := e.do("GET", "/api/export", "", "", "Remote-User", "alice", "Remote-Groups", "up-admin")
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/gzip" {
			t.Fatalf("admin: %d %s", resp.StatusCode, b)
		}
		gz, _ := gzip.NewReader(strings.NewReader(b))
		if h, err := tar.NewReader(gz).Next(); err != nil || h.Name != "manifest.json" {
			t.Fatalf("first entry: %v %v", h, err)
		}
	})
}
