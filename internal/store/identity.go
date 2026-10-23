package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"codex-control/relay-go/internal/protocol"
	"golang.org/x/crypto/argon2"
)

const (
	passwordMemoryKiB   = 64 * 1024
	passwordIterations  = 3
	passwordParallelism = 2
	passwordKeyBytes    = 32
)

type User struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	CreatedAt   int64  `json:"createdAt"`
}

type Workspace struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Role      string `json:"role"`
	CreatedAt int64  `json:"createdAt"`
}

type WorkspaceMember struct {
	UserID      string `json:"userId"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
	CreatedAt   int64  `json:"createdAt"`
}

type AuthResult struct {
	Token      string      `json:"token"`
	ExpiresAt  int64       `json:"expiresAt"`
	User       User        `json:"user"`
	Workspaces []Workspace `json:"workspaces"`
}

type Principal struct {
	UserID string
	Email  string
}

type DeviceClaim struct {
	ID                   string  `json:"id"`
	ClaimSecret          string  `json:"claimSecret,omitempty"`
	PollToken            string  `json:"pollToken,omitempty"`
	DevicePublicKey      string  `json:"devicePublicKey"`
	DeviceKeyFingerprint string  `json:"deviceKeyFingerprint"`
	DeviceName           string  `json:"deviceName"`
	AccountLabel         *string `json:"accountLabel,omitempty"`
	Status               string  `json:"status"`
	WorkspaceID          *string `json:"workspaceId,omitempty"`
	DeviceID             *string `json:"deviceId,omitempty"`
	DeviceToken          string  `json:"deviceToken,omitempty"`
	RelayURL             string  `json:"relayUrl,omitempty"`
	LegacyDeviceID       *string `json:"-"`
	ExpiresAt            int64   `json:"expiresAt"`
	CreatedAt            int64   `json:"createdAt"`
}

// ClaimProgress is safe to return to the QR page without an account session.
// Device credentials remain available only through the separate poll token.
type ClaimProgress struct {
	Status    string `json:"status"`
	ExpiresAt int64  `json:"expiresAt"`
}

type AuditEvent struct {
	ID          int64          `json:"id"`
	WorkspaceID string         `json:"workspaceId"`
	ActorUserID *string        `json:"actorUserId"`
	Action      string         `json:"action"`
	TargetType  string         `json:"targetType"`
	TargetID    string         `json:"targetId"`
	Metadata    map[string]any `json:"metadata"`
	CreatedAt   int64          `json:"createdAt"`
}

func (s *Store) CreateAccount(ctx context.Context, email, displayName, password, personalWorkspaceName string, sessionTTL time.Duration) (AuthResult, error) {
	email = normalizeEmail(email)
	displayName = strings.TrimSpace(displayName)
	personalWorkspaceName = strings.TrimSpace(personalWorkspaceName)
	if !validEmail(email) || displayName == "" || len([]rune(displayName)) > 200 || len(password) < 12 || len(password) > 1024 {
		return AuthResult{}, ErrInvalidCredentials
	}
	if personalWorkspaceName == "" {
		personalWorkspaceName = displayName + " 的空间"
	}
	if len([]rune(personalWorkspaceName)) > 200 {
		return AuthResult{}, ErrInvalidCredentials
	}
	passwordHash, err := encodePassword(password)
	if err != nil {
		return AuthResult{}, err
	}
	userID, err := randomUUID()
	if err != nil {
		return AuthResult{}, err
	}
	workspaceID, err := randomUUID()
	if err != nil {
		return AuthResult{}, err
	}
	token, tokenHash, err := newSecret()
	if err != nil {
		return AuthResult{}, err
	}
	now := time.Now().UnixMilli()
	expiresAt := now + sessionTTL.Milliseconds()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AuthResult{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT INTO users(id,email,display_name,password_hash,created_at) VALUES(?,?,?,?,?)", userID, email, displayName, passwordHash, now); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return AuthResult{}, ErrAccountExists
		}
		return AuthResult{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO workspaces(id,name,kind,created_by_user_id,created_at) VALUES(?,?,?,?,?)", workspaceID, personalWorkspaceName, "personal", userID, now); err != nil {
		return AuthResult{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO memberships(workspace_id,user_id,role,created_at) VALUES(?,?,?,?)", workspaceID, userID, "owner", now); err != nil {
		return AuthResult{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO controller_sessions(token_hash,user_id,expires_at,created_at,last_used_at) VALUES(?,?,?,?,?)", tokenHash, userID, expiresAt, now, now); err != nil {
		return AuthResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return AuthResult{}, err
	}
	return AuthResult{
		Token: token, ExpiresAt: expiresAt,
		User:       User{ID: userID, Email: email, DisplayName: displayName, CreatedAt: now},
		Workspaces: []Workspace{{ID: workspaceID, Name: personalWorkspaceName, Kind: "personal", Role: "owner", CreatedAt: now}},
	}, nil
}

func (s *Store) Login(ctx context.Context, email, password string, sessionTTL time.Duration) (AuthResult, error) {
	var user User
	var passwordHash string
	err := s.db.QueryRowContext(ctx, "SELECT id,email,display_name,password_hash,created_at FROM users WHERE email=?", normalizeEmail(email)).Scan(&user.ID, &user.Email, &user.DisplayName, &passwordHash, &user.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		_, _ = encodePassword(password)
		return AuthResult{}, ErrInvalidCredentials
	}
	if err != nil {
		return AuthResult{}, err
	}
	ok, err := verifyPassword(password, passwordHash)
	if err != nil || !ok {
		return AuthResult{}, ErrInvalidCredentials
	}
	token, tokenHash, err := newSecret()
	if err != nil {
		return AuthResult{}, err
	}
	now := time.Now().UnixMilli()
	expiresAt := now + sessionTTL.Milliseconds()
	if _, err := s.db.ExecContext(ctx, "DELETE FROM controller_sessions WHERE expires_at<=?", now); err != nil {
		return AuthResult{}, err
	}
	if _, err := s.db.ExecContext(ctx, "INSERT INTO controller_sessions(token_hash,user_id,expires_at,created_at,last_used_at) VALUES(?,?,?,?,?)", tokenHash, user.ID, expiresAt, now, now); err != nil {
		return AuthResult{}, err
	}
	workspaces, err := s.ListWorkspaces(ctx, user.ID)
	if err != nil {
		return AuthResult{}, err
	}
	return AuthResult{Token: token, ExpiresAt: expiresAt, User: user, Workspaces: workspaces}, nil
}

func (s *Store) AuthenticateController(ctx context.Context, token string) (Principal, bool, error) {
	var result Principal
	now := time.Now().UnixMilli()
	err := s.db.QueryRowContext(ctx, `SELECT u.id,u.email FROM controller_sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=? AND s.expires_at>?`, hash(token), now).Scan(&result.UserID, &result.Email)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, false, nil
	}
	if err != nil {
		return Principal{}, false, err
	}
	_, _ = s.db.ExecContext(ctx, "UPDATE controller_sessions SET last_used_at=? WHERE token_hash=?", now, hash(token))
	return result, true, nil
}

func (s *Store) Logout(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM controller_sessions WHERE token_hash=?", hash(token))
	return err
}

func (s *Store) User(ctx context.Context, userID string) (User, error) {
	var user User
	err := s.db.QueryRowContext(ctx, "SELECT id,email,display_name,created_at FROM users WHERE id=?", userID).Scan(&user.ID, &user.Email, &user.DisplayName, &user.CreatedAt)
	return user, err
}

func (s *Store) ListWorkspaces(ctx context.Context, userID string) ([]Workspace, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.id,w.name,w.kind,m.role,w.created_at FROM memberships m JOIN workspaces w ON w.id=m.workspace_id WHERE m.user_id=? ORDER BY w.created_at,w.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Workspace{}
	for rows.Next() {
		var item Workspace
		if err := rows.Scan(&item.ID, &item.Name, &item.Kind, &item.Role, &item.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) CreateWorkspace(ctx context.Context, userID, name string) (Workspace, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 200 {
		return Workspace{}, ErrForbidden
	}
	id, err := randomUUID()
	if err != nil {
		return Workspace{}, err
	}
	now := time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Workspace{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT INTO workspaces(id,name,kind,created_by_user_id,created_at) VALUES(?,?,?,?,?)", id, name, "team", userID, now); err != nil {
		return Workspace{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO memberships(workspace_id,user_id,role,created_at) VALUES(?,?,?,?)", id, userID, "owner", now); err != nil {
		return Workspace{}, err
	}
	if err := insertAudit(ctx, tx, id, &userID, "workspace.created", "workspace", id, map[string]any{"name": name}); err != nil {
		return Workspace{}, err
	}
	if err := tx.Commit(); err != nil {
		return Workspace{}, err
	}
	return Workspace{ID: id, Name: name, Kind: "team", Role: "owner", CreatedAt: now}, nil
}

func (s *Store) AddWorkspaceMember(ctx context.Context, actorUserID, workspaceID, email, role string) error {
	if role != "admin" && role != "member" && role != "viewer" {
		return ErrForbidden
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	actorRole, err := membershipRole(ctx, tx, actorUserID, workspaceID)
	if err != nil || (actorRole != "owner" && actorRole != "admin") {
		return ErrForbidden
	}
	var workspaceKind string
	if err := tx.QueryRowContext(ctx, "SELECT kind FROM workspaces WHERE id=?", workspaceID).Scan(&workspaceKind); err != nil || workspaceKind != "team" {
		return ErrForbidden
	}
	var userID string
	if err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE email=?", normalizeEmail(email)).Scan(&userID); errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidCredentials
	} else if err != nil {
		return err
	}
	var currentRole string
	err = tx.QueryRowContext(ctx, "SELECT role FROM memberships WHERE workspace_id=? AND user_id=?", workspaceID, userID).Scan(&currentRole)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if currentRole == "owner" || actorUserID == userID || (actorRole == "admin" && (role == "admin" || currentRole == "admin")) {
		return ErrForbidden
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO memberships(workspace_id,user_id,role,created_at) VALUES(?,?,?,?) ON CONFLICT(workspace_id,user_id) DO UPDATE SET role=excluded.role`, workspaceID, userID, role, now); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, workspaceID, &actorUserID, "member.added", "user", userID, map[string]any{"role": role}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListWorkspaceMembers(ctx context.Context, userID, workspaceID string) ([]WorkspaceMember, error) {
	ok, err := s.CanUseWorkspace(ctx, userID, workspaceID, false)
	if err != nil || !ok {
		return nil, ErrForbidden
	}
	rows, err := s.db.QueryContext(ctx, `SELECT u.id,u.email,u.display_name,m.role,m.created_at
		FROM memberships m JOIN users u ON u.id=m.user_id
		WHERE m.workspace_id=?
		ORDER BY CASE m.role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 WHEN 'member' THEN 2 ELSE 3 END,u.display_name,u.email`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	members := []WorkspaceMember{}
	for rows.Next() {
		var member WorkspaceMember
		if err := rows.Scan(&member.UserID, &member.Email, &member.DisplayName, &member.Role, &member.CreatedAt); err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	return members, rows.Err()
}

func (s *Store) CanUseWorkspace(ctx context.Context, userID, workspaceID string, write bool) (bool, error) {
	var role string
	err := s.db.QueryRowContext(ctx, "SELECT role FROM memberships WHERE user_id=? AND workspace_id=?", userID, workspaceID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !write || role != "viewer", nil
}

func (s *Store) CreateDeviceClaim(ctx context.Context, publicKey, fingerprint, deviceName string, accountLabel, legacyDeviceID *string, ttl time.Duration) (DeviceClaim, error) {
	id, err := randomUUID()
	if err != nil {
		return DeviceClaim{}, err
	}
	claimSecret, claimHash, err := newSecret()
	if err != nil {
		return DeviceClaim{}, err
	}
	pollToken, pollHash, err := newSecret()
	if err != nil {
		return DeviceClaim{}, err
	}
	now := time.Now().UnixMilli()
	expiresAt := now + ttl.Milliseconds()
	if _, err := s.db.ExecContext(ctx, "UPDATE device_claims SET status='expired',wrapped_device_token=NULL WHERE status='pending' AND expires_at<=?", now); err != nil {
		return DeviceClaim{}, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO device_claims(id,claim_secret_hash,poll_token_hash,device_public_key,device_key_fingerprint,device_name,account_label,status,expires_at,created_at,legacy_device_id) VALUES(?,?,?,?,?,?,?,'pending',?,?,?)`, id, claimHash, pollHash, publicKey, fingerprint, deviceName, accountLabel, expiresAt, now, legacyDeviceID); err != nil {
		return DeviceClaim{}, err
	}
	return DeviceClaim{ID: id, ClaimSecret: claimSecret, PollToken: pollToken, DevicePublicKey: publicKey, DeviceKeyFingerprint: fingerprint, DeviceName: deviceName, AccountLabel: accountLabel, Status: "pending", LegacyDeviceID: legacyDeviceID, ExpiresAt: expiresAt, CreatedAt: now}, nil
}

func (s *Store) CanMigrateLegacyDevice(ctx context.Context, deviceID string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM devices WHERE id=? AND revoked_at IS NULL AND claimed_by_user_id IS NULL AND device_public_key IS NULL", deviceID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) InspectDeviceClaim(ctx context.Context, claimID, claimSecret string) (DeviceClaim, error) {
	claim, _, err := s.readClaim(ctx, s.db, claimID)
	if errors.Is(err, sql.ErrNoRows) {
		return DeviceClaim{}, ErrClaimInvalid
	}
	if err != nil {
		return DeviceClaim{}, err
	}
	if claim.ClaimSecret != hash(claimSecret) || claim.Status != "pending" || claim.ExpiresAt <= time.Now().UnixMilli() {
		return DeviceClaim{}, ErrClaimInvalid
	}
	claim.ClaimSecret = ""
	return claim, nil
}

func (s *Store) DeviceClaimProgress(ctx context.Context, claimID, claimSecret string) (ClaimProgress, error) {
	claim, _, err := s.readClaim(ctx, s.db, claimID)
	if errors.Is(err, sql.ErrNoRows) {
		return ClaimProgress{}, ErrClaimInvalid
	}
	if err != nil {
		return ClaimProgress{}, err
	}
	if claimSecret == "" || claim.ClaimSecret != hash(claimSecret) {
		return ClaimProgress{}, ErrClaimInvalid
	}
	status := claim.Status
	if claim.DeviceID != nil {
		// Credential delivery can expire after approval; the binding still succeeded.
		status = "approved"
	} else if status == "pending" && claim.ExpiresAt <= time.Now().UnixMilli() {
		status = "expired"
	}
	return ClaimProgress{Status: status, ExpiresAt: claim.ExpiresAt}, nil
}

func (s *Store) ApproveDeviceClaim(ctx context.Context, userID, workspaceID, claimID, claimSecret string, wrapKey []byte) (DeviceClaim, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeviceClaim{}, err
	}
	defer tx.Rollback()
	role, err := membershipRole(ctx, tx, userID, workspaceID)
	if err != nil || role == "viewer" {
		return DeviceClaim{}, ErrForbidden
	}
	claim, wrappedSecretHash, err := s.readClaim(ctx, tx, claimID)
	if errors.Is(err, sql.ErrNoRows) {
		return DeviceClaim{}, ErrClaimInvalid
	}
	if err != nil {
		return DeviceClaim{}, err
	}
	if claim.ExpiresAt <= time.Now().UnixMilli() || wrappedSecretHash != hash(claimSecret) {
		return DeviceClaim{}, ErrClaimInvalid
	}
	if claim.Status != "pending" {
		return DeviceClaim{}, ErrClaimAlreadyUsed
	}
	var existing string
	err = tx.QueryRowContext(ctx, "SELECT id FROM devices WHERE device_public_key=?", claim.DevicePublicKey).Scan(&existing)
	if err == nil {
		return DeviceClaim{}, ErrClaimAlreadyUsed
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return DeviceClaim{}, err
	}
	deviceID := ""
	if claim.LegacyDeviceID != nil {
		deviceID = *claim.LegacyDeviceID
	} else {
		deviceID, err = randomUUID()
		if err != nil {
			return DeviceClaim{}, err
		}
	}
	deviceToken, deviceTokenHash, err := newSecret()
	if err != nil {
		return DeviceClaim{}, err
	}
	wrappedToken, err := wrapSecret(deviceToken, wrapKey)
	if err != nil {
		return DeviceClaim{}, err
	}
	now := time.Now().UnixMilli()
	auditAction := "device.claimed"
	if claim.LegacyDeviceID == nil {
		if _, err := tx.ExecContext(ctx, `INSERT INTO devices(id,owner_id,token_hash,name,account_label,status,last_seen_at,created_at,device_public_key,device_key_fingerprint,claimed_by_user_id) VALUES(?,?,?,?,?,'offline',NULL,?,?,?,?)`, deviceID, workspaceID, deviceTokenHash, claim.DeviceName, claim.AccountLabel, now, claim.DevicePublicKey, claim.DeviceKeyFingerprint, userID); err != nil {
			return DeviceClaim{}, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE commands SET state='failed',completed_at=?,result_json='{"error":"Device migrated to an account workspace"}' WHERE device_id=? AND state IN ('queued','claimed')`, now, deviceID); err != nil {
			return DeviceClaim{}, err
		}
		operation, err := tx.ExecContext(ctx, `UPDATE devices SET owner_id=?,token_hash=?,name=?,account_label=?,status='offline',device_public_key=?,device_key_fingerprint=?,claimed_by_user_id=? WHERE id=? AND revoked_at IS NULL AND claimed_by_user_id IS NULL AND device_public_key IS NULL`, workspaceID, deviceTokenHash, claim.DeviceName, claim.AccountLabel, claim.DevicePublicKey, claim.DeviceKeyFingerprint, userID, deviceID)
		if err != nil {
			return DeviceClaim{}, err
		}
		if changed, _ := operation.RowsAffected(); changed != 1 {
			return DeviceClaim{}, ErrClaimAlreadyUsed
		}
		auditAction = "device.migrated"
	}
	operation, err := tx.ExecContext(ctx, `UPDATE device_claims SET status='approved',workspace_id=?,approved_by_user_id=?,device_id=?,wrapped_device_token=?,approved_at=? WHERE id=? AND status='pending'`, workspaceID, userID, deviceID, wrappedToken, now, claimID)
	if err != nil {
		return DeviceClaim{}, err
	}
	if changed, _ := operation.RowsAffected(); changed != 1 {
		return DeviceClaim{}, ErrClaimAlreadyUsed
	}
	if err := insertAudit(ctx, tx, workspaceID, &userID, auditAction, "device", deviceID, map[string]any{"fingerprint": claim.DeviceKeyFingerprint, "name": claim.DeviceName}); err != nil {
		return DeviceClaim{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeviceClaim{}, err
	}
	claim.Status, claim.DeviceID, claim.WorkspaceID = "approved", &deviceID, &workspaceID
	claim.ClaimSecret = ""
	return claim, nil
}

func (s *Store) PollDeviceClaim(ctx context.Context, claimID, pollToken string, wrapKey []byte) (DeviceClaim, error) {
	claim, _, err := s.readClaim(ctx, s.db, claimID)
	if errors.Is(err, sql.ErrNoRows) {
		return DeviceClaim{}, ErrClaimInvalid
	}
	if err != nil {
		return DeviceClaim{}, err
	}
	if claim.PollToken != hash(pollToken) {
		return DeviceClaim{}, ErrClaimInvalid
	}
	now := time.Now().UnixMilli()
	if claim.ExpiresAt <= now && (claim.Status == "pending" || claim.Status == "approved") {
		_, _ = s.db.ExecContext(ctx, "UPDATE device_claims SET status='expired',wrapped_device_token=NULL WHERE id=? AND status IN ('pending','approved')", claimID)
		claim.Status = "expired"
	}
	if claim.Status == "approved" {
		var wrapped string
		if err := s.db.QueryRowContext(ctx, "SELECT wrapped_device_token FROM device_claims WHERE id=?", claimID).Scan(&wrapped); err != nil {
			return DeviceClaim{}, err
		}
		claim.DeviceToken, err = unwrapSecret(wrapped, wrapKey)
		if err != nil {
			return DeviceClaim{}, err
		}
		_, _ = s.db.ExecContext(ctx, "UPDATE device_claims SET delivered_at=COALESCE(delivered_at,?) WHERE id=?", now, claimID)
	}
	claim.ClaimSecret, claim.PollToken = "", ""
	return claim, nil
}

func (s *Store) RevokeDevice(ctx context.Context, userID, workspaceID, deviceID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	role, err := membershipRole(ctx, tx, userID, workspaceID)
	if err != nil || (role != "owner" && role != "admin") {
		return ErrForbidden
	}
	now := time.Now().UnixMilli()
	operation, err := tx.ExecContext(ctx, "UPDATE devices SET revoked_at=?,status='offline' WHERE id=? AND owner_id=? AND revoked_at IS NULL", now, deviceID, workspaceID)
	if err != nil {
		return err
	}
	if changed, _ := operation.RowsAffected(); changed != 1 {
		return ErrDeviceNotFound
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM tasks WHERE device_id=?", deviceID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM pending_requests WHERE device_id=?", deviceID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE commands SET state='failed',completed_at=?,result_json='{"error":"Device revoked"}' WHERE device_id=? AND state IN ('queued','claimed')`, now, deviceID); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, workspaceID, &userID, "device.revoked", "device", deviceID, nil); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeviceBelongsToWorkspace(ctx context.Context, deviceID, workspaceID string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM devices WHERE id=? AND owner_id=? AND revoked_at IS NULL", deviceID, workspaceID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// CreateWorkspaceCommand authorizes the actor, checks device tenancy, creates
// the command, and records its audit event in one transaction. This prevents a
// membership or device reassignment race from crossing workspace boundaries.
func (s *Store) CreateWorkspaceCommand(ctx context.Context, userID, workspaceID, deviceID string, command protocol.Command, dedupeKey string) (int64, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	role, err := membershipRole(ctx, tx, userID, workspaceID)
	if errors.Is(err, sql.ErrNoRows) || role == "viewer" {
		return 0, false, ErrForbidden
	}
	if err != nil {
		return 0, false, err
	}
	var exists int
	err = tx.QueryRowContext(ctx, "SELECT 1 FROM devices WHERE id=? AND owner_id=? AND revoked_at IS NULL", deviceID, workspaceID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, ErrDeviceNotFound
	}
	if err != nil {
		return 0, false, err
	}
	if dedupeKey == "" {
		dedupeKey, err = randomUUID()
		if err != nil {
			return 0, false, err
		}
	}
	now := time.Now().UnixMilli()
	result, err := tx.ExecContext(ctx, `INSERT INTO commands(device_id,kind,thread_id,body_json,dedupe_key,created_at) VALUES(?,?,?,?,?,?) ON CONFLICT(dedupe_key) DO NOTHING`, deviceID, command.Kind, command.ThreadID, string(command.Body), dedupeKey, now)
	if err != nil {
		return 0, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	created := rows == 1
	var id int64
	if created {
		id, err = result.LastInsertId()
	} else {
		err = tx.QueryRowContext(ctx, "SELECT id FROM commands WHERE dedupe_key=? AND device_id=?", dedupeKey, deviceID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, ErrDedupeConflict
		}
	}
	if err != nil {
		return 0, false, err
	}
	if created {
		if err := insertAudit(ctx, tx, workspaceID, &userID, "command.created", "command", fmt.Sprintf("%d", id), map[string]any{"deviceId": deviceID, "kind": command.Kind, "threadId": command.ThreadID}); err != nil {
			return 0, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return id, created, nil
}

func (s *Store) AuditEvents(ctx context.Context, userID, workspaceID string, limit int) ([]AuditEvent, error) {
	ok, err := s.CanUseWorkspace(ctx, userID, workspaceID, false)
	if err != nil || !ok {
		return nil, ErrForbidden
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,workspace_id,actor_user_id,action,target_type,target_id,metadata_json,created_at FROM audit_events WHERE workspace_id=? ORDER BY created_at DESC,id DESC LIMIT ?`, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []AuditEvent{}
	for rows.Next() {
		var item AuditEvent
		var metadata []byte
		if err := rows.Scan(&item.ID, &item.WorkspaceID, &item.ActorUserID, &item.Action, &item.TargetType, &item.TargetID, &metadata, &item.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(metadata, &item.Metadata); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

type claimReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) readClaim(ctx context.Context, reader claimReader, claimID string) (DeviceClaim, string, error) {
	var claim DeviceClaim
	var claimSecretHash, pollTokenHash string
	err := reader.QueryRowContext(ctx, `SELECT id,claim_secret_hash,poll_token_hash,device_public_key,device_key_fingerprint,device_name,account_label,status,workspace_id,device_id,expires_at,created_at,legacy_device_id FROM device_claims WHERE id=?`, claimID).Scan(
		&claim.ID, &claimSecretHash, &pollTokenHash, &claim.DevicePublicKey, &claim.DeviceKeyFingerprint, &claim.DeviceName, &claim.AccountLabel, &claim.Status, &claim.WorkspaceID, &claim.DeviceID, &claim.ExpiresAt, &claim.CreatedAt, &claim.LegacyDeviceID,
	)
	claim.ClaimSecret, claim.PollToken = claimSecretHash, pollTokenHash
	return claim, claimSecretHash, err
}

func membershipRole(ctx context.Context, tx *sql.Tx, userID, workspaceID string) (string, error) {
	var role string
	err := tx.QueryRowContext(ctx, "SELECT role FROM memberships WHERE user_id=? AND workspace_id=?", userID, workspaceID).Scan(&role)
	return role, err
}

func insertAudit(ctx context.Context, tx *sql.Tx, workspaceID string, actorUserID *string, action, targetType, targetID string, metadata map[string]any) error {
	if metadata == nil {
		metadata = map[string]any{}
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO audit_events(workspace_id,actor_user_id,action,target_type,target_id,metadata_json,created_at) VALUES(?,?,?,?,?,?,?)", workspaceID, actorUserID, action, targetType, targetID, string(encoded), time.Now().UnixMilli())
	return err
}

func encodePassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, passwordIterations, passwordMemoryKiB, passwordParallelism, passwordKeyBytes)
	return fmt.Sprintf("argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", passwordMemoryKiB, passwordIterations, passwordParallelism, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

func verifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "argon2id" || parts[1] != "v=19" {
		return false, errors.New("password hash is malformed")
	}
	var memory, iterations uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[2], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism); err != nil || memory < 8*1024 || memory > 256*1024 || iterations == 0 || iterations > 10 || parallelism == 0 || parallelism > 8 {
		return false, errors.New("password hash parameters are invalid")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false, err
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(expected) < 16 || len(expected) > 64 {
		return false, errors.New("password hash is invalid")
	}
	actual := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1, nil
}

func normalizeEmail(value string) string { return strings.ToLower(strings.TrimSpace(value)) }
func validEmail(value string) bool {
	if len(value) < 3 || len(value) > 320 || strings.ContainsAny(value, " \t\r\n") {
		return false
	}
	parts := strings.Split(value, "@")
	return len(parts) == 2 && parts[0] != "" && strings.Contains(parts[1], ".")
}

func newSecret() (string, string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", "", err
	}
	value := base64.RawURLEncoding.EncodeToString(data)
	return value, hash(value), nil
}

func wrapSecret(value string, key []byte) (string, error) {
	derived := sha256.Sum256(key)
	block, err := aes.NewCipher(derived[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(value), nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func unwrapSecret(value string, key []byte) (string, error) {
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	derived := sha256.Sum256(key)
	block, err := aes.NewCipher(derived[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(data) < gcm.NonceSize() {
		return "", errors.New("wrapped secret is invalid")
	}
	opened, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], nil)
	return string(opened), err
}
