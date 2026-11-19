// Package store 用 SQLite 保存用户、设备和通话记录。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db *sql.DB
}

type User struct {
	Username     string
	PasswordHash string
	Role         string
	CreatedAt    time.Time
}

type Participant struct {
	Identity string     `json:"identity"`
	JoinedAt time.Time  `json:"joinedAt"`
	LeftAt   *time.Time `json:"leftAt,omitempty"`
	Reason   string     `json:"reason,omitempty"`
}

type Device struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	SecretHash string     `json:"-"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastSeen   *time.Time `json:"lastSeen,omitempty"`
}

type CallRecord struct {
	ID         string     `json:"id"`
	Caller     string     `json:"caller"`
	Callee     string     `json:"callee"`
	StartedAt  time.Time  `json:"startedAt"`
	AnsweredAt *time.Time `json:"answeredAt,omitempty"`
	EndedAt    *time.Time `json:"endedAt,omitempty"`
	EndReason  string     `json:"endReason,omitempty"`
}

const schema = `
CREATE TABLE IF NOT EXISTS users (
	username      TEXT PRIMARY KEY,
	password_hash TEXT NOT NULL,
	created_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS devices (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	secret_hash TEXT NOT NULL,
	created_at  INTEGER NOT NULL,
	last_seen   INTEGER
);
CREATE TABLE IF NOT EXISTS calls (
	id          TEXT PRIMARY KEY,
	caller      TEXT NOT NULL,
	callee      TEXT NOT NULL,
	started_at  INTEGER NOT NULL,
	answered_at INTEGER,
	ended_at    INTEGER,
	end_reason  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_calls_started ON calls(started_at DESC);
CREATE TABLE IF NOT EXISTS call_participants (
	call_id   TEXT NOT NULL,
	identity  TEXT NOT NULL,
	joined_at INTEGER NOT NULL,
	left_at   INTEGER,
	reason    TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (call_id, identity, joined_at)
);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	st := &Store{db: db}
	if err := st.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return st, nil
}

func (s *Store) migrate(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(users)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	hasRole := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == "role" {
			hasRole = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !hasRole {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'user'`); err != nil {
			return fmt.Errorf("migrate users.role: %w", err)
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func unix(t time.Time) int64 { return t.UnixMilli() }

func fromUnix(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMilli(v.Int64)
	return &t
}

func (s *Store) UpsertUser(ctx context.Context, username, passwordHash string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users(username, password_hash, created_at) VALUES(?, ?, ?)
		ON CONFLICT(username) DO UPDATE SET password_hash = excluded.password_hash`,
		username, passwordHash, unix(time.Now()))
	return err
}

func (s *Store) GetUser(ctx context.Context, username string) (*User, error) {
	var u User
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT username, password_hash, role, created_at FROM users WHERE username = ?`, username).
		Scan(&u.Username, &u.PasswordHash, &u.Role, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt = time.UnixMilli(created)
	if u.Role == "" {
		u.Role = "user"
	}
	return &u, nil
}

func (s *Store) SetRole(ctx context.Context, username, role string) error {
	if role != "admin" && role != "user" {
		return fmt.Errorf("invalid role %q", role)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE users SET role = ? WHERE username = ?`, role, username)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteUser(ctx context.Context, username string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE username = ?`, username)
	return err
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT username, password_hash, role, created_at FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var created int64
		if err := rows.Scan(&u.Username, &u.PasswordHash, &u.Role, &created); err != nil {
			return nil, err
		}
		u.CreatedAt = time.UnixMilli(created)
		if u.Role == "" {
			u.Role = "user"
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) UpsertDevice(ctx context.Context, id, name, secretHash string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO devices(id, name, secret_hash, created_at) VALUES(?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, secret_hash = excluded.secret_hash`,
		id, name, secretHash, unix(time.Now()))
	return err
}

func (s *Store) DeleteDevice(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE id = ?`, id)
	return err
}

func scanDevice(sc interface{ Scan(...any) error }) (*Device, error) {
	var d Device
	var created int64
	var lastSeen sql.NullInt64
	if err := sc.Scan(&d.ID, &d.Name, &d.SecretHash, &created, &lastSeen); err != nil {
		return nil, err
	}
	d.CreatedAt = time.UnixMilli(created)
	d.LastSeen = fromUnix(lastSeen)
	return &d, nil
}

func (s *Store) GetDevice(ctx context.Context, id string) (*Device, error) {
	d, err := scanDevice(s.db.QueryRowContext(ctx,
		`SELECT id, name, secret_hash, created_at, last_seen FROM devices WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return d, err
}

func (s *Store) ListDevices(ctx context.Context) ([]*Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, secret_hash, created_at, last_seen FROM devices ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) TouchDevice(ctx context.Context, id string, t time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE devices SET last_seen = ? WHERE id = ?`, unix(t), id)
	return err
}

func (s *Store) RoomOpened(ctx context.Context, id, caller, callee string, t time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO calls(id, caller, callee, started_at, answered_at) VALUES(?, ?, ?, ?, ?)`,
		id, caller, callee, unix(t), unix(t))
	return err
}

