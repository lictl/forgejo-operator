package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const (
	testLabelHost   = "host"
	testKeyStatus   = "status"
	testKeyOwnerID  = "owner_id"
	testOrg         = "acme"
	testRepo        = "acme/app"
	testLabelDocker = "docker"
)

// fakeForgejo serves the runner endpoints of Forgejo's API (15 and 17) for user admin:pw. It
// caps pages at maxItems, as [api] MAX_RESPONSE_ITEMS does.
type fakeForgejo struct {
	mu       sync.Mutex
	runners  []APIRunner
	jobs     map[string][]map[string]any // by path
	users    map[string]int64            // user and organization ids by name
	maxItems int
	visible  []string
	deleted  []int64
	// listed counts the list pages served; afterPage runs after each (e.g. to move runners as
	// Forgejo does when they poll).
	listed    int
	afterPage func(f *fakeForgejo, page int)
}

func (f *fakeForgejo) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, p, ok := req.BasicAuth(); !ok || u != "admin" || p != "pw" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case req.Method == http.MethodGet && req.URL.Path == "/api/v1/admin/actions/runners":
		f.visible = append(f.visible, req.URL.Query().Get("visible"))
		page, _ := strconv.Atoi(req.URL.Query().Get("page"))
		limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
		limit = min(limit, f.maxItems)
		start := min((page-1)*limit, len(f.runners))
		end := min(start+limit, len(f.runners))
		w.Header().Set("X-Total-Count", strconv.Itoa(len(f.runners)))
		_ = json.NewEncoder(w).Encode(f.runners[start:end])
		f.listed++
		if f.afterPage != nil {
			f.afterPage(f, page)
		}
	case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/api/v1/admin/actions/runners/") &&
		!strings.HasSuffix(req.URL.Path, "/jobs"):
		var id int64
		if _, err := fmt.Sscanf(req.URL.Path, "/api/v1/admin/actions/runners/%d", &id); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, r := range f.runners {
			if r.ID == id {
				_ = json.NewEncoder(w).Encode(r)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	case req.Method == http.MethodDelete:
		var id int64
		if _, err := fmt.Sscanf(req.URL.Path, "/api/v1/admin/actions/runners/%d", &id); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for i, r := range f.runners {
			if r.ID == id {
				f.runners = append(f.runners[:i], f.runners[i+1:]...)
				f.deleted = append(f.deleted, id)
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/api/v1/users/"):
		id, ok := f.users[strings.TrimPrefix(req.URL.Path, "/api/v1/users/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "login": strings.TrimPrefix(req.URL.Path, "/api/v1/users/")})
	case req.Method == http.MethodGet:
		jobs, ok := f.jobs[req.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if req.URL.Query().Get("labels") != "docker,host" {
			_ = json.NewEncoder(w).Encode([]any{})
			return
		}
		_ = json.NewEncoder(w).Encode(jobs)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func TestAPIListsEveryPage(t *testing.T) {
	f := &fakeForgejo{maxItems: 20}
	for i := 1; i <= 45; i++ {
		f.runners = append(f.runners, APIRunner{ID: int64(i), UUID: fmt.Sprintf("u%d", i), Status: statusIdle})
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	got, err := NewAPI(srv.URL, "admin", "pw").ListRunners(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 45 {
		t.Fatalf("listed %d runners, want 45 (pages capped at 20)", len(got))
	}
	for _, v := range f.visible {
		if v != "true" {
			t.Fatalf("listed without visible=true: organization and repository runners are missed")
		}
	}
}

func TestAPIDelete(t *testing.T) {
	f := &fakeForgejo{maxItems: 50, runners: []APIRunner{{ID: 7, UUID: "u"}}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	api := NewAPI(srv.URL+"/", "admin", "pw")
	if deleted, err := api.DeleteRunner(context.Background(), 7); err != nil || !deleted {
		t.Fatalf("deleted %v, %v", deleted, err)
	}
	if deleted, err := api.DeleteRunner(context.Background(), 7); err != nil || deleted {
		t.Fatalf("deleting a runner that is gone: %v, %v", deleted, err)
	}
	if len(f.deleted) != 1 || f.deleted[0] != 7 {
		t.Fatalf("deleted %v", f.deleted)
	}
}

func TestAPIGetRunner(t *testing.T) {
	f := &fakeForgejo{maxItems: 50, runners: []APIRunner{{ID: 7, UUID: "u", Status: statusIdle}}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	api := NewAPI(srv.URL, "admin", "pw")
	got, err := api.GetRunner(context.Background(), 7)
	if err != nil || got == nil || got.UUID != "u" || !got.Online() {
		t.Fatalf("GetRunner(7) = %+v, %v", got, err)
	}
	if got, err := api.GetRunner(context.Background(), 8); err != nil || got != nil {
		t.Fatalf("GetRunner of a deleted runner = %+v, %v; want nil, nil", got, err)
	}
}

// Forgejo orders runners by last_online, newest first, so a runner that polls while the pages
// are read moves to page 1: the read sees another runner twice and misses it. The list is
// read again until its pages add up, and a list of several pages is read twice and merged.
func TestAPIListSurvivesRunnersMovingBetweenPages(t *testing.T) {
	f := &fakeForgejo{maxItems: 2}
	for i := 1; i <= 5; i++ {
		f.runners = append(f.runners, APIRunner{ID: int64(i), UUID: fmt.Sprintf("u%d", i), Status: statusIdle})
	}
	// After page 1 of the first read, runner 4 (on page 2) polls and moves to the front.
	f.afterPage = func(f *fakeForgejo, page int) {
		if f.listed == 1 {
			r := f.runners[3]
			f.runners = append([]APIRunner{r}, append(f.runners[:3:3], f.runners[4:]...)...)
		}
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	got, err := NewAPI(srv.URL, "admin", "pw").ListRunners(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int64]int{}
	for _, r := range got {
		ids[r.ID]++
	}
	if len(got) != 5 || len(ids) != 5 {
		t.Fatalf("listed %v, want runners 1 to 5 once each", ids)
	}
	if f.listed != 3+3+3 {
		t.Errorf("served %d pages, want 9 (an inconsistent read, then two full reads)", f.listed)
	}

	// Pages that never add up are an error, not a short list.
	f.afterPage = func(f *fakeForgejo, page int) {
		if page == 1 {
			r := f.runners[len(f.runners)-1]
			f.runners = append([]APIRunner{r}, f.runners[:len(f.runners)-1]...)
		}
	}
	if _, err := NewAPI(srv.URL, "admin", "pw").ListRunners(context.Background()); err == nil {
		t.Fatal("a list that keeps changing between pages was taken as complete")
	}
}

func TestAPIUnauthorized(t *testing.T) {
	srv := httptest.NewServer(&fakeForgejo{maxItems: 50})
	defer srv.Close()
	_, err := NewAPI(srv.URL, "admin", "wrong").ListRunners(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
}

func TestAPIWaitingJobsByScope(t *testing.T) {
	waiting := []map[string]any{{testKeyStatus: statusWaiting}, {testKeyStatus: "running"}, {testKeyStatus: statusWaiting}}
	f := &fakeForgejo{maxItems: 50, jobs: map[string][]map[string]any{
		"/api/v1/admin/actions/runners/jobs":            waiting,
		"/api/v1/orgs/acme/actions/runners/jobs":        waiting[:1],
		"/api/v1/repos/acme/app/actions/runners/jobs":   waiting[1:2],
		"/api/v1/repos/acme/other/actions/runners/jobs": nil,
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	api := NewAPI(srv.URL, "admin", "pw")
	for scope, want := range map[string]int{"": 2, testOrg: 1, testRepo: 0} {
		got, err := api.WaitingJobs(context.Background(), scope, []string{testLabelDocker, testLabelHost})
		if err != nil || got != want {
			t.Errorf("scope %q: %d, %v; want %d", scope, got, err, want)
		}
	}
	if _, err := api.WaitingJobs(context.Background(), "nobody", []string{testLabelDocker, testLabelHost}); err == nil {
		t.Error("a 404 counted as no waiting jobs")
	}
}

// A user scope: /orgs/{user}/actions/runners/jobs answers 404 (Forgejo 15 and 17), so the
// instance's waiting jobs are counted for the user's id.
func TestAPIWaitingJobsOfAUser(t *testing.T) {
	f := &fakeForgejo{maxItems: 50, users: map[string]int64{"alice": 2, testOrg: 3}, jobs: map[string][]map[string]any{
		"/api/v1/admin/actions/runners/jobs": {
			{testKeyStatus: statusWaiting, testKeyOwnerID: 2}, {testKeyStatus: "running", testKeyOwnerID: 2},
			{testKeyStatus: statusWaiting, testKeyOwnerID: 2}, {testKeyStatus: statusWaiting, testKeyOwnerID: 3},
		},
		"/api/v1/orgs/acme/actions/runners/jobs": {{testKeyStatus: statusWaiting, testKeyOwnerID: 3}},
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	api := NewAPI(srv.URL, "admin", "pw")
	for scope, want := range map[string]int{"alice": 2, testOrg: 1, "": 3} {
		got, err := api.WaitingJobs(context.Background(), scope, []string{testLabelDocker, testLabelHost})
		if err != nil || got != want {
			t.Errorf("scope %q: %d, %v; want %d", scope, got, err, want)
		}
	}
	_, err := api.WaitingJobs(context.Background(), "nobody", []string{testLabelDocker, testLabelHost})
	if err == nil || !strings.Contains(err.Error(), `owner "nobody" does not exist`) {
		t.Errorf("an unknown owner: %v", err)
	}
}
