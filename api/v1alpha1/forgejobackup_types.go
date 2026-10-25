/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// BackupReason records why a backup was taken.
// +kubebuilder:validation:Enum=Manual;Scheduled;PreUpgrade
type BackupReason string

const (
	BackupReasonManual     BackupReason = "Manual"
	BackupReasonScheduled  BackupReason = "Scheduled"
	BackupReasonPreUpgrade BackupReason = "PreUpgrade"
)

// BackupPhase is the lifecycle of a backup.
// +kubebuilder:validation:Enum=Pending;Running;Completed;Failed
type BackupPhase string

const (
	BackupPhasePending   BackupPhase = "Pending"
	BackupPhaseRunning   BackupPhase = "Running"
	BackupPhaseCompleted BackupPhase = "Completed"
	BackupPhaseFailed    BackupPhase = "Failed"
)

// ForgejoRef points at a Forgejo instance in the same namespace.
type ForgejoRef struct {
	// name of the Forgejo.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=56
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
}

// ForgejoBackupSpec defines a one-shot backup of a Forgejo instance: a PostgreSQL logical
// dump plus (optionally) an archive of the data volume, written to the "<forgejo>-backups" PVC.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type ForgejoBackupSpec struct {
	// forgejoRef is the instance to back up.
	ForgejoRef ForgejoRef `json:"forgejoRef"`

	// includeData archives the data volume (repositories, LFS, attachments). Defaults to
	// the instance's spec.backup.includeData (true).
	// +optional
	IncludeData *bool `json:"includeData,omitempty"`

	// reason is informational and drives retention: only Scheduled backups are pruned.
	// +kubebuilder:default=Manual
	// +optional
	Reason BackupReason `json:"reason,omitempty"`
}

// ForgejoBackupStatus defines the observed state of ForgejoBackup.
type ForgejoBackupStatus struct {
	// +optional
	Phase BackupPhase `json:"phase,omitempty"`
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	// +optional
	JobName string `json:"jobName,omitempty"`
	// pvcName holding the backup files.
	// +optional
	PVCName string `json:"pvcName,omitempty"`
	// path of the backup directory inside the PVC.
	// +optional
	Path string `json:"path,omitempty"`
	// forgejoImage running when the backup was taken (restores must use a compatible image).
	// +optional
	ForgejoImage string `json:"forgejoImage,omitempty"`
	// postgresImage whose pg_dump wrote the database dump: the image of the instance's running
	// PostgreSQL. A restore into a server of an older major is refused.
	// +optional
	PostgresImage string `json:"postgresImage,omitempty"`
	// +optional
	DatabaseSizeBytes int64 `json:"databaseSizeBytes,omitempty"`
	// +optional
	DataSizeBytes int64 `json:"dataSizeBytes,omitempty"`
	// +optional
	IncludesData bool `json:"includesData,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// remote reports the upload of the backup to spec.backup.remote of its instance.
	// +optional
	Remote *RemoteBackupStatus `json:"remote,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// RemoteBackupPhase is the lifecycle of the upload of a backup.
// +kubebuilder:validation:Enum=Pending;Uploading;Uploaded;Failed
type RemoteBackupPhase string

const (
	RemoteBackupPending   RemoteBackupPhase = "Pending"
	RemoteBackupUploading RemoteBackupPhase = "Uploading"
	RemoteBackupUploaded  RemoteBackupPhase = "Uploaded"
	RemoteBackupFailed    RemoteBackupPhase = "Failed"
)

// RemoteBackupStatus reports the remote copy of a backup.
type RemoteBackupStatus struct {
	// +optional
	Phase RemoteBackupPhase `json:"phase,omitempty"`
	// location of the backup: s3://<bucket>/<prefix>/<path>/.
	// +optional
	Location string `json:"location,omitempty"`
	// path of the backup below the prefix ("<namespace>/<name>/<backup>-<uid>", where <uid> is
	// the first 8 characters of the ForgejoBackup's UID, so that every ForgejoBackup has a
	// directory of its own): what a ForgejoRestore (spec.remote.path) or a new Forgejo
	// (spec.restoreFrom.remote.path) restores it by.
	// +optional
	Path string `json:"path,omitempty"`
	// s3 is the bucket the upload Job writes to, recorded when it starts: what the operator
	// checks (and cleans up) after an upload that did not succeed, whatever spec.backup.remote
	// says by then.
	// +optional
	S3 *S3Connection `json:"s3,omitempty"`
	// +optional
	JobName string `json:"jobName,omitempty"`
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	// keySecret held the encryption key.
	// +optional
	KeySecret *BackupKeySecretRef `json:"keySecret,omitempty"`
	// keyFingerprint identifies the key the backup was encrypted with.
	// +optional
	KeyFingerprint string `json:"keyFingerprint,omitempty"`
	// files uploaded, with the size and SHA-256 of their contents before encryption.
	// +listType=atomic
	// +optional
	Files []RemoteBackupFile `json:"files,omitempty"`
	// verified is true when every file was read back from the bucket, decrypted, and its
	// SHA-256 matched.
	// +optional
	Verified bool `json:"verified,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// RemoteBackupFile is one file of a remote backup.
type RemoteBackupFile struct {
	// name of the file before encryption (the object's name ends in ".bin").
	Name string `json:"name"`
	// sizeBytes of the contents before encryption.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// sha256 of the contents before encryption, in hex.
	// +optional
	SHA256 string `json:"sha256,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=fjb,categories=forgejo-all
// +kubebuilder:printcolumn:name="Forgejo",type=string,JSONPath=`.spec.forgejoRef.name`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.spec.reason`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Remote",type=string,JSONPath=`.status.remote.phase`
// +kubebuilder:printcolumn:name="Completed",type=date,JSONPath=`.status.completionTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ForgejoBackup is a point-in-time backup of a Forgejo instance.
type ForgejoBackup struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ForgejoBackup
	// +required
	Spec ForgejoBackupSpec `json:"spec"`

	// status defines the observed state of ForgejoBackup
	// +optional
	Status ForgejoBackupStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ForgejoBackupList contains a list of ForgejoBackup
type ForgejoBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ForgejoBackup `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ForgejoBackup{}, &ForgejoBackupList{})
		return nil
	})
}
