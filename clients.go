package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	ID, Description                string
	CreatedAt, LastSeen, RevokedAt int64
	Claimed, Completed             int
}

func tokenDigest(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
func (s *Store) addClient(id, description string) (string, error) {
	token := "dts_" + randomToken(32)
	now := time.Now().Unix()
	_, err := s.DB.Exec("INSERT INTO clients(id,description,token_hash,created_at) VALUES(?,?,?,?)", id, description, tokenDigest(token), now)
	if err != nil {
		return "", err
	}
	return token, nil
}
func (s *Store) clientByToken(token string) (string, error) {
	if len(token) != 68 || !strings.HasPrefix(token, "dts_") {
		return "", sql.ErrNoRows
	}
	var id string
	err := s.DB.QueryRow("SELECT id FROM clients WHERE token_hash=? AND revoked_at=0", tokenDigest(token)).Scan(&id)
	if err != nil {
		return "", err
	}
	// Activity is based on real authenticated API requests, not inferred client presence.
	result, err := s.DB.Exec("UPDATE clients SET last_seen=? WHERE id=? AND revoked_at=0", time.Now().Unix(), id)
	if err != nil {
		return "", err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return "", sql.ErrNoRows
	}
	return id, nil
}
func (s *Store) listClients() ([]Client, error) {
	rows, err := s.DB.Query(`SELECT c.id,c.description,c.created_at,c.last_seen,c.revoked_at,
 COALESCE(w.claimed,0),COALESCE(w.completed,0)
 FROM clients c LEFT JOIN workers w ON w.id=c.id ORDER BY c.created_at DESC,c.id LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Client{}
	for rows.Next() {
		var c Client
		if err := rows.Scan(&c.ID, &c.Description, &c.CreatedAt, &c.LastSeen, &c.RevokedAt, &c.Claimed, &c.Completed); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}
func (s *Store) changeClient(id, action string) (string, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	var token string
	var result sql.Result
	switch action {
	case "revoke":
		result, err = tx.Exec("UPDATE clients SET revoked_at=? WHERE id=? AND revoked_at=0", now, id)
	case "rotate":
		token = "dts_" + randomToken(32)
		result, err = tx.Exec("UPDATE clients SET token_hash=?,revoked_at=0 WHERE id=?", tokenDigest(token), id)
	default:
		return "", errors.New("acción inválida")
	}
	if err != nil {
		return "", err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", sql.ErrNoRows
	}
	// Existing reservations are invalidated when a token is revoked or rotated.
	_, err = tx.Exec(`UPDATE jobs SET status='pending',worker_id='',lease_token='',lease_until=0,
 error='Credencial de cliente revocada o renovada',updated_at=?
 WHERE worker_id=? AND status='processing'`, now, id)
	if err != nil {
		return "", err
	}
	return token, tx.Commit()
}
func (a *App) authorizedClient(r *http.Request) (string, bool) {
	authorization := r.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") {
		return "", false
	}
	token := strings.TrimPrefix(authorization, "Bearer ")
	id, err := a.db.clientByToken(token)
	return id, err == nil
}
func (a *App) clientsPage(w http.ResponseWriter, r *http.Request, issuedToken, issuedID, notice string) {
	clients, err := a.db.listClients()
	if err != nil {
		http.Error(w, "No se pudieron leer los clientes", 500)
		return
	}
	a.render(w, r, "workers", pageData{Title: "Clientes", Active: "workers", Clients: clients, IssuedToken: issuedToken, IssuedID: issuedID, Notice: notice})
}
func (a *App) registerClient(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PostForm.Get("client_id"))
	desc := strings.TrimSpace(r.PostForm.Get("description"))
	if !workerName.MatchString(id) || len(desc) > 150 {
		http.Error(w, "Identificador inválido (letras, números, guion o guion bajo; máx. 61) o descripción demasiado larga", 400)
		return
	}
	token, err := a.db.addClient(id, desc)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			http.Error(w, "Ya existe una instancia con ese ID. Puedes renovar su token.", 409)
			return
		}
		http.Error(w, "No se pudo crear cliente", 500)
		return
	}
	a.clientsPage(w, r, token, id, "Cliente registrado. Copia ahora su token: solo se mostrará esta vez.")
}
func (a *App) updateClient(w http.ResponseWriter, r *http.Request, action string) {
	id := r.PathValue("id")
	if !workerName.MatchString(id) {
		http.Error(w, "ID inválido", 400)
		return
	}
	token, err := a.db.changeClient(id, action)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Cliente inexistente o ya revocado", 404)
		return
	}
	if err != nil {
		http.Error(w, "No se pudo actualizar el cliente", 500)
		return
	}
	if action == "rotate" {
		a.clientsPage(w, r, token, id, "Token renovado. El anterior dejó de funcionar.")
		return
	}
	http.Redirect(w, r, "/workers?notice=Acceso+revocado", http.StatusSeeOther)
}
func (a *App) revokeClient(w http.ResponseWriter, r *http.Request) { a.updateClient(w, r, "revoke") }
func (a *App) rotateClient(w http.ResponseWriter, r *http.Request) { a.updateClient(w, r, "rotate") }
func (a *App) currentClientAPI(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]string{"client_id": clientIDFromContext(r.Context()), "status": "active"})
}
func (a *App) apiDocsPage(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "api-docs", pageData{Title: "Documentación de API", Active: "api-docs"})
}
