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
	ErrAccountExists       = errors.New("account already exists")
	ErrInvalidCredentials  = errors.New("invalid credentials")
	ErrClaimInvalid        = errors.New("device claim is invalid or expired")
	ErrClaimAlreadyUsed    = errors.New("device claim was already used")
	ErrDedupeConflict      = errors.New("dedupe key belongs to another command")
	ErrForbidden           = errors.New("forbidden")
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
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	AccountLabel   *string `json:"account_label"`
	KeyFingerprint *string `json:"key_fingerprint,omitempty"`
	Status         string  `json:"status"`
	LastSeenAt     *int64  `json:"last_seen_at"`
	CreatedAt      int64   `json:"created_at"`
}

type Task struct {
	DeviceID      string  `json:"device_id"`
	ThreadID      string  `json:"thread_id"`
	Backend       *string `json:"backend,omitempty"`
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
  last_seen_at INTEGER, created_at INTEGER NOT NULL,
  device_public_key TEXT, device_key_fingerprint TEXT, claimed_by_user_id TEXT, revoked_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_devices_owner ON devices(owner_id);
CREATE TABLE IF NOT EXISTS tasks (
  device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE, thread_id TEXT NOT NULL,
  title TEXT NOT NULL, project TEXT, backend TEXT, status TEXT NOT NULL, summary TEXT, latest_message TEXT,
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
CREATE TRIGGER IF NOT EXISTS erase_delivered_message_text AFTER UPDATE OF state ON commands
WHEN NEW.kind='send_message' AND NEW.state IN ('completed','failed')
BEGIN UPDATE commands SET body_json=json_remove(body_json,'$.text') WHERE id=NEW.id; END;
CREATE TRIGGER IF NOT EXISTS erase_created_chat_text AFTER UPDATE OF state ON commands
WHEN NEW.kind='create_thread' AND NEW.state IN ('completed','failed')
BEGIN UPDATE commands SET body_json=json_remove(body_json,'$.text') WHERE id=NEW.id; END;
CREATE TABLE IF NOT EXISTS users (
  id TEXT PRIMARY KEY, email TEXT NOT NULL UNIQUE COLLATE NOCASE, display_name TEXT NOT NULL,
  password_hash TEXT NOT NULL, created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS workspaces (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('personal','team')),
  created_by_user_id TEXT NOT NULL REFERENCES users(id), created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS memberships (
  workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role TEXT NOT NULL CHECK(role IN ('owner','admin','member','viewer')), created_at INTEGER NOT NULL,
  PRIMARY KEY(workspace_id,user_id)
);
CREATE INDEX IF NOT EXISTS idx_memberships_user ON memberships(user_id,workspace_id);
CREATE TABLE IF NOT EXISTS controller_sessions (
  token_hash TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at INTEGER NOT NULL, created_at INTEGER NOT NULL, last_used_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_controller_sessions_user ON controller_sessions(user_id,expires_at);
CREATE TABLE IF NOT EXISTS device_claims (
  id TEXT PRIMARY KEY, claim_secret_hash TEXT NOT NULL, poll_token_hash TEXT NOT NULL UNIQUE,
  device_public_key TEXT NOT NULL, device_key_fingerprint TEXT NOT NULL,
  device_name TEXT NOT NULL, account_label TEXT, status TEXT NOT NULL DEFAULT 'pending',
  workspace_id TEXT, approved_by_user_id TEXT, device_id TEXT, wrapped_device_token TEXT,
  expires_at INTEGER NOT NULL, created_at INTEGER NOT NULL, approved_at INTEGER, delivered_at INTEGER,
  legacy_device_id TEXT
);
CREATE INDEX IF NOT EXISTS idx_device_claims_expiry ON device_claims(status,expires_at);
CREATE TABLE IF NOT EXISTS audit_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT, workspace_id TEXT NOT NULL, actor_user_id TEXT,
  action TEXT NOT NULL, target_type TEXT NOT NULL, target_id TEXT NOT NULL,
  metadata_json TEXT NOT NULL DEFAULT '{}', created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_workspace ON audit_events(workspace_id,created_at DESC,id DESC);
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
	if err := migrateDeviceIdentity(db); err != nil {
		return cleanup(err)
	}
	if err := migrateDeviceClaims(db); err != nil {
		return cleanup(err)
	}
	if err := migrateTaskBackend(db); err != nil {
		return cleanup(err)
	}
	// Remove the experimental server-side transcript store. History is local to phones.
	if _, err := db.Exec(`PRAGMA secure_delete=ON; DROP TABLE IF EXISTS conversation_messages; UPDATE tasks SET summary=NULL,latest_message=NULL; UPDATE commands SET body_json=json_remove(body_json,'$.text') WHERE kind IN ('send_message','create_thread') AND state IN ('completed','failed'); UPDATE commands SET result_json='{"submitted":true}' WHERE kind='send_message' AND state='completed'; UPDATE commands SET result_json=json_object('error',COALESCE(json_extract(result_json,'$.error'),'Message delivery failed')) WHERE kind='send_message' AND state='failed'; PRAGMA wal_checkpoint(TRUNCATE); PRAGMA user_version=6`); err != nil {
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
	err := s.db.QueryRowContext(ctx, "SELECT id FROM devices WHERE token_hash=? AND revoked_at IS NULL", hash(token)).Scan(&deviceID)
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
	taskStatement, err := tx.PrepareContext(ctx, `INSERT INTO tasks(device_id,thread_id,title,project,backend,status,summary,latest_message,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer taskStatement.Close()
	for _, task := range state.Tasks {
		if _, err := taskStatement.ExecContext(ctx, deviceID, task.ThreadID, task.Title, task.Project, task.Backend, task.Status, nil, nil, int64(task.UpdatedAt)); err != nil {
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
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,account_label,device_key_fingerprint,CASE WHEN last_seen_at IS NULL OR last_seen_at<? THEN 'offline' ELSE status END,last_seen_at,created_at FROM devices WHERE owner_id=? AND revoked_at IS NULL ORDER BY created_at`, offlineBefore, ownerID)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var item Device
		if err := rows.Scan(&item.ID, &item.Name, &item.AccountLabel, &item.KeyFingerprint, &item.Status, &item.LastSeenAt, &item.CreatedAt); err != nil {
			rows.Close()
			return result, err
		}
		result.Devices = append(result.Devices, item)
	}
	if err := rows.Close(); err != nil {
		return result, err
	}

	rows, err = s.db.QueryContext(ctx, `SELECT t.device_id,t.thread_id,t.title,t.project,t.backend,t.status,t.summary,t.latest_message,t.updated_at FROM tasks t JOIN devices d ON d.id=t.device_id WHERE d.owner_id=? AND d.revoked_at IS NULL ORDER BY t.updated_at DESC`, ownerID)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var item Task
		if err := rows.Scan(&item.DeviceID, &item.ThreadID, &item.Title, &item.Project, &item.Backend, &item.Status, &item.Summary, &item.LatestMessage, &item.UpdatedAt); err != nil {
			rows.Close()
			return result, err
		}
		result.Tasks = append(result.Tasks, item)
	}
	if err := rows.Close(); err != nil {
		return result, err
	}

	rows, err = s.db.QueryContext(ctx, `SELECT r.device_id,r.thread_id,r.request_id,r.turn_id,r.kind,r.payload_json,r.updated_at FROM pending_requests r JOIN devices d ON d.id=r.device_id WHERE d.owner_id=? AND d.revoked_at IS NULL ORDER BY r.updated_at DESC`, ownerID)
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

	rows, err = s.db.QueryContext(ctx, `SELECT c.id,c.device_id,c.kind,c.thread_id,c.state,c.created_at,c.claimed_at,c.completed_at,c.result_json FROM commands c JOIN devices d ON d.id=c.device_id WHERE d.owner_id=? AND d.revoked_at IS NULL ORDER BY c.created_at DESC LIMIT 100`, ownerID)
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
	if err := s.db.QueryRowContext(ctx, "SELECT 1 FROM devices WHERE id=? AND revoked_at IS NULL", deviceID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
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
	if err := s.db.QueryRowContext(ctx, "SELECT id FROM commands WHERE dedupe_key=? AND device_id=?", dedupeKey, deviceID).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return 0, false, ErrDedupeConflict
	} else if err != nil {
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var kind, threadID, workspaceID string
	var commandBody []byte
	var claimedByUserID *string
	err = tx.QueryRowContext(ctx, `SELECT c.kind,c.thread_id,d.owner_id,d.claimed_by_user_id,c.body_json
		FROM commands c JOIN devices d ON d.id=c.device_id
		WHERE c.id=? AND c.device_id=? AND c.state='claimed'`, commandID, deviceID).Scan(&kind, &threadID, &workspaceID, &claimedByUserID, &commandBody)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCommandNotClaimable
	}
	if err != nil {
		return err
	}
	if kind == "send_message" {
		// Desktop acknowledgements may include a turn payload. Keep only delivery status.
		if ok {
			encoded = []byte(`{"submitted":true}`)
		} else {
			message := "Message delivery failed"
			if fields, valid := result.(map[string]any); valid {
				if detail, valid := fields["error"].(string); valid {
					message = detail
				}
			}
			encoded, _ = json.Marshal(map[string]string{"error": message})
		}
	}
	if kind == "create_thread" {
		encoded, threadID, err = sanitizeCreatedThreadResult(ok, encoded, commandBody)
		if err != nil {
			return err
		}
	}
	now := time.Now().UnixMilli()
	operation, err := tx.ExecContext(ctx, `UPDATE commands SET state=?,thread_id=?,completed_at=?,result_json=? WHERE id=? AND device_id=? AND state='claimed'`, state, threadID, now, string(encoded), commandID, deviceID)
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
	// Legacy-enrolled devices have no claiming user and are intentionally kept
	// outside the account audit log during the compatibility period.
	if claimedByUserID != nil {
		action := "command.completed"
		if !ok {
			action = "command.failed"
		}
		if err := insertAudit(ctx, tx, workspaceID, nil, action, "command", fmt.Sprintf("%d", commandID), map[string]any{"deviceId": deviceID, "kind": kind, "threadId": threadID}); err != nil {
			return err
		}
	}
	return tx.Commit()
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

func migrateDeviceIdentity(db *sql.DB) error {
	rows, err := db.Query("PRAGMA table_info(devices)")
	if err != nil {
		return fmt.Errorf("inspect device schema: %w", err)
	}
	columns := map[string]bool{}
	for rows.Next() {
		var cid, required, primary int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &required, &defaultValue, &primary); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, column := range []struct{ name, definition string }{
		{"device_public_key", "TEXT"},
		{"device_key_fingerprint", "TEXT"},
		{"claimed_by_user_id", "TEXT"},
		{"revoked_at", "INTEGER"},
	} {
		if columns[column.name] {
			continue
		}
		if _, err := db.Exec("ALTER TABLE devices ADD COLUMN " + column.name + " " + column.definition); err != nil {
			return fmt.Errorf("add devices.%s: %w", column.name, err)
		}
	}
	if _, err := db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_devices_public_key ON devices(device_public_key) WHERE device_public_key IS NOT NULL"); err != nil {
		return fmt.Errorf("index device public keys: %w", err)
	}
	return nil
}

func migrateDeviceClaims(db *sql.DB) error {
	rows, err := db.Query("PRAGMA table_info(device_claims)")
	if err != nil {
		return fmt.Errorf("inspect device claim schema: %w", err)
	}
	hasLegacyDeviceID := false
	for rows.Next() {
		var cid, required, primary int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &required, &defaultValue, &primary); err != nil {
			rows.Close()
			return err
		}
		hasLegacyDeviceID = hasLegacyDeviceID || name == "legacy_device_id"
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !hasLegacyDeviceID {
		if _, err := db.Exec("ALTER TABLE device_claims ADD COLUMN legacy_device_id TEXT"); err != nil {
			return fmt.Errorf("add device_claims.legacy_device_id: %w", err)
		}
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
