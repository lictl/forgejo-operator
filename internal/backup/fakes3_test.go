package backup

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/rclone"
)

const (
	testCredsSecret    = "creds"
	testKeyField       = "key"
	testSecretKeyField = "secret-key"
	testPasswordField  = "password"
	testHostKeyFile    = "ssh_host_ed25519_key"
)

// Exported test helpers (fake S3 store, Secrets, remote backups) shared with the backup_test
// package of this directory.

// FakeBucket and FakeKey are the bucket and the backup key of a FakeS3.
const (
	FakeBucket = "backups"
	FakeKey    = "Zm9yZ2Vqby1vcGVyYXRvci10ZXN0LWtleS0xMjM0NTY3OA=="
)

// FakeS3 is a path-style S3 server holding one bucket.
type FakeS3 struct {
	Server *httptest.Server
	mu     sync.Mutex
	// Objects maps keys to contents; Modified to their modification times.
	Objects  map[string][]byte
	Modified map[string]time.Time
	// Deleted records deleted keys in order.
	Deleted []string
	// Down makes every request fail with 503.
	Down bool
	// Requests counts the requests served.
	Requests int
}

// NewFakeS3 starts a FakeS3; it is closed with the test.
func NewFakeS3(t *testing.T) *FakeS3 {
	f := &FakeS3{Objects: map[string][]byte{}, Modified: map[string]time.Time{}}
	f.Server = httptest.NewServer(f)
	t.Cleanup(f.Server.Close)
	return f
}

// RequestCount is the number of requests served so far.
func (f *FakeS3) RequestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Requests
}

// Endpoint is the server's URL.
func (f *FakeS3) Endpoint() string { return f.Server.URL }

// Put stores an object modified at.
func (f *FakeS3) Put(key string, data []byte, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Objects[key] = data
	f.Modified[key] = at
}

// Keys lists the stored keys below prefix, sorted.
func (f *FakeS3) Keys(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for k := range f.Objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

func (f *FakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests++
	if f.Down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if !strings.Contains(r.Header.Get("Authorization"), "Credential=GKtest/") {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<Error><Code>InvalidAccessKeyId</Code></Error>`))
		return
	}
	key, ok := strings.CutPrefix(r.URL.Path, "/"+FakeBucket+"/")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<Error><Code>NoSuchBucket</Code></Error>`))
		return
	}
	switch {
	case r.Method == http.MethodGet && key == "":
		prefix := r.URL.Query().Get("prefix")
		var keys []string
		for k := range f.Objects {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		var b strings.Builder
		b.WriteString(`<ListBucketResult><IsTruncated>false</IsTruncated>`)
		for _, k := range keys {
			fmt.Fprintf(&b, `<Contents><Key>%s</Key><Size>%d</Size><LastModified>%s</LastModified></Contents>`,
				k, len(f.Objects[k]), f.Modified[k].UTC().Format(time.RFC3339))
		}
		b.WriteString(`</ListBucketResult>`)
		_, _ = w.Write([]byte(b.String()))
	case r.Method == http.MethodGet:
		data, ok := f.Objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code></Error>`))
			return
		}
		_, _ = w.Write(data)
	case r.Method == http.MethodDelete:
		delete(f.Objects, key)
		f.Deleted = append(f.Deleted, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// FakeConnection is an S3Connection to the FakeS3 with Secret "creds".
func (f *FakeS3) FakeConnection(prefix string) forgejov1.S3Connection {
	return forgejov1.S3Connection{Endpoint: f.Endpoint(), Bucket: FakeBucket, Region: "garage", PathStyle: ptrTrue(),
		Prefix: prefix, CredentialsSecret: forgejov1.S3CredentialsSecretRef{Name: testCredsSecret}}
}

func ptrTrue() *bool { t := true; return &t }

// FakeSecrets are the credentials Secret "creds" and the backup key Secret keyName (key "key")
// in namespace.
func FakeSecrets(namespace, keyName string) []*corev1.Secret {
	return []*corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Name: testCredsSecret, Namespace: namespace},
			Data: map[string][]byte{envAWSAccessKeyID: []byte("GKtest"), envAWSSecretAccessKey: []byte("test-secret")}},
		{ObjectMeta: metav1.ObjectMeta{Name: keyName, Namespace: namespace},
			Data: map[string][]byte{testKeyField: []byte(FakeKey)}},
	}
}

// PutBackup uploads a remote backup to prefix+path as the upload Job does: every file
// encrypted with FakeKey, the manifest (built from m, with the checksums of files) last.
func (f *FakeS3) PutBackup(t *testing.T, prefix, path string, m RemoteManifest, files map[string][]byte, at time.Time) {
	t.Helper()
	c, err := rclone.NewCipher(FakeKey)
	if err != nil {
		t.Fatal(err)
	}
	put := func(name string, data []byte) {
		enc, err := c.Encrypt(data, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		f.Put(prefix+path+"/"+name+".bin", enc, at)
	}
	m.FormatVersion, m.Kind = remoteFormatVersion, remoteManifestKind
	m.KeyFingerprint = c.Fingerprint()
	names := slices.Sorted(func(yield func(string) bool) {
		for n := range files {
			if !yield(n) {
				return
			}
		}
	})
	for _, name := range names {
		sum := sha256.Sum256(files[name])
		m.Files = append(m.Files, forgejov1.RemoteBackupFile{Name: name, SizeBytes: int64(len(files[name])), SHA256: hex.EncodeToString(sum[:])})
		put(name, files[name])
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	put(RemoteManifestFile, data)
}

// FakeIdentity is an identity bundle of instance "old" with the given SECRET_KEY.
func FakeIdentity(t *testing.T, secretKey string) []byte {
	t.Helper()
	id := Identity{FormatVersion: remoteFormatVersion, Secrets: []IdentitySecret{
		{Role: IdentityRoleSecrets, Name: "old-secrets", Data: map[string][]byte{testSecretKeyField: []byte(secretKey), "internal-token": []byte("tok")}},
		{Role: IdentityRoleAdmin, Name: "old-admin", Data: map[string][]byte{"username": []byte("forgejo_admin"), testPasswordField: []byte("pw")}},
		{Role: IdentityRoleSSHHostKeys, Name: "old-ssh-host-keys", Annotations: map[string]string{"forgejo.lucas.icu/ssh-host-keys-source": keySourceGenerated},
			Data: map[string][]byte{testHostKeyFile: []byte("ed25519 key")}},
	}}
	data, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
