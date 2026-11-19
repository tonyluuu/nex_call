package gpio

import (
	"testing"
	"time"
)

func TestPressTracker(t *testing.T) {
	tr := &pressTracker{long: time.Second}
	if _, ok := tr.edge(false, 0); ok {
		t.Fatal("release without press should be ignored")
	}
	tr.edge(true, 10*time.Millisecond)
	if pr, ok := tr.edge(false, 200*time.Millisecond); !ok || pr != Short {
		t.Fatalf("want short, got %v %v", pr, ok)
	}
	tr.edge(true, time.Second)
	if pr, ok := tr.edge(false, 2500*time.Millisecond); !ok || pr != Long {
		t.Fatalf("want long, got %v %v", pr, ok)
	}
}
