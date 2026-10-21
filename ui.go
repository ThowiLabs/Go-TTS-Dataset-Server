package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type PageTemplate = template.Template
type pageData struct {
	Title, Active, CSRF, Notice, Filter string
	Stats                               Stats
	Jobs                                []Job
	Workers                             []Worker
	Clients                             []Client
	IssuedToken, IssuedID               string
	Reference                           Reference
	HasReference                        bool
	ImportPreview                       *importPreview
	Page, Prev, Next                    int
	HasNext                             bool
}
type throttleEntry struct {
	Attempts int
	Until    time.Time
}
type loginThrottle struct {
	mu      sync.Mutex
	entries map[string]throttleEntry
}

func newLoginThrottle() *loginThrottle { return &loginThrottle{entries: map[string]throttleEntry{}} }
func (l *loginThrottle) check(key string, failed bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	e := l.entries[key]
	if now.After(e.Until) {
		e = throttleEntry{Until: now.Add(5 * time.Minute)}
	}
	if e.Attempts >= 8 {
		return false
	}
	if failed {
		e.Attempts++
	} else {
		delete(l.entries, key)
		return true
	}
	l.entries[key] = e
	return e.Attempts < 8
}
func loadTemplates() (map[string]*PageTemplate, error) {
	helpers := template.FuncMap{"date": func(n int64) string {
		if n <= 0 {
			return "—"
		}
		return time.Unix(n, 0).Local().Format("02/01/2006 15:04")
	},
		"percent": func(a, total int) int {
			if total == 0 {
				return 0
			}
			return a * 100 / total
		},
		"short": func(s string, n int) string {
			r := []rune(s)
			if len(r) > n {
				return string(r[:n]) + "…"
			}
			return s
		},
	}
	base, err := fs.ReadFile(webFS, "web/templates/layouts/main.html")
	if err != nil {
		return nil, err
	}
	pages := map[string]*PageTemplate{}
	for _, name := range []string{"dashboard", "jobs", "settings", "workers", "api-docs"} {
		contents, err := fs.ReadFile(webFS, "web/templates/content/"+name+".html")
		if err != nil {
			return nil, err
		}
		t, err := template.New("layout").Funcs(helpers).Parse(string(base))
		if err != nil {
			return nil, err
		}
		if _, err := t.Parse(string(contents)); err != nil {
			return nil, err
		}
		pages[name] = t
	}
	login, err := fs.ReadFile(webFS, "web/templates/layouts/login.html")
	if err != nil {
		return nil, err
	}
	t, err := template.New("login").Funcs(helpers).Parse(string(login))
	if err != nil {
		return nil, err
	}
	pages["login"] = t
	return pages, nil
}
func (a *App) render(w http.ResponseWriter, r *http.Request, name string, d pageData) {
	nonce, _ := a.session(r)
	d.CSRF = nonce
	if d.Notice == "" {
		d.Notice = r.URL.Query().Get("notice")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.templates[name].ExecuteTemplate(w, func() string {
		if name == "login" {
			return "login"
		}
		return "layout"
	}(), d); err != nil {
		log.Printf("template %s: %v", name, err)
	}
}
func (a *App) loginPage(w http.ResponseWriter, r *http.Request) {
	if a.authenticated(r) {
		http.Redirect(w, r, "/", 303)
		return
	}
	a.render(w, r, "login", pageData{Title: "Iniciar sesión"})
}
func (a *App) loginPost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Formulario inválido", 400)
		return
	}
	user := r.Form.Get("username")
	password := r.Form.Get("password")
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	hash := sha256.Sum256([]byte(host))
	key := hex.EncodeToString(hash[:])
	if !a.loginLimiter.check(key, false) {
		http.Error(w, "Demasiados intentos, espera 5 minutos", 429)
		return
	}
	if user != a.cfg.AdminUser || !a.passwordOK(password) {
		a.loginLimiter.check(key, true)
		w.WriteHeader(http.StatusUnauthorized)
		a.render(w, r, "login", pageData{Title: "Iniciar sesión", Notice: "Credenciales incorrectas"})
		return
	}
	a.setSession(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "dataset_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isHTTPS(r)})
	http.Redirect(w, r, "/login", 303)
}
func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	s, err := a.db.stats()
	if err != nil {
		http.Error(w, "error SQLite", 500)
		return
	}
	jobs, err := a.db.jobs("", 8, 0)
	if err != nil {
		http.Error(w, "error SQLite", 500)
		return
	}
	a.render(w, r, "dashboard", pageData{Title: "Resumen", Active: "dashboard", Stats: s, Jobs: jobs})
}
func (a *App) jobsPage(w http.ResponseWriter, r *http.Request) {
	st := r.URL.Query().Get("status")
	switch st {
	case "pending", "processing", "completed", "failed", "":
	default:
		st = ""
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	if page > 100000 {
		page = 100000
	}
	jobs, err := a.db.jobs(st, 26, (page-1)*25)
	if err != nil {
		http.Error(w, "error SQLite", 500)
		return
	}
	next := page + 1
	prev := page - 1
	if prev < 1 {
		prev = 1
	}
	a.render(w, r, "jobs", pageData{Title: "Trabajos", Active: "jobs", Filter: st, Jobs: jobs[:min(len(jobs), 25)], Page: page, Next: next, Prev: prev, HasNext: len(jobs) > 25})
}
func (a *App) settingsPage(w http.ResponseWriter, r *http.Request) {
	ref, err := a.db.reference()
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "error SQLite", 500)
		return
	}
	var preview *importPreview
	if id := r.URL.Query().Get("preview"); id != "" {
		preview, err = a.loadImportPreview(r, id)
		if err != nil {
			http.Error(w, "Vista previa caducada o inválida: vuelve a subir el CSV", http.StatusGone)
			return
		}
	}
	a.render(w, r, "settings", pageData{Title: "Configuración", Active: "settings", Reference: ref, HasReference: err == nil, ImportPreview: preview})
}
func (a *App) workersPage(w http.ResponseWriter, r *http.Request) {
	a.clientsPage(w, r, "", "", "")
}
