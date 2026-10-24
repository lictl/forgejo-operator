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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Defaults of ForgejoRunner (see docs/actions-runners.md).
const (
	// DefaultRunnerImage is the Forgejo Runner release used with Forgejo 15 and 17.
	DefaultRunnerImage = "data.forgejo.org/forgejo/runner:13.2.0"
	// DefaultDindImage runs the Docker daemon of the Docker executor.
	DefaultDindImage = "docker.io/library/docker:29.9.2-dind"
	// DefaultRunnerToolchainImage is the image the Host executor runs the runner and the job
	// steps in. It has node (for JavaScript actions such as actions/checkout), git and bash, and
	// a uid 1000 user.
	DefaultRunnerToolchainImage = "data.forgejo.org/oci/node:22-bookworm"
	// DefaultRunnerJobImage is the image of the default label of the Docker executor.
	DefaultRunnerJobImage = "data.forgejo.org/oci/node:22-bookworm"
)

// Condition types reported on ForgejoRunner.status.conditions.
const (
	// ConditionRunnerRegistered is True when every runner of spec.replicas is registered with
	// Forgejo for the current scope.
	ConditionRunnerRegistered = "Registered"
)

// RunnerExecutor selects where the steps of a job run.
// +kubebuilder:validation:Enum=Docker;Host
type RunnerExecutor string

const (
	// RunnerExecutorDocker runs every job in containers of a Docker daemon in a privileged
	// sidecar (docker:dind) of the runner pod. Labels have the form "name:docker://image".
	// The namespace must allow privileged pods (Pod Security "privileged").
	RunnerExecutorDocker RunnerExecutor = "Docker"
	// RunnerExecutorHost runs the steps of a job as processes in the runner's container
	// (spec.host.image), without isolation from the runner. Labels have the form "name:host".
	// The pod passes the "restricted" Pod Security Standard.
	RunnerExecutorHost RunnerExecutor = "Host"
)

// RunnerScope selects which jobs the runners take. Empty: every repository of the instance.
// +kubebuilder:validation:XValidation:rule="!(has(self.organization) && has(self.repository))",message="set at most one of organization and repository"
type RunnerScope struct {
	// organization registers the runners for the repositories of this organization (or user).
	// +optional
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.-]+$`
	Organization string `json:"organization,omitempty"`

	// repository registers the runners for one repository, "owner/name".
	// +optional
	// +kubebuilder:validation:MaxLength=150
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`
	Repository string `json:"repository,omitempty"`
}

// RunnerDockerSpec configures the Docker daemon of the Docker executor.
type RunnerDockerSpec struct {
	// image of the Docker daemon (docker:dind). Defaults to DefaultDindImage.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Image string `json:"image,omitempty"`

	// mountSocket gives job containers the daemon's socket at /var/run/docker.sock
	// (container.docker_host: automount), so that steps can run "docker build" and
	// "docker run". A job with the socket controls the privileged daemon, which amounts to root
	// on the node: enable it only for trusted repositories.
	// +optional
	MountSocket bool `json:"mountSocket,omitempty"`

	// args are added to the dockerd command line, e.g. "--mtu=1400" or
	// "--registry-mirror=https://mirror.example.com".
	// +optional
	Args []string `json:"args,omitempty"`

	// resources of the Docker daemon container. The job containers run inside it.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// storageSizeLimit limits the emptyDir that holds images and containers
	// (/var/lib/docker). The kubelet evicts the pod when it is exceeded.
	// +optional
	StorageSizeLimit *resource.Quantity `json:"storageSizeLimit,omitempty"`
}

// RunnerHostSpec configures the Host executor.
type RunnerHostSpec struct {
	// image the runner and the job steps run in. An init container copies the runner binary
	// into it, so it needs no runner of its own; it should have node (for JavaScript actions),
	// git and a shell, and run as a non-root user. Defaults to DefaultRunnerToolchainImage.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Image string `json:"image,omitempty"`
}

