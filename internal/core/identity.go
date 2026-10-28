package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// brokenRequeue re-checks a Broken instance; the watches on its Secrets, PVCs and CNPG
// cluster report a restored object sooner.
const brokenRequeue = time.Minute

// identityReasons are the reasons of condition Broken that the identity step reports.
var identityReasons = []string{
	forgejo.BrokenSecretsMissing, forgejo.BrokenDataVolumeMissing, forgejo.BrokenDatabaseMissing, forgejo.BrokenIdentityMismatch,
}

// ReconcileIdentity checks, before anything is generated or created, that the objects that make
// up the instance belong together: Secret "<name>-secrets" (SECRET_KEY and the tokens), the
// data volume and, for database type CNPG, the CNPG cluster. The data volume and the cluster
// carry the SECRET_KEY fingerprint they were created for (forgejo.AnnotationIdentity) and the
// last image that ran on them (forgejo.AnnotationRunningImage).
//
// Once the instance has run, a missing or foreign one is not replaced: a new SECRET_KEY leaves
// two-factor secrets, Actions secrets and mirror credentials undecryptable, and a new, empty
// database or volume next to the old other half serves an empty forge. The step reports
// condition Broken and stops, so the steps that generate, create and roll out are skipped,
// running pods are left alone, and backups, retention, upgrades and autopilot wait until a
// person restores what is missing.
//
// A volume or cluster without the identity annotation is adopted: one of an instance that ran
// before the annotations existed, one whose adoption a failed write left half-done, or one a person
// put in place (a PVC restored from a snapshot, another existingClaim). The operator never
// creates one for an instance that has run, so an unannotated one was put there on purpose. It
// also recovers status.image from the annotations when status was lost, so that the upgrade gate
// keeps comparing spec.image with the version that ran.
func ReconcileIdentity(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	sec := &corev1.Secret{}
	hasSecret, err := forgejo.GetConfirmed(ctx, rc, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.SecretsName(f)}, sec)
	if err != nil {
		return forgejo.StepResult{}, fmt.Errorf("reading Secret %s: %w", forgejo.SecretsName(f), err)
	}
	data, err := forgejo.ReadDataObjects(ctx, rc)
	if err != nil {
		return forgejo.StepResult{}, err
	}
	fingerprint := ""
	if hasSecret {
		fingerprint = forgejo.SecretKeyFingerprint(sec.Data)
	}
	rc.IdentityFingerprint = fingerprint

	ran := f.Status.Image
	for _, o := range data.Present() {
		if ran == "" {
			ran = o.Object.GetAnnotations()[forgejo.AnnotationRunningImage]
		}
	}
	rc.IdentityInitialized = ran != ""

	if forgejo.Bootstrapping(f) {
		// spec.restoreFrom restores the identity, the database and the data volume from the
		// backup, and judges objects that existed before the instance itself (TargetNotEmpty).
		// It needs the volume and the cluster created next to leftovers of an instance of the
		// same name, and stamps them with the restored identity before Forgejo starts.
		rc.IdentityInitialized = false
		rc.ClearBroken(identityReasons...)
		return forgejo.StepResult{}, nil
	}

	if reason, msg := identityProblem(rc, sec, hasSecret, data); reason != "" {
		rc.SetBroken(reason, msg)
		return forgejo.StepResult{Stop: true, RequeueAfter: brokenRequeue}, nil
	}
	rc.ClearBroken(identityReasons...)

	if fingerprint != "" {
		for _, o := range data.Present() {
			if err := forgejo.StampIdentity(ctx, rc.Client, o.Object, fingerprint, f.Status.Image); err != nil {
				return forgejo.StepResult{}, fmt.Errorf("recording the identity on %s %s: %w", o.Kind, o.Object.GetName(), err)
			}
		}
	}
	if f.Status.Image == "" && ran != "" {
		rc.Event(corev1.EventTypeWarning, "StatusImageRecovered", "Identity",
			"status.image was empty although %s ran on the data volume or the database; recovered it from their annotation %s",
			ran, forgejo.AnnotationRunningImage)
		f.Status.Image = ran
		rc.TargetImage = ran
		rc.Layout = forgejo.LayoutForImage(ran)
	}
	return forgejo.StepResult{}, nil
}

