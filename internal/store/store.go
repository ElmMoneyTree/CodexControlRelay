package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codex-control/relay-go/internal/protocol"
	_ "modernc.org/sqlite"
)

var (
	ErrPairingCodeInvalid  = errors.New("pairing code is invalid or expired")
	ErrDeviceNotFound      = errors.New("device not found")
	ErrCommandNotClaimable = errors.New("command is not claimable")
)

type Store struct{ db *sql.DB }

type PairingCode struct {
	Code      string `json:"code"`
	ExpiresAt int64  `json:"expiresAt"`
}

type Enrollment struct {
	DeviceID string `json:"deviceId"`
	Token    string `json:"token"`
}

type State struct {
	Devices  []Device         `json:"devices"`
	Tasks    []Task           `json:"tasks"`
	Requests []PendingRequest `json:"requests"`
	Commands []CommandStatus  `json:"commands"`
}

type Device struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	AccountLabel *string `json:"account_label"`
	Status       string  `json:"status"`
	LastSeenAt   *int64  `json:"last_seen_at"`
	CreatedAt    int64   `json:"created_at"`
}

type Task struct {
	DeviceID      string  `json:"device_id"`
	ThreadID      string  `json:"thread_id"`
	Title         string  `json:"title"`
	Project       *string `json:"project"`
	Status        string  `json:"status"`
	Summary       *string `json:"summary"`
	LatestMessage *string `json:"latest_message"`
	UpdatedAt     int64   `json:"updated_at"`
}

type PendingRequest struct {
	DeviceID  string         `json:"device_id"`
	ThreadID  string         `json:"thread_id"`
	RequestID string         `json:"request_id"`
	TurnID    *string        `json:"turn_id"`
	Kind      string         `json:"kind"`
	Payload   map[string]any `json:"payload"`
	UpdatedAt int64          `json:"updated_at"`
}

type CommandStatus struct {
	ID          int64  `json:"id"`
	DeviceID    string `json:"device_id"`
	Kind        string `json:"kind"`
	ThreadID    string `json:"thread_id"`
	State       string `json:"state"`
	CreatedAt   int64  `json:"created_at"`
	ClaimedAt   *int64 `json:"claimed_at"`
	CompletedAt *int64 `json:"completed_at"`
	Result      any    `json:"result"`
}

type AgentCommand map[string]any

const schema = `
CREATE TABLE IF NOT EXISTS pairing_codes (
  code_hash TEXT PRIMARY KEY, owner_id TEXT NOT NULL, expires_at INTEGER NOT NULL, created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS devices (
  id TEXT PRIMARY KEY, owner_id TEXT NOT NULL, token_hash TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL, account_label TEXT, status TEXT NOT NULL DEFAULT 'offline',
  last_seen_at INTEGER, created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_devices_owner ON devices(owner_id);
CREATE TABLE IF NOT EXISTS tasks (
  device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE, thread_id TEXT NOT NULL,
  title TEXT NOT NULL, project TEXT, status TEXT NOT NULL, summary TEXT, latest_message TEXT,
  updated_at INTEGER NOT NULL, PRIMARY KEY(device_id, thread_id)
);
CREATE INDEX IF NOT EXISTS idx_tasks_device_updated ON tasks(device_id, updated_at DESC);
CREATE TABLE IF NOT EXISTS pending_requests (
  device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE, thread_id TEXT NOT NULL,
  request_id TEXT NOT NULL, turn_id TEXT, kind TEXT NOT NULL, payload_json TEXT NOT NULL,
  updated_at INTEGER NOT NULL, PRIMARY KEY(device_id, thread_id, request_id)
);
CREATE INDEX IF NOT EXISTS idx_requests_device ON pending_requests(device_id, updated_at DESC);
CREATE TABLE IF NOT EXISTS commands (
  id INTEGER PRIMARY KEY AUTOINCREMENT, device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  kind TEXT NOT NULL, thread_id TEXT NOT NULL, body_json TEXT NOT NULL, dedupe_key TEXT NOT NULL UNIQUE,
  state TEXT NOT NULL DEFAULT 'queued', created_at INTEGER NOT NULL, claimed_at INTEGER,
  completed_at INTEGER, result_json TEXT
);
CREATE INDEX IF NOT EXISTS idx_commands_poll ON commands(device_id, state, id);
`

