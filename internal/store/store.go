// Package store is the SQLite persistence layer. The local database is the
// source of truth; Cloudflare is reconciled from it.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct{ db *sql.DB }

const schemaV1 = `
CREATE TABLE users (
  id INTEGER PRIMARY KEY,
  username TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE sessions (
  token_hash TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at INTEGER NOT NULL
);
CREATE TABLE settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE tunnels (
  id INTEGER PRIMARY KEY,
  cf_id TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL,
  mode TEXT NOT NULL,
  token_enc TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  last_sync_at INTEGER NOT NULL DEFAULT 0,
  last_sync_error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE hosts (
  id INTEGER PRIMARY KEY,
  tunnel_id INTEGER NOT NULL REFERENCES tunnels(id),
  hostname TEXT NOT NULL,
  path TEXT NOT NULL DEFAULT '',
  zone_id TEXT NOT NULL,
  zone_name TEXT NOT NULL,
  service TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  options TEXT NOT NULL DEFAULT '{}',
  access TEXT NOT NULL DEFAULT '{}',
  dns_record_id TEXT NOT NULL DEFAULT '',
  access_app_id TEXT NOT NULL DEFAULT '',
  access_policy_id TEXT NOT NULL DEFAULT '',
  sync_error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE (hostname, path)
);
`

func Open(path string) (*Store, error) {
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() +
		"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return nil, err
	}
	if v < 1 {
		tx, err := db.Begin()
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		if _, err := tx.Exec(schemaV1); err != nil {
			return nil, err
		}
		if _, err := tx.Exec("PRAGMA user_version = 1"); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func now() int64 { return time.Now().Unix() }

// ---- users & sessions ----

type User struct {
	ID           int64  `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"-"`
}

func (s *Store) CountUsers() (n int, err error) {
	err = s.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&n)
	return
}