// RunnerCacheSpec configures the built-in cache server of each runner (actions/cache).
type RunnerCacheSpec struct {
	// enabled serves ACTIONS_CACHE_URL from the runner pod. Defaults to true.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// sizeLimit limits the emptyDir that holds the cache.
	// +optional
	SizeLimit *resource.Quantity `json:"sizeLimit,omitempty"`
}

// ForgejoRunnerSpec defines a set of Forgejo Actions runners for one Forgejo instance.
// +kubebuilder:validation:XValidation:rule="!has(self.labels) || self.executor != 'Host' || self.labels.all(l, l.matches('^[^:,]+:host$'))",message="labels of the Host executor have the form name:host"
// +kubebuilder:validation:XValidation:rule="!has(self.labels) || self.executor != 'Docker' || self.labels.all(l, l.matches('^[^:,]+:docker://[^,]+$'))",message="labels of the Docker executor have the form name:docker://image"
// +kubebuilder:validation:XValidation:rule="self.executor == 'Docker' || !has(self.docker) || (!has(self.docker.image) && !has(self.docker.args) && !has(self.docker.storageSizeLimit) && (!has(self.docker.mountSocket) || !self.docker.mountSocket))",message="docker settings apply to the Docker executor only",fieldPath=".docker",reason="FieldValueForbidden"
// +kubebuilder:validation:XValidation:rule="self.executor == 'Host' || !has(self.host) || !has(self.host.image)",message="host.image applies to the Host executor only",fieldPath=".host.image",reason="FieldValueForbidden"
type ForgejoRunnerSpec struct {
	// forgejoRef is the Forgejo instance (in the same namespace) the runners register with.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="forgejoRef is immutable"
	ForgejoRef ForgejoRef `json:"forgejoRef"`

	// scope of the runners: an organization, a repository, or (empty) the whole instance.
	// +optional
	Scope RunnerScope `json:"scope,omitempty"`

	// labels the runners offer, matched against "runs-on" of jobs: "name:docker://image" for
	// the Docker executor, "name:host" for the Host executor. Defaults to
	// "docker:docker://<DefaultRunnerJobImage>" and "host:host".
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=512
	Labels []string `json:"labels,omitempty"`

	// replicas is the number of runners. Each one is registered with Forgejo under its own
	// name ("<name>-<index>") and unregistered when it is scaled away.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	Replicas *int32 `json:"replicas,omitempty"`

	// capacity is the number of jobs each runner runs at once (runner.capacity).
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	Capacity int32 `json:"capacity,omitempty"`

	// executor selects where job steps run (see docs/actions-runners.md): Docker runs a
	// privileged Docker daemon next to each runner, Host runs the steps in the runner's own
	// unprivileged container. It has no default, so that a privileged pod is only created when
	// it is asked for.
	// +required
	Executor RunnerExecutor `json:"executor"`

	// image of Forgejo Runner.
	// +optional
	// +kubebuilder:default="data.forgejo.org/forgejo/runner:13.2.0"
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Image string `json:"image,omitempty"`

	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// docker configures the Docker executor.
	// +optional
	Docker RunnerDockerSpec `json:"docker,omitempty"`

	// host configures the Host executor.
	// +optional
	Host RunnerHostSpec `json:"host,omitempty"`

	// resources of the runner container (with the Host executor, the job steps run there).
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// env is added to the runner container, e.g. HTTP_PROXY. Jobs get variables from
	// config.runner.envs instead.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// cache configures the built-in cache server.
	// +optional
	Cache RunnerCacheSpec `json:"cache,omitempty"`

	// shutdownTimeout is how long a stopping runner (scale-down, rollout, deletion) lets its
	// running jobs finish before it cancels them. Defaults to 10m.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|µs|μs|ms|s|m|h))+$`
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="must be a positive Go duration such as 30s, 5m or 1h"
	// +optional
	ShutdownTimeout *metav1.Duration `json:"shutdownTimeout,omitempty"`

	// config is merged into the runner's configuration file (config.yml of Forgejo Runner),
	// e.g. {"runner": {"envs": {...}, "timeout": "1h"}, "container": {"options": "..."}}.
	// The operator's server.connections and runner.labels/capacity always win.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Type=object
	Config *runtime.RawExtension `json:"config,omitempty"`

	// podTemplate carries scheduling knobs for the runner pods.
	// +optional
	PodTemplate PodTemplateSpec `json:"podTemplate,omitempty"`
}

