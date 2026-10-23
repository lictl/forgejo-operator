package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// APIRunner is a runner as Forgejo's API reports it (structs.ActionRunner, Forgejo 15 and 17).
type APIRunner struct {
	ID      int64  `json:"id"`
	UUID    string `json:"uuid"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	OwnerID int64  `json:"owner_id"`
	RepoID  int64  `json:"repo_id"`
	Version string `json:"version"`
}

// Online reports whether Forgejo heard from the runner within the last minute.
func (r APIRunner) Online() bool { return r.Status == "idle" || r.Status == "active" }

// API is what the operator asks Forgejo about runners.
type API interface {
	// ListRunners lists every runner of the instance. A list of several pages can still miss
	// a runner (see httpAPI.ListRunners): callers ask for a known runner with GetRunner before
	// they take it for gone.
	ListRunners(ctx context.Context) ([]APIRunner, error)
	// GetRunner returns the runner with that id, or nil when Forgejo has none (it was
	// deleted: ids are never reused).
	GetRunner(ctx context.Context, id int64) (*APIRunner, error)
	// DeleteRunner unregisters a runner and reports whether it existed; one that does not
	// exist is not an error.
	DeleteRunner(ctx context.Context, id int64) (bool, error)
	// WaitingJobs counts the jobs of the scope ("" instance, "<org>", "<owner>/<repo>") that
	// wait for a runner with the given label names.
	WaitingJobs(ctx context.Context, scope string, labels []string) (int, error)
}

// ErrUnauthorized: Forgejo refused the operator's admin credentials.
var ErrUnauthorized = errors.New("forgejo refused the admin credentials")

// apiTimeout bounds every request.
const apiTimeout = 10 * time.Second

// pageSize asks for Forgejo's default maximum page ([api] MAX_RESPONSE_ITEMS = 50).
const pageSize = 50

// listAttempts bounds the reads of a runner list whose pages do not add up.
const listAttempts = 3

// NewAPI returns a client of the instance's API at base (no trailing slash) that
// authenticates as the instance's admin with HTTP basic authentication.
func NewAPI(base, username, password string) API {
	return &httpAPI{
		base: strings.TrimSuffix(base, "/"), user: username, pass: password,
		http: &http.Client{
			Timeout: apiTimeout,
			Transport: &http.Transport{
				Proxy:                 nil,
				DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				ResponseHeaderTimeout: apiTimeout,
				MaxIdleConns:          2,
				IdleConnTimeout:       30 * time.Second,
			},
			// The API does not redirect; a redirect (to a login page, a proxy) is an error.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

type httpAPI struct {
	base, user, pass string
	http             *http.Client
}

// do sends a request and decodes a JSON answer into out. It returns the status code and the
// X-Total-Count header of list answers (-1 when absent). A 404 is no error: callers decide.
func (a *httpAPI) do(ctx context.Context, method, path string, out any) (int, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.base+"/api/v1"+path, nil)
	if err != nil {
		return 0, -1, err
	}
	req.SetBasicAuth(a.user, a.pass)
	req.Header.Set("Accept", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return 0, -1, err
	}
	defer func() { _ = resp.Body.Close() }()
	total := -1
	if v, err := strconv.Atoi(resp.Header.Get("X-Total-Count")); err == nil && v >= 0 {
		total = v
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, total, err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return resp.StatusCode, total, fmt.Errorf("%s %s: HTTP %d: %w", method, path, resp.StatusCode, ErrUnauthorized)
	case resp.StatusCode == http.StatusNotFound:
		return resp.StatusCode, total, nil
	case resp.StatusCode >= 300:
		return resp.StatusCode, total, fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, snippet(body))
	}
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return resp.StatusCode, total, fmt.Errorf("%s %s: decoding the answer: %w", method, path, err)
		}
	}
	return resp.StatusCode, total, nil
}

// ListRunners reads every page. Forgejo caps the page size at [api] MAX_RESPONSE_ITEMS, so
// the end is found from X-Total-Count (or an empty page), never from a short page.
//
// Forgejo orders the list by the time it last heard from each runner (last_online, newest
// first; the API has no other order), and every poll of a runner moves it to the front, so
// runners move between pages while they are read: one is read twice and another not at all.
// A read whose distinct runners do not add up to X-Total-Count is read again, and a list of
// more than one page is read twice and merged, so that a runner one read missed is still
// listed. A single page is one query and cannot miss anything.
func (a *httpAPI) ListRunners(ctx context.Context) ([]APIRunner, error) {
	first, pages, err := a.readRunners(ctx)
	if err != nil || pages <= 1 {
		return first, err
	}
	second, _, err := a.readRunners(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[int64]bool, len(second))
	for _, r := range second {
		seen[r.ID] = true
	}
	for _, r := range first {
		if !seen[r.ID] {
			second = append(second, r)
		}
	}
	return second, nil
}

// readRunners reads the list until its pages add up (at most listAttempts times) and returns
// the distinct runners and the number of pages.
func (a *httpAPI) readRunners(ctx context.Context) ([]APIRunner, int, error) {
	var err error
	for range listAttempts {
		list, pages, total, rerr := a.readPages(ctx)
		if rerr != nil {
			return nil, 0, rerr
		}
		if total < 0 || len(list) == total {
			return list, pages, nil
		}
		err = fmt.Errorf("GET /admin/actions/runners: the pages listed %d distinct runners, Forgejo counts %d "+
			"(runners moved between pages while they were read)", len(list), total)
	}
	return nil, 0, err
}

// readPages reads every page once. It returns the distinct runners, the number of pages and
// the last X-Total-Count (-1 when absent).
func (a *httpAPI) readPages(ctx context.Context) ([]APIRunner, int, int, error) {
	var all []APIRunner
	seen := map[int64]bool{}
	rows := 0
	for page := 1; page <= 1000; page++ {
		var batch []APIRunner
		// visible=true: also the runners of organizations, users and repositories (without
		// it Forgejo lists only the instance's own).
		code, total, err := a.do(ctx, http.MethodGet, fmt.Sprintf("/admin/actions/runners?visible=true&page=%d&limit=%d", page, pageSize), &batch)
		if err != nil {
			return nil, 0, 0, err
		}
		if code == http.StatusNotFound {
			return nil, 0, 0, errors.New("GET /admin/actions/runners: HTTP 404 (Forgejo older than 15, or Actions disabled)")
		}
		rows += len(batch)
		for _, r := range batch {
			if !seen[r.ID] {
				seen[r.ID] = true
				all = append(all, r)
			}
		}
		if len(batch) == 0 || (total >= 0 && rows >= total) || (total < 0 && len(batch) < pageSize) {
			return all, page, total, nil
		}
	}
	return nil, 0, 0, errors.New("GET /admin/actions/runners: too many pages")
}

func (a *httpAPI) GetRunner(ctx context.Context, id int64) (*APIRunner, error) {
	var out APIRunner
	code, _, err := a.do(ctx, http.MethodGet, fmt.Sprintf("/admin/actions/runners/%d", id), &out)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound {
		return nil, nil
	}
	return &out, nil
}

func (a *httpAPI) DeleteRunner(ctx context.Context, id int64) (bool, error) {
	code, _, err := a.do(ctx, http.MethodDelete, fmt.Sprintf("/admin/actions/runners/%d", id), nil)
	return err == nil && code != http.StatusNotFound, err
}

func (a *httpAPI) WaitingJobs(ctx context.Context, scope string, labels []string) (int, error) {
	path := "/admin/actions/runners/jobs"
	if owner, repo, ok := strings.Cut(scope, "/"); ok {
		path = "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/actions/runners/jobs"
	} else if scope != "" {
		path = "/orgs/" + url.PathEscape(scope) + "/actions/runners/jobs"
	}
	var jobs []struct {
		Status string `json:"status"`
	}
	code, _, err := a.do(ctx, http.MethodGet, path+"?labels="+url.QueryEscape(strings.Join(labels, ",")), &jobs)
	if err != nil {
		return 0, err
	}
	if code == http.StatusNotFound {
		return 0, fmt.Errorf("GET %s: HTTP 404", path)
	}
	n := 0
	for _, j := range jobs {
		if j.Status == "waiting" {
			n++
		}
	}
	return n, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
