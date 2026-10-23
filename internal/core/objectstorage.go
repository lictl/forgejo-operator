package core

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
	"github.com/lictl/forgejo-operator/internal/s3"
)

const (
	// Environment variables that hand the access key to Forgejo. Forgejo (minio-go) reads them
	// when [storage] MINIO_ACCESS_KEY_ID is empty, which is how the operator renders app.ini.
	envS3AccessKeyID     = "AWS_ACCESS_KEY_ID"
	envS3SecretAccessKey = "AWS_SECRET_ACCESS_KEY"
	// objectStorageCAVolume holds the endpoint's CA certificate (spec...tls.caSecret).
	objectStorageCAVolume = "object-storage-ca"
	objectStorageCAFile   = "ca.crt"

	reasonCredentialsUnavailable = "CredentialsUnavailable"
	reasonObjectStorageInvalid   = "InvalidSpec"

	// objectStorageCheckInterval is how often the operator re-checks a reachable bucket;
	// objectStorageRetryInterval how often one that failed. A changed spec or Secret is
	// checked on the next pass.
	objectStorageCheckInterval = time.Minute
	objectStorageRetryInterval = 30 * time.Second
	objectStorageCheckTimeout  = 5 * time.Second
)

// objectStorageCheck is the last bucket check of an instance.
type objectStorageCheck struct {
	inputs string
	at     time.Time
	result s3.Result
}

// objectStorageChecks maps types.NamespacedName to *objectStorageCheck.
var objectStorageChecks sync.Map

// checkBucket is replaced in tests.
var checkBucket = s3.CheckBucket

// ReconcileObjectStorage resolves spec.objectStorage: it reads the credentials Secret (and the
// CA Secret), sets rc.ObjectStorage, and checks from the operator's pod that the bucket is
// reachable with them (ObjectStorageReady). Unusable Secrets stop the steps that need it
// (upgrade, config, workload): a pod could not start without them. An answer that also stops
// Forgejo (refused access, no bucket, a failed request or TLS handshake) sets
// rc.ObjectStorageUnavailable, which holds what replaces pods. A bucket the operator cannot
// reach at all is only reported: Forgejo pods may reach what the operator cannot (a
// NetworkPolicy, an operator outside the cluster, a VPC endpoint only nodes use).
func ReconcileObjectStorage(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	key := types.NamespacedName{Namespace: f.Namespace, Name: f.Name}
	spec := f.Spec.ObjectStorage
	if spec == nil {
		meta.RemoveStatusCondition(&f.Status.Conditions, forgejov1.ConditionObjectStorageReady)
		objectStorageChecks.Delete(key)
		return forgejo.StepResult{}, nil
	}
	c := &spec.S3

	notReady := func(reason, msg string) (forgejo.StepResult, error) {
		rc.ObjectStorageUnavailable = true
		rc.SetCondition(forgejov1.ConditionObjectStorageReady, metav1.ConditionFalse, reason, msg)
		warnEvery(rc, "object-storage/"+reason, "ObjectStorageUnavailable", "ObjectStorage", "%s", msg)
		return forgejo.StepResult{Stop: true, RequeueAfter: objectStorageRetryInterval}, nil
	}
	ep, err := forgejo.ParseS3Endpoint(c.Endpoint)
	if err != nil {
		return notReady(reasonObjectStorageInvalid, err.Error())
	}
	accessKey, err := secretKey(ctx, rc, c.CredentialsSecret.Name, forgejo.S3AccessKeyIDKey(c))
	if err != nil {
		return notReady(reasonCredentialsUnavailable, err.Error())
	}
	secretAccessKey, err := secretKey(ctx, rc, c.CredentialsSecret.Name, forgejo.S3SecretAccessKeyKey(c))
	if err != nil {
		return notReady(reasonCredentialsUnavailable, err.Error())
	}
	var caPEM []byte
	var roots *x509.CertPool
	if ref := forgejo.S3CASecret(c); ref != nil {
		ca, err := secretKey(ctx, rc, ref.Name, ref.Key)
		if err != nil {
			return notReady(reasonCredentialsUnavailable, "CA certificate: "+err.Error())
		}
		caPEM = []byte(ca)
		if roots, err = x509.SystemCertPool(); err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(caPEM) {
			return notReady(reasonCredentialsUnavailable, fmt.Sprintf("CA certificate: key %q of Secret %q holds no PEM certificate", ref.Key, ref.Name))
		}
	}
	rc.ObjectStorage = &forgejo.ObjectStorage{ContentDigest: contentDigest(accessKey, secretAccessKey, string(caPEM))}

	inputs := digest(c.Endpoint, c.Bucket, forgejo.S3Region(c), forgejo.S3Prefix(c), strconv.FormatBool(forgejo.S3VirtualHostStyle(c)),
		strconv.FormatBool(forgejo.S3InsecureSkipVerify(c)), rc.ObjectStorage.ContentDigest)
	now := time.Now()
	check, _ := objectStorageChecks.Load(key)
	last, _ := check.(*objectStorageCheck)
	if last == nil || last.inputs != inputs || now.Sub(last.at) >= checkInterval(last.result) {
		cctx, cancel := context.WithTimeout(ctx, objectStorageCheckTimeout)
		res := checkBucket(cctx, s3.Bucket{
			Scheme: map[bool]string{true: schemeHTTPS, false: schemeHTTP}[ep.Secure], Host: ep.Host,
			Name: c.Bucket, Region: forgejo.S3Region(c), VirtualHost: forgejo.S3VirtualHostStyle(c), Prefix: forgejo.S3Prefix(c),
			AccessKeyID: accessKey, SecretAccessKey: secretAccessKey,
			RootCAs: roots, InsecureSkipVerify: forgejo.S3InsecureSkipVerify(c),
			DialContext: forgejo.NamespaceDialer(f.Namespace),
		})
		cancel()
		last = &objectStorageCheck{inputs: inputs, at: now, result: res}
		objectStorageChecks.Store(key, last)
	}
	rc.ObjectStorageUnavailable = holdsReplacements(last.result)
	reportObjectStorage(rc, c, last.result)
	rc.Requeue(checkInterval(last.result) - now.Sub(last.at))
	return forgejo.StepResult{}, nil
}

