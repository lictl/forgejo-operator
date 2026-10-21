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
	// +kubebuilder:validation:MinLength=1
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
	// +optional
	DatabaseSizeBytes int64 `json:"databaseSizeBytes,omitempty"`
	// +optional
	DataSizeBytes int64 `json:"dataSizeBytes,omitempty"`
	// +optional
	IncludesData bool `json:"includesData,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=fjb,categories=forgejo
// +kubebuilder:printcolumn:name="Forgejo",type=string,JSONPath=`.spec.forgejoRef.name`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.spec.reason`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
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