// identityProblem returns the reason and message of condition Broken for the identity Secret
// and the data objects, or "" when they belong together (or are yet to be created for an
// instance that never ran).
func identityProblem(rc *forgejo.ReconcileContext, sec *corev1.Secret, hasSecret bool, data *forgejo.DataObjects) (string, string) {
	f := rc.Instance
	initialized := rc.IdentityInitialized
	secretName := forgejo.SecretsName(f)
	restore := fmt.Sprintf("Restore it (a remote backup keeps Secret %s in identity.json), or create the instance again from a "+
		"backup with spec.restoreFrom. The operator does not create it again for an instance that has run", secretName)

	if initialized {
		var lost []string
		for _, key := range forgejo.IdentitySecretKeys {
			if !hasSecret || len(sec.Data[key]) == 0 {
				lost = append(lost, key)
			}
		}
		switch {
		case !hasSecret:
			return forgejo.BrokenSecretsMissing, fmt.Sprintf("Secret %s with SECRET_KEY and the tokens of this instance is missing. %s",
				secretName, restore)
		case len(lost) > 0:
			return forgejo.BrokenSecretsMissing, fmt.Sprintf("Secret %s lacks %s. %s", secretName, strings.Join(lost, ", "), restore)
		}
	}

	fingerprint := rc.IdentityFingerprint
	for _, o := range data.Present() {
		id := o.Object.GetAnnotations()[forgejo.AnnotationIdentity]
		if id != "" && fingerprint != "" && id != fingerprint {
			return forgejo.BrokenIdentityMismatch, fmt.Sprintf(
				"%s %s was created for another identity (annotation %s=%s) than Secret %s holds (%s): one of them belongs to another "+
					"instance. Restore the matching one; if this %s is the right one after all, remove its annotation %s",
				o.Kind, o.Object.GetName(), forgejo.AnnotationIdentity, id, secretName, fingerprint, o.Kind, forgejo.AnnotationIdentity)
		}
	}

	if !initialized {
		return "", ""
	}
	if data.Volume == nil {
		return forgejo.BrokenDataVolumeMissing, fmt.Sprintf(
			"PVC %s with the repositories of this instance is missing. Restore it (a PVC put in place is adopted), or set "+
				"spec.replicas to 0, create an empty PVC of that name and restore a backup into it (ForgejoRestore), or create the "+
				"instance again from a backup with spec.restoreFrom. The operator does not create an empty one for an instance "+
				"that has run", data.VolumeName)
	}
	// While a PostgreSQL major upgrade is in flight, its own step reports a lost cluster (and ends
	// the upgrade, holding Forgejo); afterwards this step does.
	if data.ClusterChecked && data.Cluster == nil && !forgejo.DatabaseUpgradeInFlight(f) {
		if f.Annotations[forgejo.AnnotationRecreateDatabase] == valueTrue {
			// Asked for: the database-upgrade step keeps Forgejo stopped until a restore.
			rc.DatabaseRecreate = true
			return "", ""
		}
		return forgejo.BrokenDatabaseMissing, fmt.Sprintf(
			"CNPG cluster %s with the database of this instance is missing. Set the annotation %s=true on the Forgejo to have "+
				"it created again, empty, and restore a backup of the database into it with a ForgejoRestore (Forgejo stays "+
				"stopped until then), or create the instance again from a backup with spec.restoreFrom. The operator does not "+
				"create an empty database for an instance that has run by itself", data.ClusterName, forgejo.AnnotationRecreateDatabase)
	}
	return "", ""
}