// holdsReplacements reports whether a bucket check means that a new Forgejo pod would not start
// either: every failed answer of the endpoint, but not an endpoint the operator cannot reach
// (UnreachableFromOperator), which says nothing about the pods.
func holdsReplacements(r s3.Result) bool {
	return !r.OK && r.Reason != s3.ReasonUnreachable
}

func checkInterval(r s3.Result) time.Duration {
	if r.OK {
		return objectStorageCheckInterval
	}
	return objectStorageRetryInterval
}

// reportObjectStorage turns a bucket check into ObjectStorageReady.
func reportObjectStorage(rc *forgejo.ReconcileContext, c *forgejov1.S3Connection, r s3.Result) {
	where := fmt.Sprintf("%s at %s", c.Bucket, c.Endpoint)
	if r.OK {
		rc.SetCondition(forgejov1.ConditionObjectStorageReady, metav1.ConditionTrue, r.Reason,
			fmt.Sprintf("Bucket %s is reachable with the credentials of Secret %q", where, c.CredentialsSecret.Name))
		return
	}
	reason := r.Reason
	var msg string
	switch r.Reason {
	case s3.ReasonUnreachable:
		reason = reasonUnreachableFromOperator
		msg = fmt.Sprintf("The operator cannot reach %s (%s). The check runs from the operator's pod, so this holds "+
			"nothing (Forgejo pods may reach it); allow the operator's namespace in NetworkPolicies or firewalls as well "+
			"to have the bucket checked", where, r.Message)
	case s3.ReasonTLS:
		msg = fmt.Sprintf("The TLS connection to %s failed (%s). Forgejo verifies the certificate the same way and does "+
			"not start while this fails: set tls.caSecret to the CA that signed it", c.Endpoint, r.Message)
	case s3.ReasonNotFound:
		msg = fmt.Sprintf("%s at %s. Forgejo creates it when it starts if the access key may create buckets, "+
			"and does not start otherwise", r.Message, c.Endpoint)
	default:
		msg = fmt.Sprintf("%s at %s with the credentials of Secret %q. Forgejo does not start while this fails", r.Message, c.Endpoint, c.CredentialsSecret.Name)
	}
	rc.SetCondition(forgejov1.ConditionObjectStorageReady, metav1.ConditionFalse, reason, msg)
	warnEvery(rc, "object-storage/"+reason, "ObjectStorageUnavailable", "ObjectStorage", "%s", msg)
}

// ForgetObjectStorage drops the bucket check of a deleted instance.
func ForgetObjectStorage(namespace, name string) {
	objectStorageChecks.Delete(types.NamespacedName{Namespace: namespace, Name: name})
}

// secretKey reads one value of a Secret in the instance's namespace.
func secretKey(ctx context.Context, rc *forgejo.ReconcileContext, name, key string) (string, error) {
	sec := &corev1.Secret{}
	if err := rc.Client.Get(ctx, types.NamespacedName{Namespace: rc.Instance.Namespace, Name: name}, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("secret %q not found", name)
		}
		return "", fmt.Errorf("reading secret %q: %w", name, err)
	}
	v, ok := sec.Data[key]
	if !ok || len(v) == 0 {
		return "", fmt.Errorf("secret %q has no key %q", name, key)
	}
	return string(v), nil
}

// digest identifies a list of values (16 hex digits). It is unkeyed: for values that may be
// guessed, such as human-chosen keys, it stays in memory, and objects carry podDigest.
func digest(values ...string) string {
	return contentDigest(values...)[:16]
}

