// Package s3 is the operator's minimal S3 client: it checks that a bucket is reachable with
// a set of credentials (ListObjectsV2 of one key, signed with AWS Signature Version 4). It has
// no dependency on an S3 SDK, and never puts credentials into errors or messages.
package s3

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const schemeHTTP = "http"

// emptyPayloadHash is the SHA-256 of an empty body.
const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Bucket is what CheckBucket needs to reach a bucket.
type Bucket struct {
	// Scheme is "http" or "https"; Host is host[:port].
	Scheme string
	Host   string
	// Name of the bucket.
	Name string
	// Region requests are signed for.
	Region string
	// VirtualHost addresses the bucket as "<name>.<host>" instead of in the path.
	VirtualHost bool
	// Prefix limits the listing (e.g. "forgejo/"); it is only sent, never required to exist.
	Prefix string

	AccessKeyID     string
	SecretAccessKey string

	// RootCAs are trusted for https; nil means the system roots.
	RootCAs            *x509.CertPool
	InsecureSkipVerify bool
	// DialContext replaces the transport's dialer (e.g. to resolve names from another
	// namespace); nil dials directly.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Reasons of a Result.
const (
	ReasonReachable   = "BucketReachable"
	ReasonNotFound    = "BucketNotFound"
	ReasonDenied      = "AccessDenied"
	ReasonFailed      = "RequestFailed"
	ReasonUnreachable = "Unreachable"
	// ReasonTLS: the TLS handshake failed, e.g. a certificate signed by an unknown CA.
	ReasonTLS = "TLSError"
)

// Result of CheckBucket.
type Result struct {
	// OK is true when the bucket answered the listing with 200.
	OK bool
	// Reason is one of the Reason constants.
	Reason string
	// Message explains the outcome; it never contains credentials.
	Message string
}

// CheckBucket lists at most one key of the bucket. Forgejo needs the same permission
// (s3:ListBucket) at start, when it checks that the bucket exists.
func CheckBucket(ctx context.Context, b Bucket) Result {
	q := url.Values{"list-type": {"2"}, "max-keys": {"1"}}
	if b.Prefix != "" {
		q.Set("prefix", b.Prefix)
	}
	_, err := b.send(ctx, http.MethodGet, "", q, 64<<10)
	if err == nil {
		return Result{OK: true, Reason: ReasonReachable, Message: fmt.Sprintf("Bucket %q is reachable", b.Name)}
	}
	var e *Error
	if !errors.As(err, &e) {
		return Result{Reason: ReasonFailed, Message: err.Error()}
	}
	if e.Status == 0 {
		return Result{Reason: e.Reason, Message: e.Message}
	}
	switch e.Reason {
	case ReasonNotFound:
		return Result{Reason: ReasonNotFound, Message: fmt.Sprintf("Bucket %q does not exist (%s)", b.Name, e.detail())}
	case ReasonDenied:
		return Result{Reason: ReasonDenied, Message: fmt.Sprintf("Listing bucket %q was refused (%s)", b.Name, e.detail())}
	default:
		return Result{Reason: ReasonFailed, Message: fmt.Sprintf("Listing bucket %q failed (%s)", b.Name, e.detail())}
	}
}

// Error is a failed request: an S3 error response (Status > 0) or a transport failure. It never
// contains credentials.
type Error struct {
	// Reason is one of the Reason constants.
	Reason string
	// Status is the HTTP status, 0 when no response arrived.
	Status int
	// Code and Message come from the S3 error body.
	Code, Message string
	// Region is the bucket's region when the server named another one.
	Region string
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return e.Message
	}
	return e.detail()
}

func (e *Error) detail() string {
	d := fmt.Sprintf("HTTP %d", e.Status)
	if e.Code != "" {
		d += " " + e.Code
	}
	if e.Message != "" {
		d += ": " + e.Message
	}
	if e.Region != "" {
		d += fmt.Sprintf(" (the bucket is in region %q)", e.Region)
	}
	return d
}

// ReasonOf is the Reason of err (ReasonFailed for errors of other kinds).
func ReasonOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return ReasonFailed
}

// IsNotFound reports whether err is a 404 answer (no such key or bucket).
func IsNotFound(err error) bool { return ReasonOf(err) == ReasonNotFound }

// IsUnreachable reports whether err is a transport failure that may go away by itself (DNS,
// connection, timeout); TLS errors are configuration errors and do not count.
func IsUnreachable(err error) bool { return ReasonOf(err) == ReasonUnreachable }

// The operator only reads small objects (manifests, identity files) and probes buckets
// itself; backup data moves through rclone Jobs. These bounds keep a stalled endpoint from
// holding a reconcile worker even when the caller's context has no deadline.
var (
	responseHeaderTimeout = 10 * time.Second
	requestTimeout        = 30 * time.Second
)

