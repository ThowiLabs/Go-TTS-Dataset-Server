package main

import (
	"archive/zip"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var wavName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,120}\.wav$`)
var workerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,60}$`)

func validWavName(s string) bool { return wavName.MatchString(s) && !strings.Contains(s, "..") }
func jsonOK(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(data)
}
func writeErr(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
func readJSON(r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("JSON extra o inválido")
	}
	return nil
}
func numericID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("id inválido")
	}
	return id, nil
}
func (a *App) statsAPI(w http.ResponseWriter, r *http.Request) {
	v, err := a.db.stats()
	if err != nil {
		writeErr(w, 500, "error consultando estadísticas")
		return
	}
	jsonOK(w, v)
}
func (a *App) referenceMetadata(w http.ResponseWriter, r *http.Request) {
	ref, err := a.db.reference()
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, 404, "no hay voz de referencia")
		return
	}
	if err != nil {
		writeErr(w, 500, "no se pudo consultar referencia")
		return
	}
	jsonOK(w, map[string]any{"sha256": ref.SHA256, "filename": ref.Filename, "updated_at": ref.UpdatedAt, "download_url": "/api/v1/reference"})
}
func (a *App) referenceDownload(w http.ResponseWriter, r *http.Request) {
	ref, err := a.db.reference()
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "No hay voz de referencia", 404)
		return
	}
	if err != nil {
		http.Error(w, "Error de servidor", 500)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("ETag", fmt.Sprintf("%q", ref.SHA256))
	w.Header().Set("Content-Disposition", `attachment; filename="reference.wav"`)
	http.ServeFile(w, r, ref.Path)
}
func (a *App) claimJob(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkerID string `json:"worker_id"`
	}
	if err := readJSON(r, &req); err != nil || !workerName.MatchString(req.WorkerID) {
		writeErr(w, 400, "worker_id inválido")
		return
	}
	// Recover overdue leases before claiming, so a restarted server has no stale backlog.
	if _, err := a.db.requeueExpired(a.cfg.MaxAttempts); err != nil {
		writeErr(w, 500, "error de recuperación")
		return
	}
	if _, err := a.db.reference(); errors.Is(err, sql.ErrNoRows) {
		writeErr(w, 409, "configura una voz de referencia antes de procesar")
		return
	} else if err != nil {
		writeErr(w, 500, "error consultando referencia")
		return
	}
	job, err := a.db.claim(req.WorkerID, randomToken(24), a.cfg.LeaseSeconds)
	if errors.Is(err, sql.ErrNoRows) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		log.Printf("claim: %v", err)
		writeErr(w, 500, "no se pudo reservar")
		return
	}
	ref, err := a.db.reference()
	if err == nil {
		jsonOK(w, map[string]any{"job": job, "reference_sha256": ref.SHA256, "reference_url": "/api/v1/reference", "lease_seconds": a.cfg.LeaseSeconds})
		return
	}
	jsonOK(w, map[string]any{"job": job, "reference_sha256": "", "reference_url": "", "lease_seconds": a.cfg.LeaseSeconds})
}
func (a *App) heartbeatJob(w http.ResponseWriter, r *http.Request) {
	id, err := numericID(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var req struct {
		LeaseToken string `json:"lease_token"`
	}
	if err := readJSON(r, &req); err != nil || len(req.LeaseToken) < 32 {
		writeErr(w, 400, "lease_token inválido")
		return
	}
	ok, err := a.db.heartbeat(id, req.LeaseToken, a.cfg.LeaseSeconds)
	if err != nil {
		writeErr(w, 500, "error al renovar")
		return
	}
	if !ok {
		writeErr(w, 409, "reserva vencida o ajena")
		return
	}
	jsonOK(w, map[string]any{"ok": true, "lease_seconds": a.cfg.LeaseSeconds})
}
func (a *App) failJob(w http.ResponseWriter, r *http.Request) {
	id, err := numericID(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var req struct {
		LeaseToken string `json:"lease_token"`
		Error      string `json:"error"`
	}
	if err := readJSON(r, &req); err != nil || len(req.LeaseToken) < 32 {
		writeErr(w, 400, "lease_token inválido")
		return
	}
	if len(req.Error) > 500 {
		req.Error = req.Error[:500]
	}
	if strings.TrimSpace(req.Error) == "" {
		req.Error = "Error informado por el worker"
	}
	ok, err := a.db.fail(id, req.LeaseToken, req.Error, a.cfg.MaxAttempts)
	if err != nil {
		writeErr(w, 500, "error al informar fallo")
		return
	}
	if !ok {
		writeErr(w, 409, "reserva vencida o ajena")
		return
	}
	jsonOK(w, map[string]bool{"ok": true})
}
func stageWav(f multipart.File, dir string, max int64) (string, error) {
	h := make([]byte, 12)
	n, err := io.ReadFull(f, h)
	if err != nil || n != 12 || string(h[:4]) != "RIFF" || string(h[8:]) != "WAVE" {
		return "", errors.New("WAV inválido: falta cabecera RIFF/WAVE")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	out, err := os.CreateTemp(dir, "upload-*.wav")
	if err != nil {
		return "", err
	}
	defer out.Close()
	name := out.Name()
	n64, err := io.Copy(out, io.LimitReader(f, max+1))
	if err != nil || n64 > max {
		os.Remove(name)
		return "", errors.New("archivo WAV demasiado grande o ilegible")
	}
	if err := out.Sync(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}
func (a *App) completeJob(w http.ResponseWriter, r *http.Request) {
	id, err := numericID(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		writeErr(w, 400, "multipart inválido o demasiado grande")
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	token := r.FormValue("lease_token")
	if len(token) < 32 {
		writeErr(w, 400, "lease_token obligatorio")
		return
	}
	file, _, err := r.FormFile("audio")
	if err != nil {
		writeErr(w, 400, "falta campo audio")
		return
	}
	defer file.Close()
	tmp, err := stageWav(file, filepath.Join(a.cfg.DataDir, "tmp"), 60<<20)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	defer os.Remove(tmp)
	ok, err := a.db.complete(id, token, tmp)
	if err != nil {
		log.Printf("complete: %v", err)
		writeErr(w, 500, "no se pudo guardar el audio")
		return
	}
	if !ok {
		writeErr(w, 409, "reserva vencida, completada o ajena")
		return
	}
	jsonOK(w, map[string]bool{"ok": true})
}
func (a *App) exportZIP(w http.ResponseWriter, r *http.Request) {
	jobs, err := a.db.jobs("completed", 1000000, 0)
	if err != nil {
		http.Error(w, "error consultando trabajos", 500)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="dataset.zip"`)
	z := zip.NewWriter(w)
	defer z.Close()
	out, err := z.Create("metadata.csv")
	if err != nil {
		return
	}
	csvWriter := csv.NewWriter(out)
	csvWriter.Comma = '|'
	for _, j := range jobs {
		if err := csvWriter.Write([]string{"wavs/" + j.Filename, j.Text}); err != nil {
			return
		}
	}
	csvWriter.Flush()
	if csvWriter.Error() != nil {
		return
	}
	for _, j := range jobs {
		f, err := os.Open(j.AudioPath)
		if err != nil {
			log.Printf("ZIP missing wav id=%d: %v", j.ID, err)
			return
		}
		entry, err := z.Create("wavs/" + j.Filename)
		if err != nil {
			f.Close()
			return
		}
		_, err = io.Copy(entry, f)
		f.Close()
		if err != nil {
			log.Printf("ZIP copy: %v", err)
			return
		}
	}
}
func (a *App) importCSV(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		http.Error(w, "CSV demasiado grande", 400)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	f, _, err := r.FormFile("csv")
	if err != nil {
		http.Error(w, "falta CSV", 400)
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 30<<20))
	if err != nil || !utf8.Valid(raw) {
		http.Error(w, "CSV inválido o grande", 400)
		return
	}
	raw = []byte(strings.TrimPrefix(string(raw), "\ufeff"))
	delimiter := '|'
	line := strings.SplitN(string(raw), "\n", 2)[0]
	if !strings.ContainsRune(line, '|') && strings.ContainsRune(line, ',') {
		delimiter = ','
	}
	reader := csv.NewReader(strings.NewReader(string(raw)))
	reader.Comma = delimiter
	reader.FieldsPerRecord = 2
	reader.TrimLeadingSpace = true
	tx, err := a.db.DB.Begin()
	if err != nil {
		http.Error(w, "error SQLite", 500)
		return
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO jobs(filename,text,status,created_at,updated_at)VALUES(?,?,'pending',?,?) ON CONFLICT(filename) DO NOTHING`)
	if err != nil {
		http.Error(w, "error SQLite", 500)
		return
	}
	defer stmt.Close()
	added, skipped := 0, 0
	now := time.Now().Unix()
	for {
		rec, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			http.Error(w, fmt.Sprintf("CSV inválido en línea %d: %v", reader.InputOffset(), err), 400)
			return
		}
		name, text := strings.TrimSpace(rec[0]), strings.TrimSpace(rec[1])
		if strings.EqualFold(name, "filename") || strings.EqualFold(name, "archivo") {
			continue
		}
		if !validWavName(name) || text == "" || len(text) > 10000 {
			http.Error(w, "Nombre WAV o texto inválido: "+name, 400)
			return
		}
		res, err := stmt.Exec(name, text, now, now)
		if err != nil {
			http.Error(w, "error insertando registro", 500)
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			skipped++
		} else {
			added++
		}
		if added+skipped > 100000 {
			http.Error(w, "máximo 100000 registros por importación", 400)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "error confirmando importación", 500)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/settings?notice=importados+%d,+duplicados+%d", added, skipped), http.StatusSeeOther)
}
func (a *App) uploadReference(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 26<<20)
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		http.Error(w, "WAV muy grande", 400)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	f, _, err := r.FormFile("reference")
	if err != nil {
		http.Error(w, "falta WAV", 400)
		return
	}
	defer f.Close()
	tmp, err := stageWav(f, filepath.Join(a.cfg.DataDir, "tmp"), 25<<20)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	defer os.Remove(tmp)
	contents, err := os.ReadFile(tmp)
	if err != nil {
		http.Error(w, "no se pudo leer WAV", 500)
		return
	}
	digest := sha256.Sum256(contents)
	sha := hex.EncodeToString(digest[:])
	path := filepath.Join(a.cfg.DataDir, "reference", sha+".wav")
	if err := os.Rename(tmp, path); err != nil {
		http.Error(w, "no se pudo guardar WAV", 500)
		return
	}
	_, err = a.db.DB.Exec(`INSERT INTO reference(id,filename,sha256,path,updated_at)VALUES(1,'reference.wav',?,?,?)
 ON CONFLICT(id) DO UPDATE SET sha256=excluded.sha256,path=excluded.path,updated_at=excluded.updated_at`, sha, path, time.Now().Unix())
	if err != nil {
		http.Error(w, "error guardando referencia", 500)
		return
	}
	http.Redirect(w, r, "/settings?notice=referencia+actualizada", http.StatusSeeOther)
}
func (a *App) retryJob(w http.ResponseWriter, r *http.Request) {
	id, err := numericID(r)
	if err != nil {
		http.Error(w, "id inválido", 400)
		return
	}
	ok, err := a.db.retry(id)
	if err != nil {
		http.Error(w, "error SQLite", 500)
		return
	}
	if !ok {
		http.Error(w, "trabajo no está en error", 409)
		return
	}
	http.Redirect(w, r, "/jobs?status=failed", http.StatusSeeOther)
}
