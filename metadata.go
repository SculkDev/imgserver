package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Variant struct {
	Key         string `json:"-"`
	Size        int64  `json:"size"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	ContentType string `json:"content_type"`
	SHA256      string `json:"sha256"`
}

// Keys are persisted separately from the public JSON representation.
type File struct {
	ID          string             `json:"id"`
	Issuer      string             `json:"issuer"`
	Tenant      string             `json:"tenant"`
	Subject     string             `json:"subject"`
	Resource    string             `json:"resource"`
	Profile     string             `json:"profile"`
	Alias       string             `json:"alias,omitempty"`
	Public      bool               `json:"public"`
	Name        string             `json:"name"`
	ContentType string             `json:"content_type"`
	Size        int64              `json:"size"`
	SHA256      string             `json:"sha256"`
	Width       int                `json:"width,omitempty"`
	Height      int                `json:"height,omitempty"`
	CreatedAt   time.Time          `json:"created_at"`
	Variants    map[string]Variant `json:"variants,omitempty"`
	Deleted     bool               `json:"-"`
}

type Metadata struct{ db *sql.DB }

var errAliasOwned = errors.New("alias belongs to a different issuer, tenant, or resource")
var errReplay = errors.New("upload grant already used with different claims")

func openMetadata(path string) (*Metadata, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
	CREATE TABLE IF NOT EXISTS files (id TEXT PRIMARY KEY, data BLOB NOT NULL, deleted INTEGER NOT NULL DEFAULT 0);
	CREATE TABLE IF NOT EXISTS aliases (profile TEXT NOT NULL, alias TEXT NOT NULL, issuer TEXT NOT NULL, tenant TEXT NOT NULL, resource TEXT NOT NULL, file_id TEXT NOT NULL, PRIMARY KEY(profile,alias));
	CREATE TABLE IF NOT EXISTS grants (issuer TEXT NOT NULL, jti TEXT NOT NULL, fingerprint TEXT NOT NULL, file_id TEXT NOT NULL, expires INTEGER NOT NULL, ready INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(issuer,jti));`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS cleanup (file_id TEXT PRIMARY KEY, objects BLOB NOT NULL, not_before INTEGER NOT NULL)`); err != nil {
		db.Close()
		return nil, err
	}
	return &Metadata{db: db}, nil
}

func (m *Metadata) get(ctx context.Context, id string) (*File, error) {
	var data []byte
	var deleted bool
	if err := m.db.QueryRowContext(ctx, `SELECT data,deleted FROM files WHERE id=?`, id).Scan(&data, &deleted); err != nil {
		return nil, err
	}
	var disk struct {
		File
		Objects map[string]string `json:"objects"`
	}
	if err := json.Unmarshal(data, &disk); err != nil {
		return nil, err
	}
	for k, key := range disk.Objects {
		v := disk.Variants[k]
		v.Key = key
		disk.Variants[k] = v
	}
	disk.File.Deleted = deleted
	return &disk.File, nil
}

// reserve makes simultaneous retries of the same single-use JWT deterministic.
func (m *Metadata) reserve(ctx context.Context, c *Claims, fingerprint, id string) (string, bool, error) {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM grants WHERE expires < ?`, time.Now().Add(-24*time.Hour).Unix()); err != nil {
		return "", false, err
	}
	r, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO grants(issuer,jti,fingerprint,file_id,expires) VALUES(?,?,?,?,?)`, c.Issuer, c.ID, fingerprint, id, c.ExpiresAt.Unix())
	if err != nil {
		return "", false, err
	}
	n, _ := r.RowsAffected()
	if n == 1 {
		return "", false, tx.Commit()
	}
	var old, oldID string
	var ready bool
	if err = tx.QueryRowContext(ctx, `SELECT fingerprint,file_id,ready FROM grants WHERE issuer=? AND jti=?`, c.Issuer, c.ID).Scan(&old, &oldID, &ready); err != nil {
		return "", false, err
	}
	if old != fingerprint {
		return "", false, errReplay
	}
	return oldID, ready, nil
}

func (m *Metadata) release(c *Claims) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.db.ExecContext(ctx, `DELETE FROM grants WHERE issuer=? AND jti=? AND ready=0`, c.Issuer, c.ID)
}

func (m *Metadata) save(ctx context.Context, f *File, c *Claims) error {
	objects := map[string]string{}
	for k, v := range f.Variants {
		objects[k] = v.Key
	}
	data, err := json.Marshal(struct {
		*File
		Objects map[string]string `json:"objects"`
	}{f, objects})
	if err != nil {
		return err
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if f.Alias != "" {
		var issuer, tenant, resource string
		err = tx.QueryRowContext(ctx, `SELECT issuer,tenant,resource FROM aliases WHERE profile=? AND alias=?`, f.Profile, f.Alias).Scan(&issuer, &tenant, &resource)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil && (issuer != f.Issuer || tenant != f.Tenant || resource != f.Resource) {
			return errAliasOwned
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO aliases(profile,alias,issuer,tenant,resource,file_id) VALUES(?,?,?,?,?,?) ON CONFLICT(profile,alias) DO UPDATE SET file_id=excluded.file_id`, f.Profile, f.Alias, f.Issuer, f.Tenant, f.Resource, f.ID)
		if err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO files(id,data) VALUES(?,?)`, f.ID, data); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE grants SET ready=1 WHERE issuer=? AND jti=?`, c.Issuer, c.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM cleanup WHERE file_id=?`, f.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (m *Metadata) byAlias(ctx context.Context, profile, alias string) (*File, error) {
	var id string
	if err := m.db.QueryRowContext(ctx, `SELECT file_id FROM aliases WHERE profile=? AND alias=?`, profile, alias).Scan(&id); err != nil {
		return nil, err
	}
	return m.get(ctx, id)
}

func (m *Metadata) markDeleted(ctx context.Context, f *File) error {
	// Keep the alias ownership record, even after deletion, to prevent takeover.
	keys := make([]string, 0, len(f.Variants))
	for _, v := range f.Variants {
		keys = append(keys, v.Key)
	}
	data, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE files SET deleted=1 WHERE id=?`, f.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO cleanup(file_id,objects,not_before) VALUES(?,?,0) ON CONFLICT(file_id) DO UPDATE SET objects=excluded.objects,not_before=0`, f.ID, data); err != nil {
		return err
	}
	return tx.Commit()
}

func (m *Metadata) stage(ctx context.Context, id string, keys []string) error {
	data, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	_, err = m.db.ExecContext(ctx, `INSERT INTO cleanup(file_id,objects,not_before) VALUES(?,?,?)`, id, data, time.Now().Add(time.Hour).Unix())
	return err
}

func (s *Server) cleanObjects(ctx context.Context, id string, keys []string) error {
	for _, key := range keys {
		if err := s.store.Delete(ctx, key); err != nil {
			return err
		}
	}
	_, err := s.meta.db.ExecContext(ctx, `DELETE FROM cleanup WHERE file_id=?`, id)
	return err
}

func (s *Server) cleanup(ctx context.Context) error {
	rows, err := s.meta.db.QueryContext(ctx, `SELECT file_id,objects FROM cleanup WHERE not_before<=? LIMIT 100`, time.Now().Unix())
	if err != nil {
		return err
	}
	type entry struct {
		id   string
		keys []string
	}
	var entries []entry
	for rows.Next() {
		var id string
		var data []byte
		if err = rows.Scan(&id, &data); err != nil {
			rows.Close()
			return err
		}
		var keys []string
		if err = json.Unmarshal(data, &keys); err != nil {
			rows.Close()
			return err
		}
		entries = append(entries, entry{id, keys})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if err = s.cleanObjects(ctx, e.id, e.keys); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
