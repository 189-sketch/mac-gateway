package gateway

import (
	"context"
	"testing"
	"time"
)

// Jobs created in the same millisecond must each appear once across pages.
func TestDashboardPagingKeepsJobsWithTheSameTime(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	secret, err := store.AddAPIKey(ctx, "agent", []string{"*"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.APIKeyBySecret(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	const total = 7
	for i := 0; i < total; i++ {
		if _, _, err := store.CreateJob(ctx, NewJob{APIKeyID: key.ID, Capability: "x", Input: "{}", Selector: "{}", RequestHash: "h", QueueTimeout: time.Minute}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec(`UPDATE jobs SET created_ms = 1000`); err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	filter := dashboardFilter{}
	for pages := 0; pages <= total; pages++ {
		jobs, err := store.dashboardJobs(ctx, filter, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) == 0 {
			break
		}
		for _, job := range jobs {
			if seen[job.ID] {
				t.Errorf("%s appeared twice", job.ID)
			}
			seen[job.ID] = true
		}
		last := jobs[len(jobs)-1]
		filter.beforeMS, filter.beforeID = last.createdMS, last.ID
	}
	if len(seen) != total {
		t.Errorf("saw %d of %d jobs", len(seen), total)
	}
}
