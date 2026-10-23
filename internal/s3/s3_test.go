package s3

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The examples of "Signature Calculations for the Authorization Header" in the Amazon S3 API
// reference (docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-header-based-auth.html).
func TestSignMatchesAWSExamples(t *testing.T) {
	const (
		key    = "AKIAIOSFODNN7EXAMPLE"
		secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	)
	now := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, url, signature, signed string
		header                       map[string]string
	}{
		{"GET Object", "https://examplebucket.s3.amazonaws.com/test.txt",
			"f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
			"host;range;x-amz-content-sha256;x-amz-date", map[string]string{"Range": "bytes=0-9"}},
		{"GET Bucket Lifecycle", "https://examplebucket.s3.amazonaws.com/?lifecycle",
			"fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543",
			"host;x-amz-content-sha256;x-amz-date", nil},
		{"GET Bucket (List Objects)", "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J",
			"34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
			"host;x-amz-content-sha256;x-amz-date", nil},
	} {
		req, err := http.NewRequest(http.MethodGet, tc.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range tc.header {
			req.Header.Set(k, v)
		}
		Sign(req, key, secret, "us-east-1", now)
		want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=" +
			tc.signed + ", Signature=" + tc.signature
		if got := req.Header.Get("Authorization"); got != want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, want)
		}
	}
}

func TestCheckBucket(t *testing.T) {
	var gotPath, gotQuery, gotHost, gotAuth string
	status, body := http.StatusOK, ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotHost, gotAuth = r.URL.Path, r.URL.RawQuery, r.Host, r.Header.Get("Authorization")
		if status == http.StatusMovedPermanently {
			w.Header().Set("X-Amz-Bucket-Region", "eu-west-1")
			w.Header().Set("Location", "https://elsewhere/")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	b := Bucket{Scheme: schemeHTTP, Host: u.Host, Name: "forgejo", Region: "garage", Prefix: "team-a/",
		AccessKeyID: "GKexample", SecretAccessKey: "very-secret"}
	ctx := context.Background()

	r := CheckBucket(ctx, b)
	if !r.OK || r.Reason != ReasonReachable {
		t.Fatalf("200: %+v", r)
	}
	if gotPath != "/forgejo/" || gotQuery != "list-type=2&max-keys=1&prefix=team-a%2F" || gotHost != u.Host ||
		!strings.Contains(gotAuth, "Credential=GKexample/") || !strings.Contains(gotAuth, "/garage/s3/aws4_request") {
		t.Fatalf("request: path %q query %q host %q auth %q", gotPath, gotQuery, gotHost, gotAuth)
	}

	for _, tc := range []struct {
		status int
		body   string
		reason string
		inMsg  string
	}{
		{404, `<?xml version="1.0"?><Error><Code>NoSuchBucket</Code><Message>Bucket not found: forgejo</Message></Error>`, ReasonNotFound, "NoSuchBucket"},
		{403, `<Error><Code>InvalidAccessKeyId</Code><Message>No such key</Message></Error>`, ReasonDenied, "HTTP 403 InvalidAccessKeyId: No such key"},
		{400, `<Error><Code>AuthorizationHeaderMalformed</Code><Message>region 'garage' is wrong</Message></Error>`, ReasonFailed, "AuthorizationHeaderMalformed"},
		{301, ``, ReasonFailed, `the bucket is in region "eu-west-1"`},
		{500, `not xml`, ReasonFailed, "HTTP 500"},
	} {
		status, body = tc.status, tc.body
		r := CheckBucket(ctx, b)
		if r.OK || r.Reason != tc.reason || !strings.Contains(r.Message, tc.inMsg) {
			t.Errorf("%d: %+v", tc.status, r)
		}
		if strings.Contains(r.Message, "very-secret") {
			t.Errorf("%d: message leaks the secret: %s", tc.status, r.Message)
		}
	}

	// Virtual-host style puts the bucket into the host name.
	b.VirtualHost = true
	b.Host = "localhost:" + u.Port()
	b.DialContext = (&dialRedirect{to: u.Host}).dial
	status = 200
	if r := CheckBucket(ctx, b); !r.OK || gotPath != "/" || gotHost != "forgejo.localhost:"+u.Port() {
		t.Fatalf("virtual host: %+v path %q host %q", r, gotPath, gotHost)
	}

	// Nothing listens: unreachable, quickly.
	srv.Close()
	b.VirtualHost, b.DialContext, b.Host = false, nil, u.Host
	if r := CheckBucket(ctx, b); r.OK || r.Reason != ReasonUnreachable {
		t.Fatalf("closed: %+v", r)
	}
}

type dialRedirect struct{ to string }

func (d *dialRedirect) dial(ctx context.Context, network, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, d.to)
}

