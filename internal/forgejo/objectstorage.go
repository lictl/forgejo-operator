package forgejo

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

// StorageSubsystem is one of Forgejo's storages that spec.objectStorage can move to a bucket.
// The same on Forgejo 15 and 17 (modules/setting: getStorage callers).
type StorageSubsystem struct {
	// Name is Forgejo's storage name, which is also its object key prefix and the directory
	// under APP_DATA_PATH that a local storage defaults to.
	Name string
	// Section is the app.ini section whose STORAGE_TYPE selects the subsystem's storage. A
	// subsystem section without STORAGE_TYPE inherits [storage].
	Section string
	// Field is the field of spec.objectStorage.subsystems.
	Field string
	get   func(*forgejov1.ObjectStorageSubsystems) *bool
}

// Storage names that are also their app.ini sections.
const (
	StorageLFS      = "lfs"
	StoragePackages = "packages"
)

// StorageSubsystems are every storage of Forgejo, in app.ini order of the documentation.
var StorageSubsystems = []StorageSubsystem{
	{Name: StorageLFS, Section: StorageLFS, Field: "lfs", get: func(s *forgejov1.ObjectStorageSubsystems) *bool { return s.LFS }},
	{Name: "attachments", Section: "attachment", Field: "attachments", get: func(s *forgejov1.ObjectStorageSubsystems) *bool { return s.Attachments }},
	{Name: "avatars", Section: "avatar", Field: "avatars", get: func(s *forgejov1.ObjectStorageSubsystems) *bool { return s.Avatars }},
	{Name: "repo-avatars", Section: "repo-avatar", Field: "repoAvatars", get: func(s *forgejov1.ObjectStorageSubsystems) *bool { return s.RepoAvatars }},
	{Name: "repo-archive", Section: "repo-archive", Field: "repoArchives", get: func(s *forgejov1.ObjectStorageSubsystems) *bool { return s.RepoArchives }},
	{Name: StoragePackages, Section: StoragePackages, Field: "packages", get: func(s *forgejov1.ObjectStorageSubsystems) *bool { return s.Packages }},
	{Name: "actions_log", Section: "storage.actions_log", Field: "actionsLogs", get: func(s *forgejov1.ObjectStorageSubsystems) *bool { return s.ActionsLogs }},
	{Name: "actions_artifacts", Section: "actions.artifacts", Field: "actionsArtifacts", get: func(s *forgejov1.ObjectStorageSubsystems) *bool { return s.ActionsArtifacts }},
}

// ObjectStorageEnabled reports whether spec.objectStorage is set.
func ObjectStorageEnabled(f *forgejov1.Forgejo) bool { return f.Spec.ObjectStorage != nil }

// InObjectStorage reports whether subsystem s keeps its files in the bucket.
func InObjectStorage(f *forgejov1.Forgejo, s StorageSubsystem) bool {
	os := f.Spec.ObjectStorage
	return os != nil && BoolOr(s.get(&os.Subsystems), true)
}

// ObjectStorageSubsystemNames are the storage names of the subsystems kept in the bucket
// (nil without spec.objectStorage).
func ObjectStorageSubsystemNames(f *forgejov1.Forgejo) []string {
	var names []string
	for _, s := range StorageSubsystems {
		if InObjectStorage(f, s) {
			names = append(names, s.Name)
		}
	}
	return names
}

// S3Endpoint is a parsed spec S3 endpoint URL.
type S3Endpoint struct {
	// Host is host[:port] as given (MINIO_ENDPOINT).
	Host string
	// Secure is true for https.
	Secure bool
}

// ParseS3Endpoint parses an S3Connection endpoint (scheme://host[:port], no path).
func ParseS3Endpoint(endpoint string) (S3Endpoint, error) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return S3Endpoint{}, fmt.Errorf("endpoint %q: %w", endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return S3Endpoint{}, fmt.Errorf("endpoint %q: the scheme must be http or https", endpoint)
	}
	if u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return S3Endpoint{}, fmt.Errorf("endpoint %q: want scheme://host[:port] without a path", endpoint)
	}
	if p := u.Port(); p != "" {
		if _, err := net.LookupPort("tcp", p); err != nil {
			return S3Endpoint{}, fmt.Errorf("endpoint %q: invalid port", endpoint)
		}
	}
	return S3Endpoint{Host: u.Host, Secure: u.Scheme == "https"}, nil
}

// S3Region is the region requests are signed for.
func S3Region(c *forgejov1.S3Connection) string {
	if c.Region != "" {
		return c.Region
	}
	return "us-east-1"
}

// S3AccessKeyIDKey and S3SecretAccessKeyKey are the keys of the credentials Secret.
func S3AccessKeyIDKey(c *forgejov1.S3Connection) string {
	if k := c.CredentialsSecret.AccessKeyIDKey; k != "" {
		return k
	}
	return "AWS_ACCESS_KEY_ID"
}

func S3SecretAccessKeyKey(c *forgejov1.S3Connection) string {
	if k := c.CredentialsSecret.SecretAccessKeyKey; k != "" {
		return k
	}
	return "AWS_SECRET_ACCESS_KEY"
}

// S3CASecret is the Secret key with the endpoint's CA certificate, or nil.
func S3CASecret(c *forgejov1.S3Connection) *forgejov1.SecretKeyRef {
	if c.TLS == nil || c.TLS.CASecret == nil {
		return nil
	}
	ref := *c.TLS.CASecret
	if ref.Key == "" {
		ref.Key = "ca.crt"
	}
	return &ref
}

// S3InsecureSkipVerify reports whether the endpoint's certificate is not verified.
func S3InsecureSkipVerify(c *forgejov1.S3Connection) bool {
	return c.TLS != nil && c.TLS.InsecureSkipVerify
}

// S3Prefix is the object key prefix with exactly one trailing slash, or "".
func S3Prefix(c *forgejov1.S3Connection) string {
	p := strings.Trim(c.Prefix, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

// S3VirtualHostStyle reports whether requests address the bucket as a host name: pathStyle
// when set, else what minio-go (and with it Forgejo, MINIO_BUCKET_LOOKUP=auto) does: virtual
// hosts for Amazon S3, Google Cloud Storage and Alibaba Cloud OSS endpoints, unless the bucket
// name has a dot and the endpoint uses TLS (its certificate would not match).
func S3VirtualHostStyle(c *forgejov1.S3Connection) bool {
	if c.PathStyle != nil {
		return !*c.PathStyle
	}
	ep, err := ParseS3Endpoint(c.Endpoint)
	if err != nil {
		return false
	}
	if ep.Secure && strings.Contains(c.Bucket, ".") {
		return false
	}
	host := strings.ToLower(hostOnly(ep.Host))
	return host == "s3.amazonaws.com" || strings.HasSuffix(host, ".amazonaws.com") || strings.HasSuffix(host, ".amazonaws.com.cn") ||
		host == "storage.googleapis.com" || strings.HasSuffix(host, ".aliyuncs.com")
}

func hostOnly(hostPort string) string {
	if h, _, err := net.SplitHostPort(hostPort); err == nil {
		return h
	}
	return strings.Trim(hostPort, "[]")
}

// ObjectStorage is the resolved spec.objectStorage of an instance (set by the object-storage
// step).
type ObjectStorage struct {
	// Digest identifies the credentials and the CA certificate (the pod template's
	// object-storage annotation): environment variables and the trust store are read at
	// start, so a change rolls the pods.
	Digest string
}
