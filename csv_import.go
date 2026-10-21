package main

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const maxCSVSize = 30 << 20
const importExpiry = 30 * time.Minute

var importIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var safeSourceName = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

type importRow struct {
	Name string `json:"name"`
	Text string `json:"text"`
	Line int    `json:"line"`
}
type importPreview struct {
	ID                 string      `json:"id"`
	Owner              string      `json:"owner"`
	OriginalName       string      `json:"original_name"`
	CreatedAt          int64       `json:"created_at"`
	SHA256             string      `json:"sha256"`
	Rows               []importRow `json:"rows"`
	Warnings           []string    `json:"warnings"`
	Samples            []string    `json:"samples"`
	FilenameDuplicates int         `json:"filename_duplicates"`
	TextDuplicates     int         `json:"text_duplicates"`
	ExistingDuplicates int         `json:"existing_duplicates"`
}

func parseDatasetCSV(raw []byte) ([]importRow, []string, error) {
	if !utf8.Valid(raw) {
		return nil, nil, errors.New("el CSV no es UTF-8")
	}
	if len(raw) == 0 {
		return nil, nil, errors.New("el CSV está vacío")
	}
	content := strings.TrimPrefix(string(raw), "\ufeff")
	first := strings.SplitN(content, "\n", 2)[0]
	delimiter := '|'
	if !strings.ContainsRune(first, '|') && strings.ContainsRune(first, ',') {
		delimiter = ','
	}
	parser := func(lazy bool) ([]importRow, error) {
		reader := csv.NewReader(strings.NewReader(content))
		reader.Comma = delimiter
		reader.FieldsPerRecord = -1
		reader.TrimLeadingSpace = true
		reader.LazyQuotes = lazy
		result := make([]importRow, 0, 4096)
		for {
			rec, err := reader.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("línea %d: %w", len(result)+1, err)
			}
			rowLine, _ := reader.FieldPos(0)
			if len(rec) == 0 || len(rec) == 1 && strings.TrimSpace(rec[0]) == "" {
				continue
			}
			if len(rec) < 2 {
				return nil, fmt.Errorf("línea %d: falta la separación entre archivo y texto", rowLine)
			}
			name := strings.TrimSpace(rec[0])
			if strings.EqualFold(name, "filename") || strings.EqualFold(name, "archivo") {
				continue
			}
			text := strings.TrimSpace(strings.Join(rec[1:], string(delimiter)))
			if !validWavName(name) || len(text) == 0 || len(text) > 10000 {
				return nil, fmt.Errorf("línea %d: nombre WAV o texto inválido (%s)", rowLine, name)
			}
			result = append(result, importRow{Name: name, Text: text, Line: rowLine})
			if len(result) > 100000 {
				return nil, errors.New("máximo 100000 registros por importación")
			}
		}
		if len(result) == 0 {
			return nil, errors.New("el CSV no contiene registros")
		}
		return result, nil
	}
	rows, err := parser(false)
	warnings := []string{}
	if err != nil {
		strictError := err
		rows, err = parser(true)
		if err != nil {
			return nil, nil, fmt.Errorf("no se pudo corregir el CSV: %v; lector tolerante: %w", strictError, err)
		}
		lines := []int{}
		for index, row := range strings.Split(content, "\n") {
			sep := strings.IndexRune(row, delimiter)
			if sep < 0 {
				continue
			}
			value := strings.TrimSpace(row[sep+1:])
			if strings.ContainsRune(value, '"') && !strings.HasPrefix(value, "\"") {
				lines = append(lines, index+1)
			}
		}
		message := fmt.Sprintf("Se recuperaron comillas literales en %d líneas (por ejemplo", len(lines))
		for i, line := range lines {
			if i >= 5 {
				break
			}
			message += fmt.Sprintf(" %d", line)
		}
		warnings = append(warnings, message+"). Se conservó el texto y sus comillas.")
	}
	if delimiter == '|' {
		var extra []int
		for line, rawLine := range strings.Split(content, "\n") {
			if strings.Count(rawLine, "|") > 1 && !strings.Contains(rawLine, "\"|") {
				extra = append(extra, line+1)
			}
		}
		if len(extra) > 0 {
			warnings = append(warnings, fmt.Sprintf("Se conservaron separadores adicionales dentro de %d textos (primera línea %d)", len(extra), extra[0]))
		}
	}
	return rows, warnings, nil
}

