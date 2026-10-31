// Package store persists server state (access tokens) in SQLite.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db *sql.DB
}

type Token struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	Revoked    bool       `json:"revoked"`
}

const schema = `
CREATE TABLE IF NOT EXISTS tokens (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	name         TEXT    NOT NULL,
	prefix       TEXT    NOT NULL,
	hash         TEXT    NOT NULL UNIQUE,
	created_at   INTEGER NOT NULL,
	last_used_at INTEGER,
	revoked      INTEGER NOT NULL DEFAULT 0
);`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema + statsSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// CreateToken stores a new token and returns its plaintext value, which is
// never persisted and cannot be recovered later.
func (s *Store) CreateToken(name string) (Token, string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return Token{}, "", err
	}
	plain := "tsm_" + hex.EncodeToString(buf)
	t := Token{Name: name, Prefix: plain[:10], CreatedAt: time.Now().UTC()}
	res, err := s.db.Exec(`INSERT INTO tokens (name, prefix, hash, created_at) VALUES (?, ?, ?, ?)`,
		t.Name, t.Prefix, hash(plain), t.CreatedAt.Unix())
	if err != nil {
		return Token{}, "", err
	}
	t.ID, _ = res.LastInsertId()
	return t, plain, nil
}

func (s *Store) ListTokens() ([]Token, error) {
	rows, err := s.db.Query(`SELECT id, name, prefix, created_at, last_used_at, revoked FROM tokens ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tokens := []Token{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

func (s *Store) RevokeToken(id int64) error {
	res, err := s.db.Exec(`UPDATE tokens SET revoked = 1 WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Authenticate resolves a plaintext token to an active token record and marks it used.
func (s *Store) Authenticate(plain string) (Token, error) {
	row := s.db.QueryRow(`SELECT id, name, prefix, created_at, last_used_at, revoked FROM tokens WHERE hash = ? AND revoked = 0`, hash(plain))
	t, err := scanToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, ErrNotFound
	}
	if err != nil {
		return Token{}, err
	}
	_, _ = s.db.Exec(`UPDATE tokens SET last_used_at = ? WHERE id = ?`, time.Now().UTC().Unix(), t.ID)
	return t, nil
}

type scanner interface{ Scan(dest ...any) error }

func scanToken(sc scanner) (Token, error) {
	var (
		t        Token
		created  int64
		lastUsed sql.NullInt64
	)
	if err := sc.Scan(&t.ID, &t.Name, &t.Prefix, &created, &lastUsed, &t.Revoked); err != nil {
		return Token{}, err
	}
	t.CreatedAt = time.Unix(created, 0).UTC()
	if lastUsed.Valid {
		lu := time.Unix(lastUsed.Int64, 0).UTC()
		t.LastUsedAt = &lu
	}
	return t, nil
}

func hash(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}