func Open(path string, busyTimeout time.Duration) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	cleanup := func(err error) (*Store, error) { _ = db.Close(); return nil, err }
	if _, err := db.Exec(fmt.Sprintf("PRAGMA busy_timeout=%d; PRAGMA foreign_keys=ON; PRAGMA journal_mode=WAL;", busyTimeout.Milliseconds())); err != nil {
		return cleanup(fmt.Errorf("configure sqlite: %w", err))
	}
	if _, err := db.Exec(schema); err != nil {
		return cleanup(fmt.Errorf("initialize sqlite schema: %w", err))
	}
	if err := migrateNullableTurnID(db); err != nil {
		return cleanup(err)
	}
	if _, err := db.Exec("PRAGMA user_version=2"); err != nil {
		return cleanup(fmt.Errorf("set sqlite schema version: %w", err))
	}
	if err := db.Ping(); err != nil {
		return cleanup(fmt.Errorf("ping sqlite: %w", err))
	}
	if path != ":memory:" {
		// The database contains task text and token hashes. Unix deployments
		// should not make a newly created database readable by other users.
		if err := os.Chmod(path, 0o600); err != nil {
			return cleanup(fmt.Errorf("protect sqlite file: %w", err))
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error                   { return s.db.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) CreatePairingCode(ctx context.Context, ownerID string, ttl time.Duration) (PairingCode, error) {
	codeBytes := make([]byte, 5)
	if _, err := rand.Read(codeBytes); err != nil {
		return PairingCode{}, err
	}
	code := strings.ToUpper(base64.RawURLEncoding.EncodeToString(codeBytes))
	now := time.Now().UnixMilli()
	expiresAt := now + ttl.Milliseconds()
	if _, err := s.db.ExecContext(ctx, "DELETE FROM pairing_codes WHERE expires_at<=?", now); err != nil {
		return PairingCode{}, err
	}
	if _, err := s.db.ExecContext(ctx, "INSERT INTO pairing_codes(code_hash,owner_id,expires_at,created_at) VALUES(?,?,?,?)", hash(code), ownerID, expiresAt, now); err != nil {
		return PairingCode{}, err
	}
	return PairingCode{Code: code, ExpiresAt: expiresAt}, nil
}

func (s *Store) Enroll(ctx context.Context, code, name string, accountLabel *string) (Enrollment, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Enrollment{}, err
	}
	defer tx.Rollback()
	var ownerID string
	err = tx.QueryRowContext(ctx, "SELECT owner_id FROM pairing_codes WHERE code_hash=? AND expires_at>?", hash(code), time.Now().UnixMilli()).Scan(&ownerID)
	if errors.Is(err, sql.ErrNoRows) {
		return Enrollment{}, ErrPairingCodeInvalid
	}
	if err != nil {
		return Enrollment{}, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM pairing_codes WHERE code_hash=?", hash(code)); err != nil {
		return Enrollment{}, err
	}
	deviceID, err := randomUUID()
	if err != nil {
		return Enrollment{}, err
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return Enrollment{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO devices(id,owner_id,token_hash,name,account_label,status,last_seen_at,created_at) VALUES(?,?,?,?,?,?,?,?)`, deviceID, ownerID, hash(token), name, accountLabel, "online", now, now); err != nil {
		return Enrollment{}, err
	}
	if err := tx.Commit(); err != nil {
		return Enrollment{}, err
	}
	return Enrollment{DeviceID: deviceID, Token: token}, nil
}

func (s *Store) AuthenticateAgent(ctx context.Context, token string) (string, bool, error) {
	var deviceID string
	err := s.db.QueryRowContext(ctx, "SELECT id FROM devices WHERE token_hash=?", hash(token)).Scan(&deviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return deviceID, true, nil
}

func (s *Store) Sync(ctx context.Context, deviceID string, state protocol.AgentState, requests []protocol.ValidatedRequest) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE devices SET status='online',last_seen_at=? WHERE id=?", time.Now().UnixMilli(), deviceID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM tasks WHERE device_id=?", deviceID); err != nil {
		return err
	}
	taskStatement, err := tx.PrepareContext(ctx, `INSERT INTO tasks(device_id,thread_id,title,project,status,summary,latest_message,updated_at) VALUES(?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer taskStatement.Close()
	for _, task := range state.Tasks {
		if _, err := taskStatement.ExecContext(ctx, deviceID, task.ThreadID, task.Title, task.Project, task.Status, task.Summary, task.LatestMessage, int64(task.UpdatedAt)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM pending_requests WHERE device_id=?", deviceID); err != nil {
		return err
	}
	requestStatement, err := tx.PrepareContext(ctx, `INSERT INTO pending_requests(device_id,thread_id,request_id,turn_id,kind,payload_json,updated_at) VALUES(?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer requestStatement.Close()
	for _, item := range requests {
		payload, err := json.Marshal(item.Payload)
		if err != nil {
			return err
		}
		if _, err := requestStatement.ExecContext(ctx, deviceID, item.ThreadID, item.RequestID, item.TurnID, item.Kind, string(payload), item.UpdatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) State(ctx context.Context, ownerID string, offlineBefore int64) (State, error) {
	result := State{Devices: []Device{}, Tasks: []Task{}, Requests: []PendingRequest{}, Commands: []CommandStatus{}}
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,account_label,CASE WHEN last_seen_at IS NULL OR last_seen_at<? THEN 'offline' ELSE status END,last_seen_at,created_at FROM devices WHERE owner_id=? ORDER BY created_at`, offlineBefore, ownerID)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var item Device
		if err := rows.Scan(&item.ID, &item.Name, &item.AccountLabel, &item.Status, &item.LastSeenAt, &item.CreatedAt); err != nil {
			rows.Close()
			return result, err
		}
		result.Devices = append(result.Devices, item)
	}
	if err := rows.Close(); err != nil {
		return result, err
	}

	rows, err = s.db.QueryContext(ctx, `SELECT t.device_id,t.thread_id,t.title,t.project,t.status,t.summary,t.latest_message,t.updated_at FROM tasks t JOIN devices d ON d.id=t.device_id WHERE d.owner_id=? ORDER BY t.updated_at DESC`, ownerID)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var item Task
		if err := rows.Scan(&item.DeviceID, &item.ThreadID, &item.Title, &item.Project, &item.Status, &item.Summary, &item.LatestMessage, &item.UpdatedAt); err != nil {
			rows.Close()
			return result, err
		}
		result.Tasks = append(result.Tasks, item)
	}
	if err := rows.Close(); err != nil {
		return result, err
	}

	rows, err = s.db.QueryContext(ctx, `SELECT r.device_id,r.thread_id,r.request_id,r.turn_id,r.kind,r.payload_json,r.updated_at FROM pending_requests r JOIN devices d ON d.id=r.device_id WHERE d.owner_id=? ORDER BY r.updated_at DESC`, ownerID)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var item PendingRequest
		var payload []byte
		if err := rows.Scan(&item.DeviceID, &item.ThreadID, &item.RequestID, &item.TurnID, &item.Kind, &payload, &item.UpdatedAt); err != nil {
			rows.Close()
			return result, err
		}
		if err := json.Unmarshal(payload, &item.Payload); err != nil {
			rows.Close()
			return result, err
		}
		result.Requests = append(result.Requests, item)
	}
	if err := rows.Close(); err != nil {
		return result, err
	}

	rows, err = s.db.QueryContext(ctx, `SELECT c.id,c.device_id,c.kind,c.thread_id,c.state,c.created_at,c.claimed_at,c.completed_at,c.result_json FROM commands c JOIN devices d ON d.id=c.device_id WHERE d.owner_id=? ORDER BY c.created_at DESC LIMIT 100`, ownerID)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var item CommandStatus
		var raw []byte
		if err := rows.Scan(&item.ID, &item.DeviceID, &item.Kind, &item.ThreadID, &item.State, &item.CreatedAt, &item.ClaimedAt, &item.CompletedAt, &raw); err != nil {
			rows.Close()
			return result, err
		}
		if len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &item.Result); err != nil {
				return result, err
			}
		}
		result.Commands = append(result.Commands, item)
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	return result, nil
}