// A certificate of an unknown CA is a TLS error, and trusting the CA fixes it.
func TestCheckBucketTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	b := Bucket{Scheme: "https", Host: u.Host, Name: "forgejo", Region: "us-east-1", AccessKeyID: "k", SecretAccessKey: "s"}
	if r := CheckBucket(context.Background(), b); r.OK || r.Reason != ReasonTLS || !strings.Contains(r.Message, "certificate") {
		t.Fatalf("unknown CA: %+v", r)
	}
	b.RootCAs = x509.NewCertPool()
	b.RootCAs.AddCert(srv.Certificate())
	if r := CheckBucket(context.Background(), b); !r.OK {
		t.Fatalf("trusted CA: %+v", r)
	}
	// Plain HTTP spoken to with TLS.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer plain.Close()
	pu, _ := url.Parse(plain.URL)
	b.Host = pu.Host
	if r := CheckBucket(context.Background(), b); r.OK || r.Reason != ReasonTLS {
		t.Fatalf("plain HTTP: %+v", r)
	}
}

// fakeStore is a minimal S3 server for one bucket, path style, that checks every request is
// signed with the expected key.
type fakeStore struct {
	t       *testing.T
	objects map[string][]byte
	pages   int
}

func (f *fakeStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(r.Header.Get("Authorization"), "Credential=GKexample/") {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<Error><Code>AccessDenied</Code></Error>`))
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/backups/")
	switch {
	case r.Method == http.MethodGet && key == "":
		f.pages++
		prefix := r.URL.Query().Get("prefix")
		var keys []string
		for k := range f.objects {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		start := 0
		if tok := r.URL.Query().Get("continuation-token"); tok != "" {
			start, _ = strconv.Atoi(tok)
		}
		const page = 2 // small pages exercise the continuation token
		end := min(start+page, len(keys))
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
		for _, k := range keys[start:end] {
			fmt.Fprintf(&b, `<Contents><Key>%s</Key><LastModified>2026-10-22T03:00:00.000Z</LastModified><Size>%d</Size></Contents>`, k, len(f.objects[k]))
		}
		if end < len(keys) {
			fmt.Fprintf(&b, `<IsTruncated>true</IsTruncated><NextContinuationToken>%d</NextContinuationToken>`, end)
		} else {
			b.WriteString(`<IsTruncated>false</IsTruncated>`)
		}
		b.WriteString(`</ListBucketResult>`)
		_, _ = w.Write([]byte(b.String()))
	case r.Method == http.MethodGet:
		data, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code><Message>Key not found</Message></Error>`))
			return
		}
		_, _ = w.Write(data)
	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func TestObjects(t *testing.T) {
	store := &fakeStore{t: t, objects: map[string][]byte{
		"team/ns/fj/b1/db.dump.bin":       []byte("dump"),
		"team/ns/fj/b1/manifest.json.bin": []byte("manifest"),
		"team/ns/fj/b2/db.dump.bin":       []byte("dump2"),
		"team/ns/other/x.bin":             []byte("x"),
	}}
	srv := httptest.NewServer(store)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	b := Bucket{Scheme: schemeHTTP, Host: u.Host, Name: "backups", Region: "garage", AccessKeyID: "GKexample", SecretAccessKey: "very-secret"}
	ctx := context.Background()

	objs, err := ListObjects(ctx, b, "team/ns/fj/", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 3 || objs[0].Key != "team/ns/fj/b1/db.dump.bin" || objs[2].Size != 5 || objs[0].LastModified.IsZero() {
		t.Fatalf("listing: %+v", objs)
	}
	if store.pages != 2 {
		t.Errorf("listed %d pages, want 2 (continuation)", store.pages)
	}
	if _, err := ListObjects(ctx, b, "team/", 2); err == nil {
		t.Error("a listing beyond the limit did not fail")
	}

	data, err := GetObject(ctx, b, "team/ns/fj/b1/manifest.json.bin", 1<<20)
	if err != nil || string(data) != "manifest" {
		t.Fatalf("get: %q %v", data, err)
	}
	if _, err := GetObject(ctx, b, "team/ns/fj/b1/manifest.json.bin", 3); err == nil {
		t.Error("an object beyond the limit was read")
	}
	if _, err := GetObject(ctx, b, "team/missing", 1<<20); !IsNotFound(err) || !strings.Contains(err.Error(), "NoSuchKey") {
		t.Errorf("missing object: %v", err)
	}

	if err := DeleteObject(ctx, b, "team/ns/fj/b2/db.dump.bin"); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.objects["team/ns/fj/b2/db.dump.bin"]; ok {
		t.Error("object not deleted")
	}

	bad := b
	bad.AccessKeyID = "other"
	if _, err := GetObject(ctx, bad, "team/ns/fj/b1/db.dump.bin", 100); ReasonOf(err) != ReasonDenied || strings.Contains(err.Error(), "very-secret") {
		t.Errorf("denied: %v", err)
	}
	srv.Close()
	if _, err := ListObjects(ctx, b, "", 10); !IsUnreachable(err) {
		t.Errorf("closed server: %v", err)
	}
}

func TestHTTPClientBoundsAStalledEndpoint(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	old := responseHeaderTimeout
	responseHeaderTimeout = 100 * time.Millisecond
	defer func() { responseHeaderTimeout = old }()

	b := Bucket{Scheme: schemeHTTP}
	start := time.Now()
	resp, err := b.httpClient().Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("want a timeout from an endpoint that never answers")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("request took %s, want it bounded by the response header timeout", elapsed)
	}
}
