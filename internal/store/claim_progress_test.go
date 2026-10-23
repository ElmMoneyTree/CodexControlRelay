package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestClaimProgressPreservesApprovalAfterCredentialDeliveryExpires(t *testing.T) {
	database, err := Open(":memory:", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	account, err := database.CreateAccount(ctx, "progress@example.test", "Owner", "test correct horse battery staple", "Personal", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := database.CreateDeviceClaim(ctx, "test-public-key", "test-fingerprint", "PC", nil, nil, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	wrapKey := []byte("0123456789abcdef0123456789abcdef")
	_, err = database.ApproveDeviceClaim(ctx, account.User.ID, account.Workspaces[0].ID, claim.ID, claim.ClaimSecret, wrapKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.db.Exec("UPDATE device_claims SET expires_at=? WHERE id=?", time.Now().Add(-time.Second).UnixMilli(), claim.ID); err != nil {
		t.Fatal(err)
	}
	polled, err := database.PollDeviceClaim(ctx, claim.ID, claim.PollToken, wrapKey)
	if err != nil || polled.Status != "expired" || polled.DeviceToken != "" {
		t.Fatalf("expired credential delivery: %#v, %v", polled, err)
	}
	progress, err := database.DeviceClaimProgress(ctx, claim.ID, claim.ClaimSecret)
	if err != nil || progress.Status != "approved" {
		t.Fatalf("successful binding was lost: %#v, %v", progress, err)
	}
	if _, err = database.DeviceClaimProgress(ctx, claim.ID, claim.PollToken); !errors.Is(err, ErrClaimInvalid) {
		t.Fatalf("wrong secret accepted: %v", err)
	}
}
