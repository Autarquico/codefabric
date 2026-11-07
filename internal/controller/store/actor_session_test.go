package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/Autarquico/codefabric/internal/domain"
)

func TestMigration0214_KeepsTheRowsThatExistedWithNoSessionAndStoresNewOnes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	seedUntil(t, path, 213)
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO events (id, schema_version, ts, received_at, source, type, level, actor_kind, actor_id, message)
		 VALUES ('event_old', 1, 1, 1, 'controller', 'TaskCreated', 'info', 'human', 'client_1', 'm')`,
		`INSERT INTO audit_entries (ts, actor_kind, actor_id, action, resource_kind, resource_id, result)
		 VALUES (1, 'human', 'client_1', 'task.created', 'task', 'task_1', 'ok')`,
	} {
		if _, err := old.ExecContext(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(t.Context(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	mcp := domain.ActorRef{Kind: domain.ActorMCP, ID: "client_1", Session: "session-1234"}
	if err := db.WithTx(ctx, func(tx *Tx) error {
		if _, _, err := tx.AppendEvent(ctx, domain.Event{Type: domain.EventTaskCreated, Level: "info", Message: "m", Actor: mcp}); err != nil {
			return err
		}
		tx.SetActorSession(mcp.Session)
		return tx.AppendAudit(ctx, AuditEntry{ActorKind: mcp.Kind, ActorID: mcp.ID, Action: "task.cancelled", ResourceKind: "task", ResourceID: "task_1", Result: "ok"})
	}); err != nil {
		t.Fatal(err)
	}

	events, err := db.Events(ctx, 0, 10, EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Actor.Session != "" || events[1].Actor != mcp {
		t.Errorf("event actors = %+v", events)
	}
	var sessions []sql.NullString
	err = db.Read(ctx, func(q Querier) error {
		rows, err := q.Query(ctx, `SELECT actor_session FROM audit_entries ORDER BY id`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var s sql.NullString
			if err := rows.Scan(&s); err != nil {
				return err
			}
			sessions = append(sessions, s)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0].Valid || sessions[1].String != mcp.Session {
		t.Errorf("audit sessions = %+v", sessions)
	}
}
