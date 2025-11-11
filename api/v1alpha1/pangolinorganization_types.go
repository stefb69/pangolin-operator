package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PangolinOrganizationSpec defines the desired state of PangolinOrganization
type PangolinOrganizationSpec struct {
	// Pangolin API configuration
	// +kubebuilder:validation:Required
	APIEndpoint string `json:"apiEndpoint"`

	// API key reference (organization-scoped)
	// +kubebuilder:validation:Required
	APIKeyRef corev1.SecretKeySelector `json:"apiKeyRef"`

	// BINDING MODE: Organization ID to bind to existing org
	// If provided, binds to existing org instead of discovering
	OrganizationID string `json:"organizationId,omitempty"`

	// Display name (used for new orgs, updated from API for existing)
	DisplayName string `json:"displayName,omitempty"`

	// Default configuration for tunnels in this org
	Defaults *OrganizationDefaults `json:"defaults,omitempty"`
}

// OrganizationDefaults defines default settings for tunnels
type OrganizationDefaults struct {
	// Default site type for tunnels
	// +kubebuilder:validation:Enum=newt;wireguard;local
	// +kubebuilder:default="newt"
	SiteType string `json:"siteType,omitempty"`

	// Default Newt client configuration
	NewtClient *NewtClientSpec `json:"newtClient,omitempty"`

	// Default domain for HTTP resources - NEW: can be domain name or domainId
	DefaultDomain string `json:"defaultDomain,omitempty"`
}

// Domain represents a Pangolin domain
type Domain struct {
	// Domain ID from Pangolin API
	DomainID string `json:"domainId"`

	// Base domain (e.g., "yourdomain.com")
	BaseDomain string `json:"baseDomain"`

	// Whether the domain is verified
	Verified bool `json:"verified"`

	// Domain type (e.g., "wildcard")
	Type string `json:"type"`

	// Whether domain verification failed
	Failed bool `json:"failed"`

	// Number of verification tries
	Tries int `json:"tries"`

	// Whether config is managed
	ConfigManaged bool `json:"configManaged"`
}

// PangolinOrganizationStatus defines the observed state
type PangolinOrganizationStatus struct {
	// Organization ID from Pangolin API (discovered or confirmed)
	OrganizationID string `json:"organizationId,omitempty"`

	// Organization name from API
	OrganizationName string `json:"organizationName,omitempty"`

	// Network subnet for this org from API
	Subnet string `json:"subnet,omitempty"`

	// Available domains for this organization
	Domains []Domain `json:"domains,omitempty"`

	// Default domain ID resolved from spec.defaults.defaultDomain
	DefaultDomainID string `json:"defaultDomainId,omitempty"`

	// Binding mode: "Discovered" (auto-discovered) or "Bound" (explicitly bound)
	BindingMode string `json:"bindingMode,omitempty"`

	// Current status: Discovering, Binding, Ready, Error
	// +kubebuilder:validation:Enum=Discovering;Binding;Ready;Error
	Status string `json:"status,omitempty"`

	// Conditions and timestamps
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:shortName=porg
//+kubebuilder:printcolumn:name="Org ID",type=string,JSONPath=`.status.organizationId`
//+kubebuilder:printcolumn:name="Org Name",type=string,JSONPath=`.status.organizationName`
//+kubebuilder:printcolumn:name="Domains",type=integer,JSONPath=`.status.domains[*].domainId`
//+kubebuilder:printcolumn:name="Default Domain",type=string,JSONPath=`.status.defaultDomainId`
//+kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.status`
//+kubebuilder:printcolumn:name="Binding Mode",type=string,JSONPath=`.status.bindingMode`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PangolinOrganization is the Schema for the pangolinorganizations API
type PangolinOrganization struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PangolinOrganizationSpec   `json:"spec,omitempty"`
	Status PangolinOrganizationStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// PangolinOrganizationList contains a list of PangolinOrganization
type PangolinOrganizationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PangolinOrganization `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PangolinOrganization{}, &PangolinOrganizationList{})
}