// RegisteredRunner is one runner registered with Forgejo.
type RegisteredRunner struct {
	// index of the runner pod ("<name>-<index>").
	Index int32 `json:"index"`
	// name of the runner in Forgejo.
	Name string `json:"name"`
	// uuid of the runner in Forgejo (not secret).
	UUID string `json:"uuid"`
	// scope the runner was registered for: "" (the whole instance), "<organization>" or
	// "<owner>/<repository>".
	// +optional
	Scope string `json:"scope,omitempty"`
	// id of the runner in Forgejo, once the operator read it from Forgejo's API.
	// +optional
	ID int64 `json:"id,omitempty"`
	// status Forgejo reports: offline, idle or active. Empty while unknown.
	// +optional
	Status string `json:"status,omitempty"`
	// +optional
	RegisteredTime *metav1.Time `json:"registeredTime,omitempty"`
	// missing is true while Forgejo no longer has the runner (deleted in the web UI, by
	// Forgejo's offline-runner cleanup, or a database restored from an earlier backup): it is
	// registered again with the same secret and UUID, and the runner pods keep running.
	// +optional
	Missing bool `json:"missing,omitempty"`
}

// ForgejoRunnerStatus defines the observed state of ForgejoRunner.
type ForgejoRunnerStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions: Ready, Registered, Degraded.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// replicas is the number of runner pods.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// readyReplicas is the number of runner pods that are ready.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// onlineRunners is the number of runners Forgejo reports online (idle or active).
	// +optional
	OnlineRunners int32 `json:"onlineRunners,omitempty"`

	// selector for the scale subresource.
	// +optional
	Selector string `json:"selector,omitempty"`

	// image of the runner pods.
	// +optional
	Image string `json:"image,omitempty"`

	// configHash of the runner configuration the pods run with.
	// +optional
	ConfigHash string `json:"configHash,omitempty"`

	// forgejoUID is the UID of the Forgejo instance the runners are registered with.
	// +optional
	ForgejoUID string `json:"forgejoUID,omitempty"`

	// runners are registered with Forgejo, by index.
	// +listType=map
	// +listMapKey=index
	// +optional
	Runners []RegisteredRunner `json:"runners,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:resource:path=forgejorunners,shortName=fjrn,categories=forgejo
// +kubebuilder:printcolumn:name="Forgejo",type=string,JSONPath=`.spec.forgejoRef.name`
// +kubebuilder:printcolumn:name="Executor",type=string,JSONPath=`.spec.executor`
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Online",type=integer,JSONPath=`.status.onlineRunners`
// +kubebuilder:printcolumn:name="Registered",type=string,JSONPath=`.status.conditions[?(@.type=="Registered")].status`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Organization",type=string,JSONPath=`.spec.scope.organization`,priority=1
// +kubebuilder:printcolumn:name="Repository",type=string,JSONPath=`.spec.scope.repository`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 52",message="metadata.name must be at most 52 characters: the runner pods carry it in labels limited to 63 characters"
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="metadata.name must contain only lowercase letters, digits and '-' (no '.'): the runner pods' host names are \"<name>-<index>\""
// +kubebuilder:validation:XValidation:rule="!self.metadata.name.endsWith('-valkey') && !self.metadata.name.endsWith('-cron')",message="metadata.name must not end in '-valkey' or '-cron': the runners' StatefulSet \"<name>\" would have the name of the Valkey or cron runner StatefulSet of a Forgejo instance with highAvailability"

// ForgejoRunner is a set of Forgejo Actions runners registered with a Forgejo instance.
type ForgejoRunner struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ForgejoRunner
	// +required
	Spec ForgejoRunnerSpec `json:"spec"`

	// status defines the observed state of ForgejoRunner
	// +optional
	Status ForgejoRunnerStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ForgejoRunnerList contains a list of ForgejoRunner
type ForgejoRunnerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ForgejoRunner `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ForgejoRunner{}, &ForgejoRunnerList{})
		return nil
	})
}
