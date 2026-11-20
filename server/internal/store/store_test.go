package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestUnansweredCallKeepsAnsweredAtEmpty(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "calls.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Millisecond)

	if err := st.RoomOpened(ctx, "missed", "web:a", "web:b", now); err != nil {
		t.Fatal(err)
	}
	if err := st.ParticipantJoined(ctx, "missed", "web:a", now); err != nil {
		t.Fatal(err)
	}
	calls, err := st.ListCalls(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].AnsweredAt != nil {
		t.Fatalf("opened %+v", calls[0])
	}

	if err := st.MarkAnswered(ctx, "missed", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkAnswered(ctx, "missed", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	calls, err = st.ListCalls(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if calls[0].AnsweredAt == nil || !calls[0].AnsweredAt.Equal(now.Add(time.Second)) {
		t.Fatalf("mark once %+v", calls[0].AnsweredAt)
	}

	// 旧数据：未接却写了接通时间，重启后清掉。已接通的两人记录保留。
	if _, err := st.db.ExecContext(ctx, `INSERT INTO calls(id, caller, callee, started_at, answered_at) VALUES(?, ?, ?, ?, ?)`,
		"old", "web:a", "web:b", unix(now), unix(now)); err != nil {
		t.Fatal(err)
	}
	if err := st.ParticipantJoined(ctx, "old", "web:a", now); err != nil {
		t.Fatal(err)
	}
	if err := st.RoomOpened(ctx, "talked", "web:a", "device:pi1", now); err != nil {
		t.Fatal(err)
	}
	if err := st.ParticipantJoined(ctx, "talked", "device:pi1", now); err != nil {
		t.Fatal(err)
	}
	if err := st.ParticipantJoined(ctx, "talked", "web:a", now); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkAnswered(ctx, "talked", now); err != nil {
		t.Fatal(err)
	}
	if err := st.RoomOpened(ctx, "gap", "web:a", "device:pi1", now); err != nil {
		t.Fatal(err)
	}
	if err := st.ParticipantJoined(ctx, "gap", "device:pi1", now); err != nil {
		t.Fatal(err)
	}
	if err := st.ParticipantJoined(ctx, "gap", "web:a", now); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	calls, err = st.ListCalls(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*CallRecord{}
	for _, c := range calls {
		got[c.ID] = c
	}
	if got["old"] == nil || got["old"].AnsweredAt != nil {
		t.Fatalf("old row %+v", got["old"])
	}
	if got["missed"] == nil || got["missed"].AnsweredAt != nil {
		t.Fatalf("missed row %+v", got["missed"])
	}
	if got["talked"] == nil || got["talked"].AnsweredAt == nil {
		t.Fatalf("talked row %+v", got["talked"])
	}
	if got["gap"] == nil || got["gap"].AnsweredAt == nil || !got["gap"].AnsweredAt.Equal(got["gap"].StartedAt) {
		t.Fatalf("gap row %+v", got["gap"])
	}
}
