package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

type Config struct {
	Listen, DataDir, AdminUser, AdminPassword, SessionSecret string
	LeaseSeconds, MaxAttempts                                int
}
type App struct {
	db           *Store
	cfg          Config
	templates    map[string]*PageTemplate
	loginLimiter *loginThrottle
}

func loadDotEnv(path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pair := strings.SplitN(strings.TrimPrefix(line, "export "), "=", 2)
		if len(pair) != 2 {
			continue
		}
		key := strings.TrimSpace(pair[0])
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		value := strings.TrimSpace(pair[1])
		value = strings.Trim(value, "\"'")
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return nil
}
func config() (Config, error) {
	p := os.Getenv("CONFIG_FILE")
	if p == "" {
		p = ".env"
	}
	if err := loadDotEnv(p); err != nil {
		return Config{}, err
	}
	get := func(k, d string) string {
		v := os.Getenv(k)
		if v == "" {
			return d
		}
		return v
	}
	n, _ := strconv.Atoi(get("LEASE_SECONDS", "1800"))
	if n < 30 {
		n = 1800
	}
	m, _ := strconv.Atoi(get("MAX_ATTEMPTS", "3"))
	if m < 1 {
		m = 3
	}
	c := Config{Listen: get("LISTEN_ADDR", ":8080"), DataDir: get("DATA_DIR", "./data"), AdminUser: get("ADMIN_USER", "admin"), AdminPassword: os.Getenv("ADMIN_PASSWORD"), SessionSecret: os.Getenv("SESSION_SECRET"), LeaseSeconds: n, MaxAttempts: m}
	if len(c.AdminPassword) < 10 || len(c.SessionSecret) < 32 {
		return c, errors.New("configura ADMIN_PASSWORD (>=10 caracteres) y SESSION_SECRET (>=32) en .env")
	}
	return c, nil
}
func newApp(c Config) (*App, error) {
	s, err := openStore(c.DataDir)
	if err != nil {
		return nil, err
	}
	t, err := loadTemplates()
	if err != nil {
		s.Close()
		return nil, err
	}
	return &App{db: s, cfg: c, templates: t, loginLimiter: newLoginThrottle()}, nil
}
func main() {
	c, err := config()
	if err != nil {
		log.Fatal(err)
	}
	a, err := newApp(c)
	if err != nil {
		log.Fatal(err)
	}
	defer a.db.Close()
	ctx, stop := signal.NotifyContext(signalContext(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go a.sweep(ctx.Done())
	server := &http.Server{Addr: c.Listen, Handler: a.routes(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 75 * time.Second, MaxHeaderBytes: 1 << 20}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := withTimeout(15 * time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Printf("dataset server on %s", c.Listen)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
func signalContext() context.Context { return context.Background() }
func withTimeout(t time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), t)
}
func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func (a *App) setSession(w http.ResponseWriter, r *http.Request) {
	expiry := time.Now().Add(12 * time.Hour).Unix()
	payload := fmt.Sprintf("%d:%s", expiry, randomToken(16))
	mac := hmac.New(sha256.New, []byte(a.cfg.SessionSecret))
	mac.Write([]byte(payload))
	token := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	http.SetCookie(w, &http.Cookie{Name: "dataset_session", Value: token, Path: "/", HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode, MaxAge: 43200})
}
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
func (a *App) session(r *http.Request) (string, bool) {
	c, err := r.Cookie("dataset_session")
	if err != nil {
		return "", false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 2 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, []byte(a.cfg.SessionSecret))
	mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return "", false
	}
	split := strings.Split(string(payload), ":")
	if len(split) != 2 {
		return "", false
	}
	n, err := strconv.ParseInt(split[0], 10, 64)
	if err != nil || n < time.Now().Unix() {
		return "", false
	}
	return split[1], true
}
func (a *App) authenticated(r *http.Request) bool { _, ok := a.session(r); return ok }

type clientContextKey struct{}

func clientIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(clientContextKey{}).(string)
	return id
}
func (a *App) needLogin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authenticated(r) {
			if r.Method == http.MethodGet && r.URL.Path == "/" {
				a.render(w, r, "login", pageData{Title: "Iniciar sesión"})
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost && !a.validateCSRF(r) {
			http.Error(w, "CSRF inválido", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}
func (a *App) needWorker(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := a.authorizedClient(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "Token de cliente inválido o revocado")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), clientContextKey{}, id)))
	}
}
func (a *App) validateCSRF(r *http.Request) bool {
	nonce, ok := a.session(r)
	if !ok {
		return false
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		r.Body = http.MaxBytesReader(nil, r.Body, 32<<20)
		if err := r.ParseMultipartForm(4 << 20); err != nil {
			return false
		}
	} else if err := r.ParseForm(); err != nil {
		return false
	}
	sent := r.PostForm.Get("_csrf")
	return len(sent) > 0 && subtle.ConstantTimeCompare([]byte(sent), []byte(nonce)) == 1
}
func (a *App) routes() http.Handler {
	m := http.NewServeMux()
	assets, _ := fs.Sub(webFS, "web/static")
	m.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(assets))))
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "ok")
	})
	m.HandleFunc("GET /login", a.loginPage)
	m.HandleFunc("POST /login", a.loginPost)
	m.HandleFunc("POST /logout", a.needLogin(a.logout))
	m.HandleFunc("GET /", a.needLogin(a.dashboard))
	m.HandleFunc("GET /jobs", a.needLogin(a.jobsPage))
	m.HandleFunc("GET /settings", a.needLogin(a.settingsPage))
	m.HandleFunc("GET /workers", a.needLogin(a.workersPage))
	m.HandleFunc("POST /workers", a.needLogin(a.registerClient))
	m.HandleFunc("POST /workers/{id}/rotate", a.needLogin(a.rotateClient))
	m.HandleFunc("POST /workers/{id}/revoke", a.needLogin(a.revokeClient))
	m.HandleFunc("GET /api-docs", a.needLogin(a.apiDocsPage))
	m.HandleFunc("POST /import", a.needLogin(a.importCSV))
	m.HandleFunc("POST /import/confirm", a.needLogin(a.confirmCSVImport))
	m.HandleFunc("POST /reference", a.needLogin(a.uploadReference))
	m.HandleFunc("POST /jobs/{id}/retry", a.needLogin(a.retryJob))
	m.HandleFunc("GET /export", a.needLogin(a.exportZIP))
	m.HandleFunc("GET /reference.wav", a.needLogin(a.referenceDownload))
	m.HandleFunc("GET /api/v1/reference", a.needWorker(a.referenceDownload))
	m.HandleFunc("GET /api/v1/reference/meta", a.needWorker(a.referenceMetadata))
	m.HandleFunc("GET /api/v1/stats", a.needWorker(a.statsAPI))
	m.HandleFunc("GET /api/v1/client/me", a.needWorker(a.currentClientAPI))
	m.HandleFunc("POST /api/v1/jobs/claim", a.needWorker(a.claimJob))
	m.HandleFunc("POST /api/v1/jobs/{id}/heartbeat", a.needWorker(a.heartbeatJob))
	m.HandleFunc("POST /api/v1/jobs/{id}/complete", a.needWorker(a.completeJob))
	m.HandleFunc("POST /api/v1/jobs/import-completed", a.needWorker(a.importCompletedJob))
	m.HandleFunc("POST /api/v1/jobs/{id}/fail", a.needWorker(a.failJob))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		if r.URL.Path != "/healthz" {
			w.Header().Set("Cache-Control", "no-store")
		}
		m.ServeHTTP(w, r)
	})
}
func (a *App) passwordOK(pass string) bool {
	x := sha256.Sum256([]byte(pass))
	y := sha256.Sum256([]byte(a.cfg.AdminPassword))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}
func mustDir(dir string) error { return os.MkdirAll(filepath.Clean(dir), 0700) }
