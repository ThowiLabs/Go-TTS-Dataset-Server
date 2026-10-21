package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestClientProvisioningAndRevocation(t *testing.T) {
	a := testApp(t)
	h := a.routes()
	loginForm := url.Values{"username": {"admin"}, "password": {"password12345"}}.Encode()
	request := httptest.NewRequest("POST", "/login", strings.NewReader(loginForm))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	writer := httptest.NewRecorder()
	h.ServeHTTP(writer, request)
	if writer.Code != 303 {
		t.Fatalf("login: %d", writer.Code)
	}
	var session *http.Cookie
	for _, c := range writer.Result().Cookies() {
		if c.Name == "dataset_session" {
			session = c
		}
	}
	if session == nil {
		t.Fatal("cookie missing")
	}
	csrf, ok := a.session(func() *http.Request { r := httptest.NewRequest("GET", "/", nil); r.AddCookie(session); return r }())
	if !ok {
		t.Fatal("session invalid")
	}
	adminPost := func(path string, values url.Values) *httptest.ResponseRecorder {
		values.Set("_csrf", csrf)
		r := httptest.NewRequest("POST", path, strings.NewReader(values.Encode()))
		r.AddCookie(session)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		out := httptest.NewRecorder()
		h.ServeHTTP(out, r)
		return out
	}
	create := adminPost("/workers", url.Values{"client_id": {"gpu_a"}, "description": {"GPU A"}})
	if create.Code != 200 {
		t.Fatalf("registration: %d %s", create.Code, create.Body.String())
	}
	re := regexp.MustCompile(`dts_[a-f0-9]{64}`)
	token := re.FindString(create.Body.String())
	if token == "" {
		t.Fatal("no token shown")
	}
	list := httptest.NewRequest("GET", "/workers", nil)
	list.AddCookie(session)
	listed := httptest.NewRecorder()
	h.ServeHTTP(listed, list)
	if listed.Code != 200 || !strings.Contains(listed.Body.String(), "gpu_a") {
		t.Fatal("registered client not listed")
	}
	if strings.Contains(listed.Body.String(), token) {
		t.Fatal("secret shown again after issuance")
	}
	docs := httptest.NewRequest("GET", "/api-docs", nil)
	docs.AddCookie(session)
	docResponse := httptest.NewRecorder()
	h.ServeHTTP(docResponse, docs)
	if docResponse.Code != 200 || !strings.Contains(docResponse.Body.String(), "/api/v1/jobs/complete") &&
		!strings.Contains(docResponse.Body.String(), "/api/v1/jobs/{id}/complete") {
		t.Fatal("documentation not rendered")
	}
	apiRequest := func(path, method, data, tok string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(data))
		r.Header.Set("Authorization", "Bearer "+tok)
		if method == "POST" {
			r.Header.Set("Content-Type", "application/json")
		}
		response := httptest.NewRecorder()
		h.ServeHTTP(response, r)
		return response
	}
	me := apiRequest("/api/v1/client/me", "GET", "", token)
	if me.Code != 200 || !strings.Contains(me.Body.String(), "gpu_a") {
		t.Fatalf("me: %d %s", me.Code, me.Body.String())
	}
	if wrong := apiRequest("/api/v1/client/me", "GET", "", "bad-token"); wrong.Code != 401 {
		t.Fatal("wrong token accepted")
	}
	if mismatch := apiRequest("/api/v1/jobs/claim", "POST", `{"worker_id":"gpu_b"}`, token); mismatch.Code != 403 {
		t.Fatalf("worker mismatch: %d", mismatch.Code)
	}
	insertJob(t, a, "11.wav")
	// Reference needed before claiming.
	if noRef := apiRequest("/api/v1/jobs/claim", "POST", `{}`, token); noRef.Code != 409 {
		t.Fatalf("missing reference: %d", noRef.Code)
	}
	// Install only a test WAV reference, without needing a browser login.
	_, err := a.db.DB.Exec("INSERT INTO reference(id,filename,sha256,path,updated_at)VALUES(1,'ref.wav','sha','/tmp/reference.wav',1)")
	if err != nil {
		t.Fatal(err)
	}
	claimed := apiRequest("/api/v1/jobs/claim", "POST", `{}`, token)
	if claimed.Code != 200 {
		t.Fatalf("claim: %d %s", claimed.Code, claimed.Body.String())
	}
	var payload struct {
		Job Job `json:"job"`
	}
	if err := json.Unmarshal(claimed.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Job.WorkerID != "gpu_a" {
		t.Fatalf("incorrect worker: %+v", payload.Job)
	}
	oldToken := token
	revoked := adminPost("/workers/gpu_a/revoke", url.Values{})
	if revoked.Code != 303 {
		t.Fatalf("revoke: %d", revoked.Code)
	}
	if still := apiRequest("/api/v1/client/me", "GET", "", oldToken); still.Code != 401 {
		t.Fatal("revoked token accepted")
	}
	status, err := a.db.stats()
	if err != nil || status.Pending != 1 || status.Processing != 0 {
		t.Fatalf("requeue after revocation: %+v %v", status, err)
	}
	rotated := adminPost("/workers/gpu_a/rotate", url.Values{})
	if rotated.Code != 200 {
		t.Fatalf("rotate: %d %s", rotated.Code, rotated.Body.String())
	}
	newToken := re.FindString(rotated.Body.String())
	if newToken == "" || newToken == oldToken {
		t.Fatal("new token wasn't issued")
	}
	if still := apiRequest("/api/v1/client/me", "GET", "", newToken); still.Code != 200 {
		t.Fatal("rotated token rejected")
	}
	next := apiRequest("/api/v1/jobs/claim", "POST", `{}`, newToken)
	if next.Code != 200 {
		t.Fatalf("retry after rotate: %d", next.Code)
	}
	if collision := adminPost("/workers", url.Values{"client_id": {"gpu_a"}}); collision.Code != 409 {
		t.Fatal("duplicate client registered")
	}
	badCSRF := httptest.NewRequest("POST", "/workers", strings.NewReader("client_id=forged"))
	badCSRF.AddCookie(session)
	badCSRF.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	result := httptest.NewRecorder()
	h.ServeHTTP(result, badCSRF)
	if result.Code != 403 {
		t.Fatalf("CSRF bypass: %d", result.Code)
	}
	t.Log(fmt.Sprintf("registered, revoked and rotated client %q", payload.Job.WorkerID))
}
