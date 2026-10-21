package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func adminSession(t *testing.T, a *App) (*http.Cookie, string) {
	t.Helper()
	h := a.routes()
	form := url.Values{"username": {"admin"}, "password": {"password12345"}}.Encode()
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == "dataset_session" {
			get := httptest.NewRequest("GET", "/", nil)
			get.AddCookie(c)
			token, ok := a.session(get)
			if !ok {
				t.Fatal("invalid admin session")
			}
			return c, token
		}
	}
	t.Fatal("missing cookie")
	return nil, ""
}
func previewTestUpload(t *testing.T, a *App, cookie *http.Cookie, csrf, contents string) string {
	t.Helper()
	req := multipartRequest(t, "/import", "csv", "Gonzalo-4374.csv", []byte(contents), csrf)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	a.routes().ServeHTTP(rec, req)
	if rec.Code != 303 {
		t.Fatalf("upload %d: %s", rec.Code, rec.Body.String())
	}
	parsed, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	id := parsed.Query().Get("preview")
	if id == "" {
		t.Fatal("missing preview id")
	}
	return id
}
func confirmTestUpload(t *testing.T, a *App, cookie *http.Cookie, csrf, id, skipNames, textChoice string) *httptest.ResponseRecorder {
	t.Helper()
	data := url.Values{"_csrf": {csrf}, "preview_id": {id}, "duplicate_texts": {textChoice}}
	if skipNames != "" {
		data.Set("duplicate_names", skipNames)
	}
	req := httptest.NewRequest("POST", "/import/confirm", strings.NewReader(data.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	a.routes().ServeHTTP(rec, req)
	return rec
}
func TestGonzaloCSVQuotedLineAnd4374Rows(t *testing.T) {
	var lines strings.Builder
	for i := 1; i <= 4374; i++ {
		text := "Una frase de prueba para convertir en voz."
		if i == 2395 {
			text = `El profesor miró fijamente a la clase y dijo: "La constancia es la clave fundamental para alcanzar el éxito".`
		} else if i >= 3000 && i < 3019 {
			text = `Otra frase con "comillas" que deben conservarse.`
		}
		fmt.Fprintf(&lines, "%d.wav|%s\n", i, text)
	}
	rows, warnings, err := parseDatasetCSV([]byte(lines.String()))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4374 {
		t.Fatalf("rows=%d", len(rows))
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], "20 líneas") || !strings.Contains(warnings[0], "2395") {
		t.Fatalf("advertencia imprecisa: %v", warnings)
	}
	if !strings.Contains(rows[2394].Text, `"La constancia`) {
		t.Fatalf("quotes lost: %s", rows[2394].Text)
	}
	if rows[2394].Line != 2395 {
		t.Fatalf("line=%d", rows[2394].Line)
	}
}
func TestImportReviewDuplicatesAndOriginalInZIP(t *testing.T) {
	a := testApp(t)
	cookie, csrf := adminSession(t, a)
	original := "1.wav|Buenos días.\n2.wav|El profesor dijo: \"Todo es posible\".\n2.wav|Otra frase distinta.\n3.wav|Buenos días.\n4.wav|Una cuarta frase.\n"
	id := previewTestUpload(t, a, cookie, csrf, original)
	req := httptest.NewRequest("GET", "/settings?preview="+id, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	a.routes().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Línea") {
		t.Fatalf("preview status=%d has warnings=%t", rec.Code, strings.Contains(rec.Body.String(), "Línea"))
	}
	p, err := a.loadImportPreview(req, id)
	if err != nil {
		t.Fatal(err)
	}
	if p.FilenameDuplicates != 1 || p.TextDuplicates != 1 || len(p.Warnings) == 0 {
		t.Fatalf("preview duplicate summary: %+v", p)
	}
	stats, _ := a.db.stats()
	if stats.Total != 0 {
		t.Fatalf("import before confirmation! %+v", stats)
	}
	bad := confirmTestUpload(t, a, cookie, csrf, id, "", "skip")
	if bad.Code != 409 {
		t.Fatalf("duplicate confirmation not enforced: %d", bad.Code)
	}
	ok := confirmTestUpload(t, a, cookie, csrf, id, "skip", "skip")
	if ok.Code != 303 {
		t.Fatalf("confirm status=%d: %s", ok.Code, ok.Body.String())
	}
	stats, _ = a.db.stats()
	if stats.Total != 3 || stats.Pending != 3 {
		t.Fatalf("stats %+v", stats)
	}
	second := confirmTestUpload(t, a, cookie, csrf, id, "skip", "skip")
	if second.Code != http.StatusGone {
		t.Fatalf("replayed import: %d", second.Code)
	}
	exp := httptest.NewRequest("GET", "/export", nil)
	exp.AddCookie(cookie)
	output := httptest.NewRecorder()
	a.routes().ServeHTTP(output, exp)
	if output.Code != 200 {
		t.Fatalf("export status=%d", output.Code)
	}
	archive, err := zip.NewReader(bytes.NewReader(output.Body.Bytes()), int64(output.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var source bool
	for _, entry := range archive.File {
		if strings.HasPrefix(entry.Name, "sources/") {
			f, err := entry.Open()
			if err != nil {
				t.Fatal(err)
			}
			content, err := io.ReadAll(f)
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
			if string(content) != original {
				t.Fatal("original CSV changed in ZIP")
			}
			source = true
		}
	}
	if !source {
		t.Fatal("original CSV missing from ZIP")
	}
	path := filepath.Join(a.cfg.DataDir, "sources", id+"-Gonzalo-4374.csv")
	data, err := os.ReadFile(path)
	if err != nil || string(data) != original {
		t.Fatalf("source file not preserved: %v", err)
	}
}
func TestExistingJobNotDeletedAndOptInTexts(t *testing.T) {
	a := testApp(t)
	insertJob(t, a, "Existing.wav")
	cookie, csrf := adminSession(t, a)
	src := "Existing.wav|Texto distinto.\nNew.wav|Hola.\nNEW2.wav|Hola.\n"
	id := previewTestUpload(t, a, cookie, csrf, src)
	req := httptest.NewRequest("GET", "/settings?preview="+id, nil)
	req.AddCookie(cookie)
	p, err := a.loadImportPreview(req, id)
	if err != nil {
		t.Fatal(err)
	}
	if p.ExistingDuplicates != 1 || p.TextDuplicates != 1 {
		t.Fatalf("dupes %+v", p)
	}
	resp := confirmTestUpload(t, a, cookie, csrf, id, "skip", "keep")
	if resp.Code != 303 {
		t.Fatalf("confirm: %d, %s", resp.Code, resp.Body.String())
	}
	stats, _ := a.db.stats()
	if stats.Total != 3 {
		t.Fatalf("expected existing+two new, %+v", stats)
	}
	var text string
	if err := a.db.DB.QueryRow("SELECT text FROM jobs WHERE filename='Existing.wav'").Scan(&text); err != nil || text != "Hola" {
		t.Fatalf("existing modified! text=%s err=%v", text, err)
	}
}
