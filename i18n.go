package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
)

// i18n/<lang>.json are up's texts, i18n/ui/<lang>.json nobloat/ui's (copied unchanged by make ui); both are merged.
//
//go:embed i18n
var i18nFS embed.FS

var langs = []string{"en", "de"}

// texts: lang -> "area.key" -> text; an invalid file, a key in both files or a key missing in one language panics at startup.
var texts = func() map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, l := range langs {
		out[l] = map[string]string{}
		for _, f := range []string{"i18n/ui/" + l + ".json", "i18n/" + l + ".json"} {
			b, err := i18nFS.ReadFile(f)
			if err != nil {
				panic(err)
			}
			m := map[string]string{}
			if err := json.Unmarshal(b, &m); err != nil {
				panic(fmt.Sprintf("%s: %v", f, err))
			}
			for k, v := range m {
				if _, dup := out[l][k]; dup {
					panic(f + ": " + k + " is also in nobloat/ui's texts")
				}
				out[l][k] = v
			}
		}
	}
	for _, l := range langs[1:] {
		if a, b := slices.Sorted(maps.Keys(out[langs[0]])), slices.Sorted(maps.Keys(out[l])); !slices.Equal(a, b) {
			panic("i18n: " + langs[0] + " and " + l + " have different keys")
		}
	}
	return out
}()

// T translates key; args fill %s/%d verbs. An unknown key comes back as is.
func T(lang, key string, args ...any) string {
	s, ok := texts[lang][key]
	if !ok {
		s = key
	}
	if len(args) > 0 {
		return fmt.Sprintf(s, args...)
	}
	return s
}

// jsTexts: the texts ui.js and app.js read from the page.
func jsTexts(lang string) map[string]string {
	out := map[string]string{}
	for k, v := range texts[lang] {
		if strings.HasPrefix(k, "js.") || strings.HasPrefix(k, "search.") || strings.HasPrefix(k, "palette.") {
			out[k] = v
		}
	}
	return out
}

// langOf: the lang cookie (set by /lang/{l}), then Accept-Language, then English.
func langOf(r *http.Request) string {
	if c, err := r.Cookie("lang"); err == nil && slices.Contains(langs, c.Value) {
		return c.Value
	}
	for part := range strings.SplitSeq(r.Header.Get("Accept-Language"), ",") {
		tag, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		base, _, _ := strings.Cut(strings.ToLower(tag), "-")
		if slices.Contains(langs, base) {
			return base
		}
	}
	return langs[0]
}

// setLang stores the language and goes back to rd, a local path.
func setLang(w http.ResponseWriter, r *http.Request) {
	if l := r.PathValue("l"); slices.Contains(langs, l) {
		http.SetCookie(w, &http.Cookie{Name: "lang", Value: l, Path: "/", MaxAge: 365 * 86400, SameSite: http.SameSiteLaxMode, HttpOnly: true})
	}
	rd := r.FormValue("rd")
	if !strings.HasPrefix(rd, "/") || strings.HasPrefix(rd, "//") || strings.HasPrefix(rd, `/\`) {
		rd = "/"
	}
	http.Redirect(w, r, rd, http.StatusSeeOther)
}
