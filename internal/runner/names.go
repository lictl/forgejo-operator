// Package runner renders the Kubernetes objects of a ForgejoRunner (runner configuration,
// StatefulSet, registration Job), generates the runners' secrets and talks to Forgejo's API
// about runners. The reconcile loop is in internal/controller/forgejorunner_controller.go;
// docs/actions-runners.md documents the behaviour.
package runner

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// Annotations of the registration Job.
const (
	// AnnotationRegistrationScope on the registration Job is the scope it registers for.
	AnnotationRegistrationScope = "forgejo.lucas.icu/registration-scope"
	// AnnotationRegistrationRunners on the registration Job lists what it registers as
	// "<index>=<uuid>" separated by commas.
	AnnotationRegistrationRunners = "forgejo.lucas.icu/registration-runners"
)

// StatefulSetName is the StatefulSet of the runner pods ("<name>"); pod i is "<name>-<i>".
func StatefulSetName(r *forgejov1.ForgejoRunner) string { return r.Name }

// TokensSecretName holds the runners' secrets ("<name>-tokens", keys token-<i>).
func TokensSecretName(r *forgejov1.ForgejoRunner) string { return r.Name + "-tokens" }

// ConfigSecretName holds the runners' configuration files ("<name>-runner", keys
// config-<i>.yaml). The suffix differs from every Secret of a Forgejo instance (whose
// "<name>-config" holds app.ini), so a runner and an instance may share a name.
func ConfigSecretName(r *forgejov1.ForgejoRunner) string { return r.Name + "-runner" }

// RegisterJobName is the registration Job ("<name>-register").
func RegisterJobName(r *forgejov1.ForgejoRunner) string { return r.Name + "-register" }

// RunnerName is the name of runner i in Forgejo, the same as its pod.
func RunnerName(r *forgejov1.ForgejoRunner, i int32) string { return fmt.Sprintf("%s-%d", r.Name, i) }

// TokenKey is the key of runner i's secret in TokensSecretName.
func TokenKey(i int32) string { return fmt.Sprintf("token-%d", i) }

// ConfigKey is the key of runner i's configuration in ConfigSecretName.
func ConfigKey(i int32) string { return fmt.Sprintf("config-%d.yaml", i) }

// IndexOfKey parses the index of a TokenKey or ConfigKey.
func IndexOfKey(key string) (int32, bool) {
	var s string
	switch {
	case strings.HasPrefix(key, "token-"):
		s = strings.TrimPrefix(key, "token-")
	case strings.HasPrefix(key, "config-") && strings.HasSuffix(key, ".yaml"):
		s = strings.TrimSuffix(strings.TrimPrefix(key, "config-"), ".yaml")
	default:
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != s {
		return 0, false
	}
	return int32(n), true
}

// SelectorLabels select the runner pods of r.
func SelectorLabels(r *forgejov1.ForgejoRunner) map[string]string {
	return map[string]string{
		forgejo.LabelName:      forgejo.AppName,
		forgejo.LabelInstance:  r.Name,
		forgejo.LabelComponent: forgejo.ComponentRunner,
	}
}

// Labels are put on every object of r. app.kubernetes.io/name=forgejo and managed-by keep
// them in the operator's informer cache (forgejo.CacheScopes).
func Labels(r *forgejov1.ForgejoRunner, component string) map[string]string {
	return map[string]string{
		forgejo.LabelName:      forgejo.AppName,
		forgejo.LabelInstance:  r.Name,
		forgejo.LabelComponent: component,
		forgejo.LabelManagedBy: forgejo.ManagedBy,
		forgejo.LabelPartOf:    forgejo.AppName,
		forgejo.LabelForgejo:   r.Spec.ForgejoRef.Name,
		forgejo.LabelRunner:    r.Name,
	}
}

// NewToken returns a runner secret as "forgejo-cli actions register" wants it: 40 hex
// characters. Forgejo derives the runner's UUID from the first 16 (see UUIDOf).
func NewToken() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ValidToken reports whether s is a runner secret Forgejo accepts.
func ValidToken(s string) bool {
	if len(s) != 40 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// UUIDOf is the UUID Forgejo gives the runner registered with token: the first 16 bytes of
// the token taken as the UUID's bytes (models/actions.RegisterRunner in Forgejo 15 and 17).
func UUIDOf(token string) string {
	if len(token) < 16 {
		return ""
	}
	u, err := uuid.FromBytes([]byte(token[:16]))
	if err != nil {
		return ""
	}
	return u.String()
}

// Scope is the --scope of "forgejo-cli actions register": "" (instance), "<org>" or
// "<owner>/<repo>".
func Scope(r *forgejov1.ForgejoRunner) string {
	switch {
	case r.Spec.Scope.Repository != "":
		return r.Spec.Scope.Repository
	case r.Spec.Scope.Organization != "":
		return r.Spec.Scope.Organization
	}
	return ""
}

// ScopeDescription names the scope for messages.
func ScopeDescription(r *forgejov1.ForgejoRunner) string {
	switch {
	case r.Spec.Scope.Repository != "":
		return "repository " + r.Spec.Scope.Repository
	case r.Spec.Scope.Organization != "":
		return "organization " + r.Spec.Scope.Organization
	}
	return "the whole instance"
}