func (s *Store) CreateCommand(ctx context.Context, deviceID string, command protocol.Command, dedupeKey string) (int64, bool, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, "SELECT 1 FROM devices WHERE id=?", deviceID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return 0, false, ErrDeviceNotFound
	} else if err != nil {
		return 0, false, err
	}
	if dedupeKey == "" {
		var err error
		dedupeKey, err = randomUUID()
		if err != nil {
			return 0, false, err
		}
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO commands(device_id,kind,thread_id,body_json,dedupe_key,created_at) VALUES(?,?,?,?,?,?) ON CONFLICT(dedupe_key) DO NOTHING`, deviceID, command.Kind, command.ThreadID, string(command.Body), dedupeKey, time.Now().UnixMilli())
	if err != nil {
		return 0, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	if rows == 1 {
		id, err := result.LastInsertId()
		return id, true, err
	}
	var id int64
	if err := s.db.QueryRowContext(ctx, "SELECT id FROM commands WHERE dedupe_key=?", dedupeKey).Scan(&id); err != nil {
		return 0, false, err
	}
	return id, false, nil
}

func (s *Store) ClaimCommands(ctx context.Context, deviceID string, limit int) ([]AgentCommand, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,body_json,state FROM commands WHERE device_id=? AND state IN ('queued','claimed') ORDER BY id LIMIT ?`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	type row struct {
		id    int64
		body  []byte
		state string
	}
	selected := []row{}
	for rows.Next() {
		var item row
		if err := rows.Scan(&item.id, &item.body, &item.state); err != nil {
			rows.Close()
			return nil, err
		}
		selected = append(selected, item)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	for _, item := range selected {
		if item.state == "queued" {
			if _, err := tx.ExecContext(ctx, "UPDATE commands SET state='claimed',claimed_at=? WHERE id=? AND state='queued'", now, item.id); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	commands := make([]AgentCommand, 0, len(selected))
	for _, item := range selected {
		command := AgentCommand{}
		if err := json.Unmarshal(item.body, &command); err != nil {
			return nil, err
		}
		command["id"] = item.id
		commands = append(commands, command)
	}
	return commands, nil
}

func (s *Store) Acknowledge(ctx context.Context, deviceID string, commandID int64, ok bool, result any) error {
	state := "failed"
	if ok {
		state = "completed"
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	operation, err := s.db.ExecContext(ctx, `UPDATE commands SET state=?,completed_at=?,result_json=? WHERE id=? AND device_id=? AND state='claimed'`, state, time.Now().UnixMilli(), string(encoded), commandID, deviceID)
	if err != nil {
		return err
	}
	changed, err := operation.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrCommandNotClaimable
	}
	return nil
}

func migrateNullableTurnID(db *sql.DB) error {
	rows, err := db.Query("PRAGMA table_info(pending_requests)")
	if err != nil {
		return fmt.Errorf("inspect pending request schema: %w", err)
	}
	notNull := false
	for rows.Next() {
		var cid int
		var name, kind string
		var required, primary int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &required, &defaultValue, &primary); err != nil {
			rows.Close()
			return err
		}
		if name == "turn_id" && required == 1 {
			notNull = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !notNull {
		return nil
	}
	_, err = db.Exec(`BEGIN IMMEDIATE;
ALTER TABLE pending_requests RENAME TO pending_requests_legacy;
CREATE TABLE pending_requests (
  device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE, thread_id TEXT NOT NULL,
  request_id TEXT NOT NULL, turn_id TEXT, kind TEXT NOT NULL, payload_json TEXT NOT NULL,
  updated_at INTEGER NOT NULL, PRIMARY KEY(device_id, thread_id, request_id)
);
INSERT INTO pending_requests SELECT * FROM pending_requests_legacy;
DROP TABLE pending_requests_legacy;
CREATE INDEX idx_requests_device ON pending_requests(device_id, updated_at DESC);
COMMIT;`)
	if err != nil {
		_, _ = db.Exec("ROLLBACK")
		return fmt.Errorf("migrate pending request turn_id: %w", err)
	}
	return nil
}

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func randomUUID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	data[6] = (data[6] & 0x0f) | 0x40
	data[8] = (data[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(data)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}
