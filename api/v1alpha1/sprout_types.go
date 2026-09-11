/*
Copyright 2026 bpalko.

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const SproutFinalizer = "db.sproutdb.dev/finalizer"

const (
	// ConditionReady summarizes whether the tenant database, role, and
	// Secret are all provisioned and match the current spec.
	ConditionReady = "Ready"
	// ConditionDatabaseProvisioned indicates the logical database and role
	// exist on the target instance and are owned by this Sprout.
	ConditionDatabaseProvisioned = "DatabaseProvisioned"
	// ConditionSecretReady indicates the credentials Secret exists in the
	// cluster and matches the role's current password.
	ConditionSecretReady = "SecretReady"
)

type SproutPhase string

const (
	SproutPhasePending      SproutPhase = "Pending"
	SproutPhaseProvisioning SproutPhase = "Provisioning"
	SproutPhaseReady        SproutPhase = "Ready"
	SproutPhaseTerminating  SproutPhase = "Terminating"
	SproutPhaseFailed       SproutPhase = "Failed"
)

// ConnectionSpec describes the target database instance a Sprout is
// provisioned against, and where to find the admin credentials used to do
// the provisioning.
type ConnectionSpec struct {
	// host is the network address of the target database instance.
	// +required
	Host string `json:"host"`

	// port is the network port of the target database instance.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +required
	Port int32 `json:"port"`

	// adminSecretRef names a Secret, in the same namespace as this Sprout,
	// holding admin credentials (username/password keys) with enough
	// privilege to create databases, roles, and grants on the target
	// instance. The referenced Secret must already exist; Sprout currently never
	// provisions the underlying database instance itself, it assumes you are
	// bringing your own.
	// +required
	AdminSecretRef corev1.LocalObjectReference `json:"adminSecretRef"`

	// adminDatabase is the database name used as dbname= when the
	// operator opens its privileged session. Postgres requires a
	// connection to some existing database before it can run CREATE
	// DATABASE; this is that database, not the Sprout's own database
	// and not a template. Defaults to "postgres".
	// +kubebuilder:default="postgres"
	// +kubebuilder:validation:MaxLength=63
	// +optional
	AdminDatabase string `json:"adminDatabase,omitempty"`

	// sslMode is the libpq SSL mode used for both the operator connection
	// and the tenant credentials Secret. Defaults to "prefer": try TLS
	// and fall back if the server has it disabled (typical of local
	// Postgres images).
	// +kubebuilder:default="prefer"
	// +kubebuilder:validation:Enum=disable;allow;prefer;require;verify-ca;verify-full
	// +optional
	SSLMode string `json:"sslMode,omitempty"`
}

type SproutSpec struct {
	// provider selects which backend implementation provisions this
	// database. Immutable after creation: changing it would orphan
	// whatever was already provisioned under the old provider.
	// +kubebuilder:validation:Enum=postgres
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="provider is immutable"
	// +required
	Provider string `json:"provider"`

	// connection describes the target database instance and how to
	// authenticate to it as an admin.
	// +required
	Connection ConnectionSpec `json:"connection"`

	// databaseName overrides the logical database name that would
	// otherwise be derived deterministically from this Sprout's namespace
	// and name. Immutable after creation: changing it would orphan the
	// database provisioned under the old name.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="databaseName is immutable"
	// +optional
	DatabaseName string `json:"databaseName,omitempty"`

	// connectionLimit caps the number of concurrent connections the
	// tenant's role may hold open. Defaults to 20 when unset.
	// +kubebuilder:validation:Minimum=0
	// +optional
	ConnectionLimit *int32 `json:"connectionLimit,omitempty"`
}

type SproutStatus struct {
	// +optional
	Phase SproutPhase `json:"phase,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// databaseName is the logical database name actually provisioned on
	// the target instance.
	// +optional
	DatabaseName string `json:"databaseName,omitempty"`

	// roleName is the login role created for this tenant's credentials.
	// +optional
	RoleName string `json:"roleName,omitempty"`

	// secretName is the name of the Secret, in this Sprout's namespace,
	// holding the tenant's connection credentials.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// observedGeneration is the most recent .metadata.generation the
	// controller has acted on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=spr
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.provider`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Database",type=string,JSONPath=`.status.databaseName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Sprout is the Schema for the sprouts API. Each Sprout represents one
// isolated logical database, provisioned fresh (schema and data both
// start empty) on an existing target instance for something like a PR
// preview environment.
type Sprout struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Sprout
	// +required
	Spec SproutSpec `json:"spec"`

	// status defines the observed state of Sprout
	// +optional
	Status SproutStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SproutList contains a list of Sprout
type SproutList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Sprout `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Sprout{}, &SproutList{})
		return nil
	})
}