func importStagePaths(root, id string) (string, string) {
	stage := filepath.Join(root, "tmp", "csv-"+id)
	return stage + ".json", stage + ".csv"
}
func (a *App) loadImportPreview(r *http.Request, id string) (*importPreview, error) {
	if !importIDPattern.MatchString(id) {
		return nil, errors.New("identificador inválido")
	}
	stage, _ := importStagePaths(a.cfg.DataDir, id)
	data, err := os.ReadFile(stage)
	if err != nil {
		return nil, err
	}
	var preview importPreview
	if err := json.Unmarshal(data, &preview); err != nil {
		return nil, err
	}
	nonce, ok := a.session(r)
	digest := sha256.Sum256([]byte(nonce))
	if !ok || preview.ID != id || preview.Owner != hex.EncodeToString(digest[:]) ||
		time.Since(time.Unix(preview.CreatedAt, 0)) > importExpiry {
		return nil, errors.New("vista previa inválida o caducada")
	}
	return &preview, nil
}
func (a *App) inspectDuplicates(p *importPreview) error {
	existingNames := map[string]bool{}
	existingTexts := map[string]bool{}
	rows, err := a.db.DB.Query("SELECT filename, text FROM jobs")
	if err != nil {
		return err
	}
	for rows.Next() {
		var name, text string
		if err := rows.Scan(&name, &text); err != nil {
			rows.Close()
			return err
		}
		existingNames[strings.ToLower(name)] = true
		existingTexts[strings.ToLower(strings.TrimSpace(text))] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	seenNames := map[string]bool{}
	seenTexts := map[string]bool{}
	for _, row := range p.Rows {
		key := strings.ToLower(row.Name)
		txt := strings.ToLower(strings.TrimSpace(row.Text))
		if seenNames[key] {
			p.FilenameDuplicates++
			if len(p.Samples) < 10 {
				p.Samples = append(p.Samples, fmt.Sprintf("Línea %d: %s — nombre repetido en el CSV", row.Line, row.Name))
			}
		}
		if existingNames[key] {
			p.ExistingDuplicates++
			if len(p.Samples) < 10 {
				p.Samples = append(p.Samples, fmt.Sprintf("Línea %d: %s — ya existe en el servidor", row.Line, row.Name))
			}
		}
		if seenTexts[txt] || existingTexts[txt] {
			p.TextDuplicates++
			if len(p.Samples) < 10 {
				p.Samples = append(p.Samples, fmt.Sprintf("Línea %d: texto repetido (%s)", row.Line, row.Name))
			}
		}
		seenNames[key] = true
		seenTexts[txt] = true
	}
	return nil
}

func (a *App) pruneStagedImports() {
	dir := filepath.Join(a.cfg.DataDir, "tmp")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "csv-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, "csv-"), ".json")
		if !importIDPattern.MatchString(id) {
			continue
		}
		info, err := entry.Info()
		if err != nil || time.Since(info.ModTime()) < importExpiry+time.Minute {
			continue
		}
		metadata, raw := importStagePaths(a.cfg.DataDir, id)
		os.Remove(metadata)
		os.Remove(raw)
	}
}

func (a *App) previewCSVUpload(w http.ResponseWriter, r *http.Request) {
	a.pruneStagedImports()
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		http.Error(w, "CSV demasiado grande", 400)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, header, err := r.FormFile("csv")
	if err != nil {
		http.Error(w, "falta archivo CSV", 400)
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxCSVSize+1))
	if err != nil || len(raw) > maxCSVSize {
		http.Error(w, "CSV demasiado grande (máximo 30 MiB)", 400)
		return
	}
	rows, warnings, err := parseDatasetCSV(raw)
	if err != nil {
		http.Error(w, "CSV inválido: "+err.Error(), 400)
		return
	}
	nonce, ok := a.session(r)
	if !ok {
		http.Error(w, "sesión caducada", 403)
		return
	}
	id := randomToken(16)
	digest := sha256.Sum256(raw)
	owner := sha256.Sum256([]byte(nonce))
	name := filepath.Base(strings.ReplaceAll(header.Filename, "\\", "/"))
	if len(name) > 100 {
		name = name[len(name)-100:]
	}
	name = safeSourceName.ReplaceAllString(name, "_")
	if name == "" || name == "." || name == ".." {
		name = "dataset.csv"
	}
	p := importPreview{ID: id, Owner: hex.EncodeToString(owner[:]), OriginalName: name,
		CreatedAt: time.Now().Unix(), SHA256: hex.EncodeToString(digest[:]), Rows: rows, Warnings: warnings}
	if err := a.inspectDuplicates(&p); err != nil {
		http.Error(w, "No se pudieron revisar duplicados", 500)
		return
	}
	metadata, _ := json.Marshal(&p)
	jsonPath, csvPath := importStagePaths(a.cfg.DataDir, id)
	if err := os.WriteFile(csvPath, raw, 0600); err != nil {
		http.Error(w, "No se pudo guardar el CSV", 500)
		return
	}
	if err := os.WriteFile(jsonPath, metadata, 0600); err != nil {
		os.Remove(csvPath)
		http.Error(w, "No se pudo guardar vista previa", 500)
		return
	}
	http.Redirect(w, r, "/settings?preview="+id, http.StatusSeeOther)
}