func (s *Store) ParticipantJoined(ctx context.Context, callID, identity string, t time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO call_participants(call_id, identity, joined_at) VALUES(?, ?, ?)`,
		callID, identity, unix(t))
	return err
}

func (s *Store) ParticipantLeft(ctx context.Context, callID, identity, reason string, t time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE call_participants SET left_at = ?, reason = ?
		WHERE call_id = ? AND identity = ? AND left_at IS NULL`,
		unix(t), reason, callID, identity)
	return err
}

func (s *Store) RoomClosed(ctx context.Context, id, reason string, t time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE calls SET ended_at = ?, end_reason = ? WHERE id = ? AND ended_at IS NULL`,
		unix(t), reason, id)
	return err
}

// MarkDanglingCalls 服务重启时把上次未结束的通话标记为结束。
func (s *Store) MarkDanglingCalls(ctx context.Context, t time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE call_participants SET left_at = ?, reason = 'server_restart' WHERE left_at IS NULL`, unix(t)); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE calls SET ended_at = ?, end_reason = 'server_restart' WHERE ended_at IS NULL`, unix(t))
	return err
}

func (s *Store) ListCalls(ctx context.Context, party string, limit int) ([]*CallRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	q := `SELECT id, caller, callee, started_at, answered_at, ended_at, end_reason FROM calls`
	args := []any{}
	if party != "" {
		q += ` WHERE caller = ? OR callee = ? OR id IN (SELECT call_id FROM call_participants WHERE identity = ?)`
		args = append(args, party, party, party)
	}
	q += ` ORDER BY started_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CallRecord
	for rows.Next() {
		var c CallRecord
		var started int64
		var answered, ended sql.NullInt64
		if err := rows.Scan(&c.ID, &c.Caller, &c.Callee, &started, &answered, &ended, &c.EndReason); err != nil {
			return nil, err
		}
		c.StartedAt = time.UnixMilli(started)
		c.AnsweredAt = fromUnix(answered)
		c.EndedAt = fromUnix(ended)
		out = append(out, &c)
	}
	return out, rows.Err()
}

func (s *Store) GetCall(ctx context.Context, id string) (*CallRecord, error) {
	var c CallRecord
	var started int64
	var answered, ended sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT id, caller, callee, started_at, answered_at, ended_at, end_reason FROM calls WHERE id = ?`, id).
		Scan(&c.ID, &c.Caller, &c.Callee, &started, &answered, &ended, &c.EndReason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.StartedAt = time.UnixMilli(started)
	c.AnsweredAt = fromUnix(answered)
	c.EndedAt = fromUnix(ended)
	return &c, nil
}

func (s *Store) ListParticipants(ctx context.Context, callID string) ([]*Participant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT identity, joined_at, left_at, reason FROM call_participants WHERE call_id = ? ORDER BY joined_at`, callID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Participant
	for rows.Next() {
		var p Participant
		var joined int64
		var left sql.NullInt64
		if err := rows.Scan(&p.Identity, &joined, &left, &p.Reason); err != nil {
			return nil, err
		}
		p.JoinedAt = time.UnixMilli(joined)
		p.LeftAt = fromUnix(left)
		out = append(out, &p)
	}
	return out, rows.Err()
}
