package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func adminCookie(t *testing.T, a *App) *http.Cookie {
	t.Helper()
	w := httptest.NewRecorder()
	a.setSession(w, httptest.NewRequest(http.MethodGet, "/", nil))
	for _, c := range w.Result().Cookies() {
		if c.Name == "dataset_session" {
			return c
		}
	}
	t.Fatal("session cookie missing")
	return nil
}
func panelGET(a *App, path string, cookie *http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, req)
	return w
}
func TestOnDemandCompletedAudioAuthenticatedAndRanged(t *testing.T) {
	a := testApp(t)
	insertJob(t, a, "one.wav")
	insertJob(t, a, "two.wav")
	// Minimal well-formed mono 16kHz/16-bit PCM with 2 silent samples.
	wav := []byte{'R', 'I', 'F', 'F', 40, 0, 0, 0, 'W', 'A', 'V', 'E', 'f', 'm', 't', ' ', 16, 0, 0, 0, 1, 0, 1, 0, 0x80, 0x3e, 0, 0, 0, 0x7d, 0, 0, 2, 0, 16, 0, 'd', 'a', 't', 'a', 4, 0, 0, 0, 0, 0, 0, 0}
	path := filepath.Join(a.cfg.DataDir, "wavs", "1-output.wav")
	if err := os.WriteFile(path, wav, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.DB.Exec("UPDATE jobs SET status='completed',audio_path=?,updated_at=? WHERE id=1", path, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	unauth := panelGET(a, "/jobs/1/audio", nil, nil)
	if unauth.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated audio=%d", unauth.Code)
	}
	cookie := adminCookie(t, a)
	full := panelGET(a, "/jobs/1/audio", cookie, nil)
	if full.Code != 200 || !bytes.Equal(full.Body.Bytes(), wav) {
		t.Fatalf("WAV not served correctly: %d", full.Code)
	}
	if full.Header().Get("Content-Type") != "audio/wav" {
		t.Fatalf("content-type: %v", full.Header())
	}
	if full.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("missing private cache-control")
	}
	part := panelGET(a, "/jobs/1/audio", cookie, map[string]string{"Range": "bytes=0-3"})
	if part.Code != http.StatusPartialContent || part.Body.String() != "RIFF" {
		t.Fatalf("range=%d body=%q", part.Code, part.Body.String())
	}
	if rec := panelGET(a, "/jobs/2/audio", cookie, nil); rec.Code != 404 {
		t.Fatalf("pending WAV exposed: %d", rec.Code)
	}
	if rec := panelGET(a, "/jobs/999/audio", cookie, nil); rec.Code != 404 {
		t.Fatalf("unknown WAV exposed: %d", rec.Code)
	}
	if rec := panelGET(a, "/jobs/not-a-number/audio", cookie, nil); rec.Code != 400 {
		t.Fatalf("invalid id code: %d", rec.Code)
	}
	forbidden := filepath.Join(a.cfg.DataDir, "reference", "reference.wav")
	if err := os.WriteFile(forbidden, wav, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.DB.Exec("UPDATE jobs SET audio_path=? WHERE id=1", forbidden); err != nil {
		t.Fatal(err)
	}
	if rec := panelGET(a, "/jobs/1/audio", cookie, nil); rec.Code != 500 {
		t.Fatalf("path escaped WAV store: %d", rec.Code)
	}
}
func TestLiveRegionsAndLazyAudioMarkup(t *testing.T) {
	a := testApp(t)
	insertJob(t, a, "one.wav")
	cookie := adminCookie(t, a)
	for page, selector := range map[string]string{"/": "data-live-region=\"dashboard\"", "/jobs": "data-live-region=\"jobs\"", "/workers": "data-live-region=\"clients\"", "/settings": "data-live-region=\"reference\""} {
		w := panelGET(a, page, cookie, nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), selector) || !strings.Contains(w.Body.String(), "/static/js/live-panel.js") {
			t.Errorf("%s missing live refresh: status=%d", page, w.Code)
		}
	}
	_, err := a.db.DB.Exec("UPDATE jobs SET status='completed',audio_path='dummy.wav',worker_id='gpu_t4_1' WHERE id=1")
	if err != nil {
		t.Fatal(err)
	}
	w := panelGET(a, "/jobs", cookie, nil)
	if !strings.Contains(w.Body.String(), "play-wav") || !strings.Contains(w.Body.String(), "data-wav-id=\"1\"") {
		t.Fatal("completed job should offer Play")
	}
	if strings.Contains(w.Body.String(), "src=\"/jobs/1/audio\"") {
		t.Fatal("audio must not be preloaded before Play")
	}
	for _, detail := range []string{"Intentos:", "Worker", "Estado: terminado", "Detalles"} {
		if !strings.Contains(w.Body.String(), detail) {
			t.Errorf("missing tooltip %q", detail)
		}
	}
}
