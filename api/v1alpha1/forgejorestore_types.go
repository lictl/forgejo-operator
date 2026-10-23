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

// RestorePhase is the lifecycle of a restore.
// +kubebuilder:validation:Enum=Pending;ScalingDown;Restoring;ScalingUp;Completed;Failed
type RestorePhase string

const (
	RestorePhasePending     RestorePhase = "Pending"
	RestorePhaseScalingDown RestorePhase = "ScalingDown"
	RestorePhaseRestoring   RestorePhase = "Restoring"
	RestorePhaseScalingUp   RestorePhase = "ScalingUp"
	RestorePhaseCompleted   RestorePhase = "Completed"
	RestorePhaseFailed      RestorePhase = "Failed"
)

// ForgejoRestoreSpec restores a ForgejoBackup, or a remote backup, into a Forgejo instance.
// While a restore is active the instance is scaled to zero (maintenance mode).
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.backupName) != has(self.remote)",message="set exactly one of backupName and remote"
type ForgejoRestoreSpec struct {
	// forgejoRef is the instance to restore into.
	ForgejoRef ForgejoRef `json:"forgejoRef"`

	// backupName is a completed ForgejoBackup in the same namespace.
	// +kubebuilder:validation:MinLength=1
	// +optional
	BackupName string `json:"backupName,omitempty"`

	// remote restores a backup from object storage by its location; no ForgejoBackup object is
	// needed. The identity Secrets of the instance are not changed: to recreate a lost instance
	// with its identity, use spec.restoreFrom of a new Forgejo instead.
	// +optional
	Remote *RemoteBackupSource `json:"remote,omitempty"`

	// restoreDatabase replaces the database with the dump. Defaults to true.
	// +optional
	RestoreDatabase *bool `json:"restoreDatabase,omitempty"`

	// restoreData replaces the data volume with the archive (if the backup has one). Defaults to true.
	// +optional
	RestoreData *bool `json:"restoreData,omitempty"`
}

// ForgejoRestoreStatus defines the observed state of ForgejoRestore.
type ForgejoRestoreStatus struct {
	// +optional
	Phase RestorePhase `json:"phase,omitempty"`
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	// +optional
	JobName string `json:"jobName,omitempty"`
	// jobStartTime is when the restore Job was created (the transition to Restoring). Grace
	// periods for the Job to show up in the operator's cache count from here, not from
	// startTime, which also covers scaling down.
	// +optional
	JobStartTime *metav1.Time `json:"jobStartTime,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// remote describes the remote backup of spec.remote, read from its manifest.
	// +optional
	Remote *RestoreRemoteStatus `json:"remote,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// RestoreRemoteStatus describes the remote backup a restore reads.
type RestoreRemoteStatus struct {
	// location of the backup: s3://<bucket>/<prefix>/<path>/.
	// +optional
	Location string `json:"location,omitempty"`
	// backupName is the name of the ForgejoBackup that took it.
	// +optional
	BackupName string `json:"backupName,omitempty"`
	// forgejoImage ran when the backup was taken.
	// +optional
	ForgejoImage string `json:"forgejoImage,omitempty"`
	// includesData is true when the backup has an archive of the data volume.
	// +optional
	IncludesData bool `json:"includesData,omitempty"`
	// keyFingerprint identifies the key the backup was encrypted with.
	// +optional
	KeyFingerprint string `json:"keyFingerprint,omitempty"`
	// files of the backup, with the size and SHA-256 the download is checked against.
	// +listType=atomic
	// +optional
	Files []RemoteBackupFile `json:"files,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=fjr,categories=forgejo
// +kubebuilder:printcolumn:name="Forgejo",type=string,JSONPath=`.spec.forgejoRef.name`
// +kubebuilder:printcolumn:name="Backup",type=string,JSONPath=`.spec.backupName`
// +kubebuilder:printcolumn:name="Remote",type=string,JSONPath=`.spec.remote.path`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ForgejoRestore restores a Forgejo instance from a ForgejoBackup.
type ForgejoRestore struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ForgejoRestore
	// +required
	Spec ForgejoRestoreSpec `json:"spec"`

	// status defines the observed state of ForgejoRestore
	// +optional
	Status ForgejoRestoreStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ForgejoRestoreList contains a list of ForgejoRestore
type ForgejoRestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ForgejoRestore `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ForgejoRestore{}, &ForgejoRestoreList{})
		return nil
	})
}
