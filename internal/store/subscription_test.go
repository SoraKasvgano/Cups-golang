package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestCreateSubscriptionPullMethodDefaults(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})

	err = st.WithTx(ctx, false, func(tx *sql.Tx) error {
		sub, err := st.CreateSubscription(ctx, tx, nil, nil, "job-completed", 60, "alice", "mailto:alice@example.com", "", 0, nil)
		if err != nil {
			return err
		}
		if sub.PullMethod != "" {
			t.Fatalf("pullMethod with recipient = %q, want empty", sub.PullMethod)
		}
		sub2, err := st.CreateSubscription(ctx, tx, nil, nil, "job-completed", 60, "alice", "", "", 0, nil)
		if err != nil {
			return err
		}
		if sub2.PullMethod != "ippget" {
			t.Fatalf("pullMethod without recipient = %q, want ippget", sub2.PullMethod)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("transaction: %v", err)
	}
}
