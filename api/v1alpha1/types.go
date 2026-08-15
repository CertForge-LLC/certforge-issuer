package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ─── WorkloadIdentitySpec ─────────────────────────────────────────────────────

// WorkloadIdentitySpec configures token-file-based authentication using a
// Kubernetes projected ServiceAccount token. The kubelet automatically writes
// and rotates the token at the specified path. No Secret is required.
//
// To use workload identity:
//  1. Omit authSecretRef from the issuer spec.
//  2. Set workloadIdentity with the audience and tokenFile path.
//  3. Configure a matching Workload Identity Provider in the CertForge UI
//     (Settings → Workload Identity) with the same audience and the cluster's
//     OIDC issuer URL.
//
// The Helm chart adds the required projected ServiceAccount volume and mount
// when workloadIdentity.enabled=true; no manual volume wiring is needed.
type WorkloadIdentitySpec struct {
	// Audience is the intended audience for the projected ServiceAccount token.
	// Must match the audience configured in the CertForge Workload Identity Provider.
	// Use your CertForge server URL, e.g. https://app.certgovernance.app.
	// +kubebuilder:validation:Required
	Audience string `json:"audience"`

	// TokenFile is the path to the projected ServiceAccount token file inside
	// the controller pod. Defaults to /var/run/secrets/certforge/token.
	// The Helm chart mounts the token here automatically.
	// +optional
	// +kubebuilder:default="/var/run/secrets/certforge/token"
	TokenFile string `json:"tokenFile,omitempty"`
}

// ─── CertForgeIssuer ─────────────────────────────────────────────────────────

// CertForgeIssuer is a namespace-scoped issuer that talks to a CertForge server.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="URL",type="string",JSONPath=".spec.url"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type CertForgeIssuer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CertForgeIssuerSpec   `json:"spec,omitempty"`
	Status CertForgeIssuerStatus `json:"status,omitempty"`
}

// CertForgeIssuerSpec defines the desired state of CertForgeIssuer.
//
// Exactly one of authSecretRef or workloadIdentity must be set to provide
// credentials. authSecretRef uses a static long-lived token stored in a
// Kubernetes Secret. workloadIdentity uses a short-lived projected
// ServiceAccount token — no Secret required.
type CertForgeIssuerSpec struct {
	// URL is the base URL of the CertForge server.
	// Use https://app.certgovernance.app for US East or https://eu.certgovernance.app for EU West.
	// The URL (combined with the API token) determines which org and data region requests are routed to.
	// +kubebuilder:validation:Required
	URL string `json:"url"`

	// AuthSecretRef references a Secret containing a "token" key with the CertForge API bearer token.
	// For CertForgeIssuer the Secret must be in the same namespace as the issuer.
	// For CertForgeClusterIssuer the Secret is read from SecretNamespace (default: certforge-system).
	// Mutually exclusive with workloadIdentity; exactly one must be set.
	// +optional
	AuthSecretRef *corev1.LocalObjectReference `json:"authSecretRef,omitempty"`

	// WorkloadIdentity configures authentication using a projected ServiceAccount token.
	// The kubelet rotates the token automatically — no long-lived Secret is required.
	// Requires a matching Workload Identity Provider configured in the CertForge UI.
	// Mutually exclusive with authSecretRef; exactly one must be set.
	// +optional
	WorkloadIdentity *WorkloadIdentitySpec `json:"workloadIdentity,omitempty"`

	// IssuanceProfileID is the optional default issuance profile ID for certificate requests
	// routed through this issuer. Overrides the DTP default. Can be overridden per Certificate
	// via the certforge.io/issuance-profile annotation.
	// +optional
	IssuanceProfileID string `json:"issuanceProfileID,omitempty"`

	// SecretNamespace overrides the namespace used to read the authSecretRef Secret when
	// the issuer kind is CertForgeClusterIssuer. Defaults to "certforge-system".
	// Has no effect on namespace-scoped CertForgeIssuer resources.
	// +optional
	SecretNamespace string `json:"secretNamespace,omitempty"`
}

// CertForgeIssuerStatus defines the observed state of CertForgeIssuer.
type CertForgeIssuerStatus struct {
	// Conditions contains the current status conditions.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// CertForgeIssuerList contains a list of CertForgeIssuer.
// +kubebuilder:object:root=true
type CertForgeIssuerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CertForgeIssuer `json:"items"`
}

// ─── CertForgeClusterIssuer ──────────────────────────────────────────────────

// CertForgeClusterIssuer is a cluster-scoped issuer that talks to a CertForge server.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="URL",type="string",JSONPath=".spec.url"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type CertForgeClusterIssuer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CertForgeIssuerSpec   `json:"spec,omitempty"`
	Status CertForgeIssuerStatus `json:"status,omitempty"`
}

// CertForgeClusterIssuerList contains a list of CertForgeClusterIssuer.
// +kubebuilder:object:root=true
type CertForgeClusterIssuerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CertForgeClusterIssuer `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&CertForgeIssuer{}, &CertForgeIssuerList{},
		&CertForgeClusterIssuer{}, &CertForgeClusterIssuerList{},
	)
}

// DeepCopyObject implementations (required by runtime.Object).

func (in *CertForgeIssuer) DeepCopyObject() runtime.Object {
	out := &CertForgeIssuer{}
	in.DeepCopyInto(out)
	return out
}
func (in *CertForgeIssuer) DeepCopyInto(out *CertForgeIssuer) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *CertForgeIssuerSpec) DeepCopyInto(out *CertForgeIssuerSpec) {
	*out = *in
	if in.AuthSecretRef != nil {
		ref := *in.AuthSecretRef
		out.AuthSecretRef = &ref
	}
	if in.WorkloadIdentity != nil {
		wi := *in.WorkloadIdentity
		out.WorkloadIdentity = &wi
	}
}

func (in *CertForgeIssuerList) DeepCopyObject() runtime.Object {
	out := &CertForgeIssuerList{}
	in.DeepCopyInto(out)
	return out
}
func (in *CertForgeIssuerList) DeepCopyInto(out *CertForgeIssuerList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]CertForgeIssuer, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *CertForgeClusterIssuer) DeepCopyObject() runtime.Object {
	out := &CertForgeClusterIssuer{}
	in.DeepCopyInto(out)
	return out
}
func (in *CertForgeClusterIssuer) DeepCopyInto(out *CertForgeClusterIssuer) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *CertForgeClusterIssuerList) DeepCopyObject() runtime.Object {
	out := &CertForgeClusterIssuerList{}
	in.DeepCopyInto(out)
	return out
}
func (in *CertForgeClusterIssuerList) DeepCopyInto(out *CertForgeClusterIssuerList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]CertForgeClusterIssuer, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *CertForgeIssuerStatus) DeepCopyInto(out *CertForgeIssuerStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		copy(out.Conditions, in.Conditions)
	}
}