// httpClient talks to the endpoint as configured in b.
func (b Bucket) httpClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSClientConfig:       &tls.Config{RootCAs: b.RootCAs, InsecureSkipVerify: b.InsecureSkipVerify, MinVersion: tls.VersionTLS12}, //nolint:gosec // opt-in for tests
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
		DisableKeepAlives:     true,
	}
	if b.DialContext != nil {
		transport.DialContext = b.DialContext
	}
	return &http.Client{
		Transport: transport,
		Timeout:   requestTimeout,
		// A redirect (e.g. Amazon S3's PermanentRedirect to the bucket's region) is the answer.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// request builds a request for key (the bucket itself when ""), path style ("/<bucket>/<key>")
// or virtual-host style ("<bucket>.<host>/<key>"), as minio-go builds it. The path is sent
// encoded exactly as Sign encodes it.
func (b Bucket) request(ctx context.Context, method, key string, q url.Values) (*http.Request, error) {
	if b.Scheme != schemeHTTP && b.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q", b.Scheme)
	}
	host, path := b.Host, "/"+b.Name+"/"+key
	if b.VirtualHost {
		host, path = b.Name+"."+b.Host, "/"+key
	}
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s)
	}
	u := &url.URL{Scheme: b.Scheme, Host: host, Path: path, RawPath: strings.Join(segs, "/"), RawQuery: canonicalQuery(q)}
	return http.NewRequestWithContext(ctx, method, u.String(), nil)
}

// send signs and sends a request without a body and returns the response body, which must not
// be larger than limit bytes. An answer other than 2xx is an *Error, and so is a transport
// failure.
func (b Bucket) send(ctx context.Context, method, key string, q url.Values, limit int64) ([]byte, error) {
	req, err := b.request(ctx, method, key, q)
	if err != nil {
		return nil, &Error{Reason: ReasonFailed, Message: err.Error()}
	}
	Sign(req, b.AccessKeyID, b.SecretAccessKey, b.Region, time.Now())
	client := b.httpClient()
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		reason := ReasonUnreachable
		var verr *tls.CertificateVerificationError
		var rerr tls.RecordHeaderError
		// net/http reports a plain HTTP answer to a TLS client only as text.
		if errors.As(err, &verr) || errors.As(err, &rerr) || strings.Contains(err.Error(), "server gave HTTP response to HTTPS client") {
			reason = ReasonTLS
		}
		return nil, &Error{Reason: reason, Message: fmt.Sprintf("%s: %v", req.URL.Host, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err != nil {
			return nil, &Error{Reason: ReasonUnreachable, Message: fmt.Sprintf("%s: reading the answer: %v", req.URL.Host, err)}
		}
		if int64(len(body)) > limit {
			return nil, &Error{Reason: ReasonFailed, Message: fmt.Sprintf("%s: the answer is larger than %d bytes", req.URL.Host, limit)}
		}
		return body, nil
	}
	e := &Error{Status: resp.StatusCode}
	e.Code, e.Message = errorDetails(body)
	if r := resp.Header.Get("X-Amz-Bucket-Region"); r != "" && r != b.Region {
		e.Region = r
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		e.Reason = ReasonNotFound
	case http.StatusForbidden, http.StatusUnauthorized:
		e.Reason = ReasonDenied
	default:
		e.Reason = ReasonFailed
	}
	return nil, e
}

// Sign adds AWS Signature Version 4 headers for an empty body to req: x-amz-date,
// x-amz-content-sha256 and Authorization, over the headers host, x-amz-content-sha256,
// x-amz-date and any other header already set on req.
func Sign(req *http.Request, accessKeyID, secretAccessKey, region string, now time.Time) {
	const service = "s3"
	t := now.UTC()
	amzDate := t.Format("20060102T150405Z")
	day := t.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", emptyPayloadHash)

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	headers := map[string]string{"host": host}
	for k, v := range req.Header {
		headers[strings.ToLower(k)] = strings.Join(trimAll(v), ",")
	}
	names := slices.Sorted(maps.Keys(headers))
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")

	canonical := strings.Join([]string{
		req.Method,
		canonicalPath(req.URL),
		canonicalQuery(req.URL.Query()),
		canonHeaders.String(),
		signed,
		emptyPayloadHash,
	}, "\n")
	scope := day + "/" + region + "/" + service + "/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])

	key := hmacSHA256([]byte("AWS4"+secretAccessKey), day)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKeyID, scope, signed, signature))
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func trimAll(vs []string) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = strings.Join(strings.Fields(v), " ")
	}
	return out
}

// canonicalPath URI-encodes each segment of the path (S3 does not normalize it).
func canonicalPath(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" {
		return "/"
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if dec, err := url.PathUnescape(s); err == nil {
			s = dec
		}
		segs[i] = uriEncode(s)
	}
	return strings.Join(segs, "/")
}

// canonicalQuery sorts the parameters by name and value and URI-encodes them.
func canonicalQuery(q url.Values) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(q)) {
		for _, v := range slices.Sorted(slices.Values(q[k])) {
			parts = append(parts, uriEncode(k)+"="+uriEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode encodes everything except the unreserved characters A-Z a-z 0-9 - _ . ~.
func uriEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// errorDetails extracts Code and Message from an S3 XML error body.
func errorDetails(body []byte) (code, message string) {
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(body, &e) != nil {
		return "", ""
	}
	const maxLen = 256
	if len(e.Message) > maxLen {
		e.Message = e.Message[:maxLen] + "..."
	}
	return strings.TrimSpace(e.Code), strings.TrimSpace(e.Message)
}
