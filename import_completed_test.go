package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestImportCompletedResumable(t *testing.T) {
	a := testApp(t)
	insertJob(t, a, "1.wav")
	insertJob(t, a, "2.wav")
	token, err := a.db.addClient("importer", "dataset importer")
	if err != nil {
		t.Fatal(err)
	}
	data := append([]byte("RIFF"), append(make([]byte, 4), []byte("WAVEaudio-content")...)...)
	h := sha256.Sum256(data)
	digest := hex.EncodeToString(h[:])
	send := func(filename, sha, tok string) (int, string) {
		var buf bytes.Buffer
		wr := multipart.NewWriter(&buf)
		wr.WriteField("filename", filename)
		wr.WriteField("sha256", sha)
		f, e := wr.CreateFormFile("audio", filename)
		if e != nil {
			t.Fatal(e)
		}
		f.Write(data)
		wr.Close()
		req := httptest.NewRequest("POST", "/api/v1/jobs/import-completed", &buf)
		req.Header.Set("Content-Type", wr.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+tok)
		out := httptest.NewRecorder()
		a.routes().ServeHTTP(out, req)
		return out.Code, out.Body.String()
	}
	if code, _ := send("1.wav", digest, "wrong"); code != 401 {
		t.Fatalf("unauthenticated %d", code)
	}
	if code, _ := send("missing.wav", digest, token); code != 404 {
		t.Fatalf("unknown row %d", code)
	}
	if code, _ := send("1.wav", strings.Repeat("0", 64), token); code != 422 {
		t.Fatalf("bad hash %d", code)
	}
	if code, body := send("1.wav", digest, token); code != 200 || !strings.Contains(body, "imported") {
		t.Fatalf("import %d %s", code, body)
	}
	if code, body := send("1.wav", digest, token); code != 200 || !strings.Contains(body, "already_complete") {
		t.Fatalf("retry %d %s", code, body)
	}
	if _, err := a.db.DB.Exec("UPDATE jobs SET status='processing' WHERE filename='2.wav'"); err != nil {
		t.Fatal(err)
	}
	if code, _ := send("2.wav", digest, token); code != 409 {
		t.Fatalf("processing conflict %d", code)
	}
	stats, err := a.db.stats()
	if err != nil || stats.Completed != 1 {
		t.Fatalf("stats %+v %v", stats, err)
	}
}