func (a *App) confirmCSVImport(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Formulario inválido", 400)
		return
	}
	preview, err := a.loadImportPreview(r, r.Form.Get("preview_id"))
	if err != nil {
		http.Error(w, "Vista previa caducada: carga otra vez el archivo", http.StatusGone)
		return
	}
	if (preview.FilenameDuplicates > 0 || preview.ExistingDuplicates > 0) && r.Form.Get("duplicate_names") != "skip" {
		http.Error(w, "Confirma la omisión de nombres de archivo repetidos", http.StatusConflict)
		return
	}
	option := r.Form.Get("duplicate_texts")
	if option != "keep" && option != "skip" {
		http.Error(w, "Elige si conservar o eliminar los textos duplicados", 400)
		return
	}
	_, rawPath := importStagePaths(a.cfg.DataDir, preview.ID)
	raw, err := os.ReadFile(rawPath)
	if err != nil {
		http.Error(w, "CSV temporal perdido", 410)
		return
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != preview.SHA256 {
		http.Error(w, "El CSV temporal fue alterado", 409)
		return
	}
	tx, err := a.db.DB.Begin()
	if err != nil {
		http.Error(w, "Error SQLite", 500)
		return
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare("INSERT INTO jobs(filename,text,status,created_at,updated_at) VALUES(?,?,'pending',?,?) ON CONFLICT(filename) DO NOTHING")
	if err != nil {
		http.Error(w, "No se pudo iniciar importación", 500)
		return
	}
	defer stmt.Close()
	exists, err := tx.Query("SELECT filename,text FROM jobs")
	if err != nil {
		http.Error(w, "Error revisando duplicados", 500)
		return
	}
	names := map[string]bool{}
	texts := map[string]bool{}
	for exists.Next() {
		var name, text string
		if err := exists.Scan(&name, &text); err != nil {
			exists.Close()
			http.Error(w, "Error consultando registros", 500)
			return
		}
		names[strings.ToLower(name)] = true
		texts[strings.ToLower(strings.TrimSpace(text))] = true
	}
	if err := exists.Err(); err != nil {
		exists.Close()
		http.Error(w, "Error leyendo registros", 500)
		return
	}
	exists.Close()
	added, skipped := 0, 0
	now := time.Now().Unix()
	for _, row := range preview.Rows {
		key := strings.ToLower(row.Name)
		txt := strings.ToLower(strings.TrimSpace(row.Text))
		if names[key] || option == "skip" && texts[txt] {
			skipped++
			continue
		}
		res, err := stmt.Exec(row.Name, row.Text, now, now)
		if err != nil {
			http.Error(w, "No se pudo insertar registro", 500)
			return
		}
		n, _ := res.RowsAffected()
		if n == 1 {
			added++
		} else {
			skipped++
		}
		names[key] = true
		texts[txt] = true
	}
	sourceName := preview.ID + "-" + preview.OriginalName
	target := filepath.Join(a.cfg.DataDir, "sources", sourceName)
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		http.Error(w, "La importación ya fue confirmada", http.StatusConflict)
		return
	}
	_, err = file.Write(raw)
	if syncErr := file.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(target)
		http.Error(w, "No se pudo conservar CSV original", 500)
		return
	}
	defer func() {
		if err != nil {
			os.Remove(target)
		}
	}()
	_, err = tx.Exec("INSERT INTO import_sources(id,original_filename,path,sha256,imported_at,imported_count,skipped_count) VALUES(?,?,?,?,?,?,?)",
		preview.ID, preview.OriginalName, target, preview.SHA256, now, added, skipped)
	if err != nil {
		os.Remove(target)
		http.Error(w, "No se pudo registrar archivo fuente", 500)
		return
	}
	if err = tx.Commit(); err != nil {
		os.Remove(target)
		http.Error(w, "No se pudo confirmar importación", 500)
		return
	}
	stage, _ := importStagePaths(a.cfg.DataDir, preview.ID)
	os.Remove(stage)
	os.Remove(rawPath)
	http.Redirect(w, r, fmt.Sprintf("/settings?notice=importados+%d,+omitidos+%d", added, skipped), http.StatusSeeOther)
}

// Html-safe snippets for the existing Go template. Kept bounded in the preview.
func (p *importPreview) Summary() string {
	return fmt.Sprintf("%d registros, %d nombres repetidos en el archivo, %d ya existentes, %d textos repetidos", len(p.Rows), p.FilenameDuplicates, p.ExistingDuplicates, p.TextDuplicates)
}