// CreateFirstUser inserts the admin only when no user exists yet (atomic, so
// two concurrent setup requests cannot both succeed).
func (s *Store) CreateFirstUser(username, hash string) (bool, error) {
	res, err := s.db.Exec(`INSERT INTO users (username, password_hash, created_at)
		SELECT ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM users)`, username, hash, now())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) UserByName(username string) (*User, error) {
	u := &User{}
	err := s.db.QueryRow("SELECT id, username, password_hash FROM users WHERE username = ?", username).
		Scan(&u.ID, &u.Username, &u.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

func (s *Store) SetPassword(userID int64, hash string) error {
	_, err := s.db.Exec("UPDATE users SET password_hash = ? WHERE id = ?", hash, userID)
	return err
}

func (s *Store) CreateSession(tokenHash string, userID int64, ttl time.Duration) error {
	_, _ = s.db.Exec("DELETE FROM sessions WHERE expires_at < ?", now())
	_, err := s.db.Exec("INSERT INTO sessions (token_hash, user_id, expires_at) VALUES (?, ?, ?)",
		tokenHash, userID, time.Now().Add(ttl).Unix())
	return err
}

func (s *Store) SessionUser(tokenHash string) (*User, error) {
	u := &User{}
	err := s.db.QueryRow(`SELECT u.id, u.username, u.password_hash FROM sessions s
		JOIN users u ON u.id = s.user_id WHERE s.token_hash = ? AND s.expires_at > ?`, tokenHash, now()).
		Scan(&u.ID, &u.Username, &u.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

func (s *Store) DeleteSession(tokenHash string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE token_hash = ?", tokenHash)
	return err
}

// DeleteUserSessions drops every session of the user except keepHash.
func (s *Store) DeleteUserSessions(userID int64, keepHash string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE user_id = ? AND token_hash <> ?", userID, keepHash)
	return err
}

// ---- settings ----

func (s *Store) Setting(key string) (string, error) {
	var v string
	err := s.db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetSettings(kv map[string]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range kv {
		if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- tunnels ----

const (
	ModeManaged  = "managed"  // cloudflared runs inside this app
	ModeExternal = "external" // the connector runs elsewhere with the tunnel token
)

type Tunnel struct {
	ID            int64  `json:"id"`
	CFID          string `json:"cfId"`
	Name          string `json:"name"`
	Mode          string `json:"mode"`
	TokenEnc      string `json:"-"`
	Enabled       bool   `json:"enabled"`
	CreatedAt     int64  `json:"createdAt"`
	LastSyncAt    int64  `json:"lastSyncAt"`
	LastSyncError string `json:"lastSyncError"`
}

const tunnelCols = "id, cf_id, name, mode, token_enc, enabled, created_at, last_sync_at, last_sync_error"

type scanner interface{ Scan(...any) error }

func scanTunnel(r scanner) (*Tunnel, error) {
	t := &Tunnel{}
	err := r.Scan(&t.ID, &t.CFID, &t.Name, &t.Mode, &t.TokenEnc, &t.Enabled, &t.CreatedAt, &t.LastSyncAt, &t.LastSyncError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

func (s *Store) Tunnels() ([]*Tunnel, error) {
	rows, err := s.db.Query("SELECT " + tunnelCols + " FROM tunnels ORDER BY name COLLATE NOCASE")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Tunnel{}
	for rows.Next() {
		t, err := scanTunnel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) Tunnel(id int64) (*Tunnel, error) {
	return scanTunnel(s.db.QueryRow("SELECT "+tunnelCols+" FROM tunnels WHERE id = ?", id))
}

func (s *Store) InsertTunnel(t *Tunnel) error {
	t.CreatedAt = now()
	res, err := s.db.Exec(`INSERT INTO tunnels (cf_id, name, mode, token_enc, enabled, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, t.CFID, t.Name, t.Mode, t.TokenEnc, t.Enabled, t.CreatedAt)
	if err != nil {
		return err
	}
	t.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) UpdateTunnelRun(id int64, mode string, enabled bool) error {
	_, err := s.db.Exec("UPDATE tunnels SET mode = ?, enabled = ? WHERE id = ?", mode, enabled, id)
	return err
}

func (s *Store) SetTunnelSync(id int64, syncErr string) error {
	_, err := s.db.Exec("UPDATE tunnels SET last_sync_at = ?, last_sync_error = ? WHERE id = ?", now(), syncErr, id)
	return err
}

func (s *Store) DeleteTunnel(id int64) error {
	_, err := s.db.Exec("DELETE FROM tunnels WHERE id = ?", id)
	return err
}

// ---- hosts ----

// OriginOptions maps to cloudflared's originRequest block. Zero values are omitted.
type OriginOptions struct {
	NoTLSVerify            bool   `json:"noTLSVerify,omitempty"`
	HTTPHostHeader         string `json:"httpHostHeader,omitempty"`
	OriginServerName       string `json:"originServerName,omitempty"`
	HTTP2Origin            bool   `json:"http2Origin,omitempty"`
	DisableChunkedEncoding bool   `json:"disableChunkedEncoding,omitempty"`
	ConnectTimeout         int    `json:"connectTimeout,omitempty"` // seconds
}

// AccessConfig is the Cloudflare Access (Zero Trust) protection of a host.
type AccessConfig struct {
	Enabled         bool     `json:"enabled"`
	Emails          []string `json:"emails"`
	EmailDomains    []string `json:"emailDomains"`
	SessionDuration string   `json:"sessionDuration"`
}

type Host struct {
	ID             int64         `json:"id"`
	TunnelID       int64         `json:"tunnelId"`
	Hostname       string        `json:"hostname"`
	Path           string        `json:"path"`
	ZoneID         string        `json:"zoneId"`
	ZoneName       string        `json:"zoneName"`
	Service        string        `json:"service"`
	Enabled        bool          `json:"enabled"`
	Options        OriginOptions `json:"options"`
	Access         AccessConfig  `json:"access"`
	DNSRecordID    string        `json:"dnsRecordId"`
	AccessAppID    string        `json:"accessAppId"`
	AccessPolicyID string        `json:"-"`
	SyncError      string        `json:"syncError"`
	CreatedAt      int64         `json:"createdAt"`
	UpdatedAt      int64         `json:"updatedAt"`
}

const hostCols = `id, tunnel_id, hostname, path, zone_id, zone_name, service, enabled, options, access,
	dns_record_id, access_app_id, access_policy_id, sync_error, created_at, updated_at`

func scanHost(r scanner) (*Host, error) {
	h := &Host{}
	var opts, access string
	err := r.Scan(&h.ID, &h.TunnelID, &h.Hostname, &h.Path, &h.ZoneID, &h.ZoneName, &h.Service, &h.Enabled,
		&opts, &access, &h.DNSRecordID, &h.AccessAppID, &h.AccessPolicyID, &h.SyncError, &h.CreatedAt, &h.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(opts), &h.Options)
	_ = json.Unmarshal([]byte(access), &h.Access)
	if h.Access.Emails == nil {
		h.Access.Emails = []string{}
	}
	if h.Access.EmailDomains == nil {
		h.Access.EmailDomains = []string{}
	}
	return h, nil
}

func (s *Store) queryHosts(where string, args ...any) ([]*Host, error) {
	rows, err := s.db.Query("SELECT "+hostCols+" FROM hosts "+where+" ORDER BY hostname, path", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Host{}
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) Hosts() ([]*Host, error) { return s.queryHosts("") }

func (s *Store) HostsByTunnel(tunnelID int64) ([]*Host, error) {
	return s.queryHosts("WHERE tunnel_id = ?", tunnelID)
}

func (s *Store) HostsByHostname(hostname string) ([]*Host, error) {
	return s.queryHosts("WHERE hostname = ?", hostname)
}

func (s *Store) Host(id int64) (*Host, error) {
	return scanHost(s.db.QueryRow("SELECT "+hostCols+" FROM hosts WHERE id = ?", id))
}

func IsUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func (s *Store) InsertHost(h *Host) error {
	opts, _ := json.Marshal(h.Options)
	access, _ := json.Marshal(h.Access)
	h.CreatedAt, h.UpdatedAt = now(), now()
	res, err := s.db.Exec(`INSERT INTO hosts (tunnel_id, hostname, path, zone_id, zone_name, service, enabled,
		options, access, dns_record_id, access_app_id, access_policy_id, sync_error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		h.TunnelID, h.Hostname, h.Path, h.ZoneID, h.ZoneName, h.Service, h.Enabled, string(opts), string(access),
		h.DNSRecordID, h.AccessAppID, h.AccessPolicyID, h.SyncError, h.CreatedAt, h.UpdatedAt)
	if err != nil {
		return err
	}
	h.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) UpdateHost(h *Host) error {
	opts, _ := json.Marshal(h.Options)
	access, _ := json.Marshal(h.Access)
	h.UpdatedAt = now()
	_, err := s.db.Exec(`UPDATE hosts SET tunnel_id = ?, hostname = ?, path = ?, zone_id = ?, zone_name = ?,
		service = ?, enabled = ?, options = ?, access = ?, dns_record_id = ?, access_app_id = ?,
		access_policy_id = ?, sync_error = ?, updated_at = ? WHERE id = ?`,
		h.TunnelID, h.Hostname, h.Path, h.ZoneID, h.ZoneName, h.Service, h.Enabled, string(opts), string(access),
		h.DNSRecordID, h.AccessAppID, h.AccessPolicyID, h.SyncError, h.UpdatedAt, h.ID)
	return err
}

func (s *Store) DeleteHost(id int64) error {
	_, err := s.db.Exec("DELETE FROM hosts WHERE id = ?", id)
	return err
}