// contentDigest is the full unkeyed SHA-256 of a list of values (length-prefixed).
func contentDigest(values ...string) string {
	h := sha256.New()
	for _, v := range values {
		_, _ = fmt.Fprintf(h, "%d:%s\n", len(v), v)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// podDigest is the keyed digest of content for the pod template annotation of purpose:
// HMAC-SHA256 with the instance's PodDigestSalt (from "<name>-secrets"), 16 hex digits. It
// changes whenever content changes, but cannot be recomputed from guessed values without the
// salt, which only readers of that Secret have.
func podDigest(rc *forgejo.ReconcileContext, purpose, content string) (string, error) {
	if rc.PodDigestSalt == "" {
		return "", fmt.Errorf("the key %q of Secret %s is not known (the secrets step has not run)",
			secretKeyPodDigestSalt, forgejo.SecretsName(rc.Instance))
	}
	mac := hmac.New(sha256.New, []byte(rc.PodDigestSalt))
	mac.Write([]byte(purpose + "\x00" + content))
	return hex.EncodeToString(mac.Sum(nil))[:16], nil
}

// objectStorageConfig renders spec.objectStorage into app.ini: [storage] points every storage
// at the bucket (a subsystem's own section only overrides SERVE_DIRECT, MINIO_BASE_PATH and
// MINIO_BUCKET), and the subsystems kept on the data volume get STORAGE_TYPE = local in their
// section, which makes it their own target. The access key is not rendered: Forgejo takes it
// from the environment (objectStorageEnv). Identical for Forgejo 15 and 17.
func objectStorageConfig(cfg iniFile, f *forgejov1.Forgejo) error {
	spec := f.Spec.ObjectStorage
	if spec == nil {
		return nil
	}
	c := &spec.S3
	ep, err := forgejo.ParseS3Endpoint(c.Endpoint)
	if err != nil {
		return err
	}
	// MINIO_BUCKET_LOOKUP: auto, path or dns (virtual-hosted style).
	lookup := "auto"
	if c.PathStyle != nil {
		lookup = map[bool]string{true: "path", false: "dns"}[*c.PathStyle] //nolint:goconst // app.ini values
	}
	set := func(k, v string) { cfg.set("storage", k, v) }
	set("STORAGE_TYPE", "minio")
	set("MINIO_ENDPOINT", ep.Host)
	set("MINIO_USE_SSL", strconv.FormatBool(ep.Secure))
	set("MINIO_INSECURE_SKIP_VERIFY", strconv.FormatBool(forgejo.S3InsecureSkipVerify(c)))
	set("MINIO_BUCKET", c.Bucket)
	set("MINIO_LOCATION", forgejo.S3Region(c))
	set("MINIO_BUCKET_LOOKUP", lookup)
	if p := forgejo.S3Prefix(c); p != "" {
		set("MINIO_BASE_PATH", p)
	}
	set("SERVE_DIRECT", strconv.FormatBool(spec.ServeDirect))
	for _, s := range forgejo.StorageSubsystems {
		if !forgejo.InObjectStorage(f, s) {
			cfg.set(s.Section, "STORAGE_TYPE", "local")
		}
	}
	return nil
}

// lfsOnVolume reports whether LFS objects are kept on the data volume ([lfs] PATH).
func lfsOnVolume(f *forgejov1.Forgejo) bool {
	for _, s := range forgejo.StorageSubsystems {
		if s.Name == forgejo.StorageLFS {
			return !forgejo.InObjectStorage(f, s)
		}
	}
	return true
}

// objectStorageEnv hands the access key to Forgejo through references to the Secret, and adds
// the CA certificate's directory to the trust store's (Go reads SSL_CERT_DIR as a list, next
// to the system bundle; OpenSSL keeps its system directory too).
func objectStorageEnv(f *forgejov1.Forgejo, l forgejo.ImageLayout) []corev1.EnvVar {
	spec := f.Spec.ObjectStorage
	if spec == nil {
		return nil
	}
	c := &spec.S3
	ref := func(key string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: c.CredentialsSecret.Name}, Key: key,
		}}
	}
	env := []corev1.EnvVar{
		{Name: envS3AccessKeyID, ValueFrom: ref(forgejo.S3AccessKeyIDKey(c))},
		{Name: envS3SecretAccessKey, ValueFrom: ref(forgejo.S3SecretAccessKeyKey(c))},
	}
	if forgejo.S3CASecret(c) != nil {
		env = append(env, corev1.EnvVar{Name: "SSL_CERT_DIR", Value: l.SystemCertDir + ":" + l.S3CADir})
	}
	return env
}

// objectStorageVolume is the CA certificate's volume and its mount, when there is one.
func objectStorageVolume(f *forgejov1.Forgejo, l forgejo.ImageLayout) (*corev1.Volume, *corev1.VolumeMount) {
	if f.Spec.ObjectStorage == nil {
		return nil, nil
	}
	ref := forgejo.S3CASecret(&f.Spec.ObjectStorage.S3)
	if ref == nil {
		return nil, nil
	}
	return &corev1.Volume{Name: objectStorageCAVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  ref.Name,
			Items:       []corev1.KeyToPath{{Key: ref.Key, Path: objectStorageCAFile}},
			DefaultMode: ptr.To[int32](0o444),
		}}},
		&corev1.VolumeMount{Name: objectStorageCAVolume, MountPath: l.S3CADir, ReadOnly: true}
}
