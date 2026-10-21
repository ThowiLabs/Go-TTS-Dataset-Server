package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func testApp(t *testing.T) *App {
	t.Helper()
	c := Config{DataDir: t.TempDir(), AdminUser: "admin", AdminPassword: "password12345", SessionSecret: strings.Repeat("s", 40), WorkerKey: strings.Repeat("k", 40), LeaseSeconds: 300, MaxAttempts: 3}
	a, err := newApp(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.db.Close() })
	return a
}
func insertJob(t *testing.T, a *App, name string) {
	t.Helper()
	now := time.Now().Unix()
	if _, err := a.db.DB.Exec("INSERT INTO jobs(filename,text,created_at,updated_at) VALUES(?,?,?,?)", name, "Hola", now, now); err != nil {
		t.Fatal(err)
	}
}
func TestAtomicClaimsAndRecovery(t *testing.T) {
	a := testApp(t)
	for i := 1; i <= 5; i++ {
		insertJob(t, a, string(rune('0'+i))+".wav")
	}
	var wg sync.WaitGroup
	ids := make(chan int64, 5)
	errs := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			j, err := a.db.claim("w"+string(rune('0'+n)), randomToken(24), 300)
			if err != nil {
				errs <- err
			} else {
				ids <- j.ID
			}
		}(i)
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	seen := map[int64]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("doble claim: %d", id)
		}
		seen[id] = true
	}
	if len(seen) != 5 {
		t.Fatalf("se reservaron %d de 5", len(seen))
	}
	_, err := a.db.DB.Exec("UPDATE jobs SET lease_until=? WHERE id=1", time.Now().Unix()-5)
	if err != nil {
		t.Fatal(err)
	}
	n, err := a.db.requeueExpired(3)
	if err != nil || n != 1 {
		t.Fatalf("recovered=%d err=%v", n, err)
	}
	j, err := a.db.claim("retry", randomToken(24), 300)
	if err != nil || j.ID != 1 || j.Attempts != 2 {
		t.Fatalf("retry job=%+v err=%v", j, err)
	}
	if ok, _ := a.db.heartbeat(j.ID, "bad_token", 300); ok {
		t.Fatal("heartbeat ajeno aceptado")
	}
}
func multipartRequest(t *testing.T, url, field, name string, file []byte, csrf string) *http.Request {
	t.Helper()
	b := new(bytes.Buffer)
	m := multipart.NewWriter(b)
	_ = m.WriteField("_csrf", csrf)
	p, err := m.CreateFormFile(field, name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Write(file); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, url, b)
	r.Header.Set("Content-Type", m.FormDataContentType())
	return r
}
func TestHTTPDatasetWorkflow(t *testing.T) {
	a := testApp(t)
	h := a.routes()
	form := url.Values{"username": {"admin"}, "password": {"password12345"}}.Encode()
	login := httptest.NewRequest("POST", "/login", strings.NewReader(form))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, login)
	if rec.Code != 303 {
		t.Fatalf("login %d: %s", rec.Code, rec.Body.String())
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "dataset_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("sin cookie")
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Dataset TTS") {
		t.Fatalf("dashboard %d", rec.Code)
	}
	csrf, ok := a.session(r)
	if !ok {
		t.Fatal("no session")
	}
	r = multipartRequest(t, "/import", "csv", "metadata.csv", []byte("1.wav|Hola, mundo\n2.wav|La inteligencia artificial\n"), csrf)
	r.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 303 {
		t.Fatalf("import %d: %s", rec.Code, rec.Body.String())
	}
	stats, _ := a.db.stats()
	if stats.Pending != 2 {
		t.Fatalf("stats %+v", stats)
	}
	wav := append([]byte("RIFF\x10\x00\x00\x00WAVEfmt "), make([]byte, 32)...)
	r = multipartRequest(t, "/reference", "reference", "ref.wav", wav, csrf)
	r.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 303 {
		t.Fatalf("reference upload %d: %s", rec.Code, rec.Body.String())
	}
	claim := func() *httptest.ResponseRecorder {
		body := strings.NewReader(`{"worker_id":"gpu_t4_1"}`)
		req := httptest.NewRequest("POST", "/api/v1/jobs/claim", body)
		req.Header.Set("Authorization", "Bearer "+a.cfg.WorkerKey)
		out := httptest.NewRecorder()
		h.ServeHTTP(out, req)
		return out
	}
	c := claim()
	if c.Code != 200 {
		t.Fatalf("claim %d %s", c.Code, c.Body.String())
	}
	var result struct {
		Job Job `json:"job"`
	}
	if err := json.Unmarshal(c.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Job.Filename != "1.wav" {
		t.Fatalf("unexpected job: %+v", result.Job)
	}
	// No lease token = rejection.
	req := httptest.NewRequest("POST", "/api/v1/jobs/1/complete", nil)
	req.Header.Set("Authorization", "Bearer "+a.cfg.WorkerKey)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == 200 {
		t.Fatal("aceptó upload sin token")
	}
	body := new(bytes.Buffer)
	m := multipart.NewWriter(body)
	m.WriteField("lease_token", result.Job.LeaseToken)
	part, _ := m.CreateFormFile("audio", "1.wav")
	part.Write(wav)
	m.Close()
	req = httptest.NewRequest("POST", "/api/v1/jobs/1/complete", body)
	req.Header.Set("Authorization", "Bearer "+a.cfg.WorkerKey)
	req.Header.Set("Content-Type", m.FormDataContentType())
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("complete %d: %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest("GET", "/export", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("zip: %d %s", rec.Code, rec.Body.String())
	}
	zipped, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, f := range zipped.File {
		reader, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(reader)
		reader.Close()
		found[f.Name] = string(b)
	}
	if !strings.Contains(found["metadata.csv"], "wavs/1.wav|Hola, mundo") || !strings.HasPrefix(found["wavs/1.wav"], "RIFF") {
		t.Fatalf("bad ZIP entries: %v", mapsKeys(found))
	}
}
func mapsKeys(m map[string]string) []string {
	k := []string{}
	for s := range m {
		k = append(k, s)
	}
	return k
}
func TestSafeFilename(t *testing.T) {
	if validWavName("../x.wav") || validWavName("bad/name.wav") || validWavName("x.WAV") || !validWavName("5.wav") {
		t.Fatal("invalid filename policy")
	}
}
