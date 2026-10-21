package forgejo

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

// Database and role created in CNPG-managed clusters.
const (
	CNPGDatabaseName = "forgejo"
	CNPGOwnerName    = "forgejo"
)

// CNPGAppSecretName is the credentials Secret CNPG generates for the application owner.
func CNPGAppSecretName(f *forgejov1.Forgejo) string { return DatabaseClusterName(f) + "-app" }

// CNPGReadWriteService is the CNPG service pointing at the current primary.
func CNPGReadWriteService(f *forgejov1.Forgejo) string { return DatabaseClusterName(f) + "-rw" }

// ResolveDB returns the connection details for the instance's database, reading the
// password from the credentials Secret. It is used by the core pipeline and by
// backup/restore jobs (which should reference SecretName/keys rather than the password).
func ResolveDB(ctx context.Context, c client.Client, f *forgejov1.Forgejo) (*DBConnection, error) {
	db := &DBConnection{}
	switch DatabaseType(f) {
	case forgejov1.DatabaseTypeExternal:
		ext := f.Spec.Database.External
		if ext == nil {
			return nil, fmt.Errorf("spec.database.external is required when type is External")
		}
		db.Host = ext.Host
		db.Port = ext.Port
		if db.Port == 0 {
			db.Port = 5432
		}
		db.Name = ext.Database
		if db.Name == "" {
			db.Name = "forgejo"
		}
		db.SSLMode = ext.SSLMode
		if db.SSLMode == "" {
			db.SSLMode = "disable"
		}
		db.SecretName = ext.CredentialsSecret.Name
		db.UsernameKey = ext.CredentialsSecret.UsernameKey
		db.PasswordKey = ext.CredentialsSecret.PasswordKey
	default:
		db.Host = CNPGReadWriteService(f) + "." + f.Namespace + ".svc.cluster.local"
		db.Port = 5432
		db.Name = CNPGDatabaseName
		db.SSLMode = "disable"
		db.SecretName = CNPGAppSecretName(f)
		db.UsernameKey = "username"
		db.PasswordKey = "password"
	}
	if db.UsernameKey == "" {
		db.UsernameKey = "username"
	}
	if db.PasswordKey == "" {
		db.PasswordKey = "password"
	}

	sec := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: db.SecretName}, sec); err != nil {
		return nil, fmt.Errorf("reading database credentials secret %q: %w", db.SecretName, err)
	}
	db.User = string(sec.Data[db.UsernameKey])
	db.Password = string(sec.Data[db.PasswordKey])
	if db.User == "" || db.Password == "" {
		return nil, fmt.Errorf("database credentials secret %q lacks %q/%q", db.SecretName, db.UsernameKey, db.PasswordKey)
	}
	return db, nil
}
