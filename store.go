package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	DB   *sql.DB
	root string
}
type Job struct {
	ID         int64  `json:"id"`
	Filename   string `json:"filename"`
	Text       string `json:"text"`
	Status     string `json:"status"`
	Attempts   int    `json:"attempts"`
	WorkerID   string `json:"worker_id,omitempty"`
	LeaseUntil int64  `json:"lease_until,omitempty"`
	LeaseToken string `json:"lease_token,omitempty"`
	Error      string `json:"error,omitempty"`
	AudioPath  string `json:"-"`
}
type Stats struct {
	Total      int `json:"total"`
	Pending    int `json:"pending"`
	Processing int `json:"processing"`
	Completed  int `json:"completed"`
	Failed     int `json:"failed"`
}
type Reference struct {
	SHA256    string `json:"sha256"`
	Filename  string `json:"filename"`
	UpdatedAt int64  `json:"updated_at"`
	Path      string `json:"-"`
}
type Worker struct {
	ID        string
	LastSeen  int64
	Claimed   int
	Completed int
}

func openStore(root string) (*Store, error) {
	if err := mustDir(root); err != nil {
		return nil, err
	}
	for _, p := range []string{"wavs", "reference", "tmp"} {
		if err := mustDir(filepath.Join(root, p)); err != nil {
			return nil, err
		}
	}
	dsn := filepath.Join(root, "dataset.db")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{DB: db, root: root}
	statements := []string{
		"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON",
		`CREATE TABLE IF NOT EXISTS jobs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,filename TEXT NOT NULL UNIQUE COLLATE NOCASE,
    text TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','processing','completed','failed')),
    attempts INTEGER NOT NULL DEFAULT 0, worker_id TEXT NOT NULL DEFAULT '',
    lease_token TEXT NOT NULL DEFAULT '',lease_until INTEGER NOT NULL DEFAULT 0,
    error TEXT NOT NULL DEFAULT '',audio_path TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,completed_at INTEGER NOT NULL DEFAULT 0
   )`,
		"CREATE INDEX IF NOT EXISTS idx_jobs_queue ON jobs(status,id)",
		"CREATE INDEX IF NOT EXISTS idx_jobs_leases ON jobs(status,lease_until)",
		`CREATE TABLE IF NOT EXISTS reference(id INTEGER PRIMARY KEY CHECK(id=1),
   filename TEXT NOT NULL,sha256 TEXT NOT NULL,path TEXT NOT NULL,updated_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS workers(id TEXT PRIMARY KEY,last_seen INTEGER NOT NULL,
   claimed INTEGER NOT NULL DEFAULT 0,completed INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS clients(id TEXT PRIMARY KEY, description TEXT NOT NULL DEFAULT '',
   token_hash TEXT NOT NULL UNIQUE, created_at INTEGER NOT NULL,
   last_seen INTEGER NOT NULL DEFAULT 0, revoked_at INTEGER NOT NULL DEFAULT 0)`,
	}
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("sqlite schema: %w", err)
		}
	}
	return s, nil
}
func (s *Store) Close() error { return s.DB.Close() }
func (s *Store) stats() (Stats, error) {
	var v Stats
	err := s.DB.QueryRow(`SELECT COUNT(*),COALESCE(SUM(status='pending'),0),COALESCE(SUM(status='processing'),0),
 COALESCE(SUM(status='completed'),0),COALESCE(SUM(status='failed'),0) FROM jobs`).Scan(&v.Total, &v.Pending, &v.Processing, &v.Completed, &v.Failed)
	return v, err
}
func (s *Store) jobs(status string, limit, offset int) ([]Job, error) {
	query := `SELECT id,filename,text,status,attempts,worker_id,lease_until,error,audio_path FROM jobs`
	args := []any{}
	if status != "" {
		query += " WHERE status=?"
		args = append(args, status)
	}
	query += " ORDER BY id LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := make([]Job, 0)
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.Filename, &j.Text, &j.Status, &j.Attempts, &j.WorkerID, &j.LeaseUntil, &j.Error, &j.AudioPath); err != nil {
			return nil, err
		}
		results = append(results, j)
	}
	return results, rows.Err()
}
func (s *Store) reference() (Reference, error) {
	var v Reference
	err := s.DB.QueryRow("SELECT filename,sha256,path,updated_at FROM reference WHERE id=1").Scan(&v.Filename, &v.SHA256, &v.Path, &v.UpdatedAt)
	return v, err
}
func (s *Store) claim(worker, token string, lease int) (Job, error) {
	now := time.Now().Unix()
	j := Job{}
	err := s.DB.QueryRow(`UPDATE jobs SET status='processing',attempts=attempts+1,worker_id=?,lease_token=?,
 lease_until=?,error='',updated_at=? WHERE id=(SELECT id FROM jobs WHERE status='pending' ORDER BY id LIMIT 1)
 AND EXISTS(SELECT 1 FROM clients WHERE id=? AND revoked_at=0)
 RETURNING id,filename,text,status,attempts,worker_id,lease_until`, worker, token, now+int64(lease), now, worker).
		Scan(&j.ID, &j.Filename, &j.Text, &j.Status, &j.Attempts, &j.WorkerID, &j.LeaseUntil)
	if err != nil {
		return Job{}, err
	}
	j.LeaseToken = token
	_, err = s.DB.Exec(`INSERT INTO workers(id,last_seen,claimed)VALUES(?,?,1)
 ON CONFLICT(id) DO UPDATE SET last_seen=excluded.last_seen,claimed=claimed+1`, worker, now)
	return j, err
}
func (s *Store) heartbeat(id int64, token string, lease int, worker string) (bool, error) {
	now := time.Now().Unix()
	r, err := s.DB.Exec(`UPDATE jobs SET lease_until=?,updated_at=? WHERE id=? AND lease_token=? AND worker_id=? AND status='processing' AND lease_until>=?
 AND EXISTS(SELECT 1 FROM clients WHERE id=jobs.worker_id AND revoked_at=0)`, now+int64(lease), now, id, token, worker, now)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	if n > 0 {
		_, _ = s.DB.Exec("UPDATE workers SET last_seen=? WHERE id=(SELECT worker_id FROM jobs WHERE id=?)", now, id)
	}
	return n > 0, nil
}
func (s *Store) fail(id int64, token, message, worker string, max int) (bool, error) {
	now := time.Now().Unix()
	r, err := s.DB.Exec(`UPDATE jobs SET status=CASE WHEN attempts>=? THEN 'failed' ELSE 'pending' END,
 error=?,worker_id='',lease_token='',lease_until=0,updated_at=? WHERE id=? AND lease_token=? AND worker_id=?
 AND status='processing' AND lease_until>=?
 AND EXISTS(SELECT 1 FROM clients WHERE id=jobs.worker_id AND revoked_at=0)`, max, message, now, id, token, worker, now)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n > 0, nil
}
func (s *Store) retry(id int64) (bool, error) {
	r, err := s.DB.Exec(`UPDATE jobs SET status='pending',attempts=0,error='',updated_at=? WHERE id=? AND status='failed'`, time.Now().Unix(), id)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n > 0, nil
}
func (s *Store) complete(id int64, token, temp, identity string) (bool, error) {
	now := time.Now().Unix()
	tx, err := s.DB.BeginTx(context.Background(), nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var filename, worker string
	err = tx.QueryRow(`SELECT filename,worker_id FROM jobs WHERE id=? AND lease_token=? AND worker_id=? AND status='processing' AND lease_until>=?
 AND EXISTS(SELECT 1 FROM clients WHERE id=jobs.worker_id AND revoked_at=0)`, id, token, identity, now).Scan(&filename, &worker)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	path := filepath.Join(s.root, "wavs", fmt.Sprintf("%d-%s.wav", id, randomToken(8)))
	if err := os.Rename(temp, path); err != nil {
		return false, err
	}
	r, err := tx.Exec(`UPDATE jobs SET status='completed',lease_token='',lease_until=0,audio_path=?,completed_at=?,updated_at=?,error='' WHERE id=? AND lease_token=? AND status='processing'`, path, now, now, id, token)
	if err != nil {
		os.Remove(path)
		return false, err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		os.Remove(path)
		return false, nil
	}
	_, err = tx.Exec("UPDATE workers SET last_seen=?,completed=completed+1 WHERE id=?", now, worker)
	if err != nil {
		os.Remove(path)
		return false, err
	}
	if err := tx.Commit(); err != nil {
		os.Remove(path)
		return false, err
	}
	return true, nil
}
func (s *Store) requeueExpired(max int) (int64, error) {
	now := time.Now().Unix()
	r, err := s.DB.Exec(`UPDATE jobs SET status=CASE WHEN attempts>=? THEN 'failed' ELSE 'pending' END,
 error='Reserva vencida: cliente sin heartbeat',worker_id='',lease_token='',lease_until=0,updated_at=?
 WHERE status='processing' AND lease_until<?`, max, now, now)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}
func (s *Store) workers() ([]Worker, error) {
	rows, err := s.DB.Query("SELECT id,last_seen,claimed,completed FROM workers ORDER BY last_seen DESC LIMIT 100")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res := []Worker{}
	for rows.Next() {
		var w Worker
		if err := rows.Scan(&w.ID, &w.LastSeen, &w.Claimed, &w.Completed); err != nil {
			return nil, err
		}
		res = append(res, w)
	}
	return res, rows.Err()
}
func (a *App) sweep(done <-chan struct{}) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			n, err := a.db.requeueExpired(a.cfg.MaxAttempts)
			if err != nil {
				log.Printf("recovery: %v", err)
			} else if n > 0 {
				log.Printf("recovered %d expired jobs", n)
			}
		}
	}
}
