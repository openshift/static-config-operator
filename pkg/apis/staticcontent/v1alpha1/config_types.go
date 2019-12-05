package v1alpha1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConfigSpec defines the desired state of Config
// +k8s:openapi-gen=true
type ConfigSpec struct {
	Config      StaticConfig   `json:"config,omitempty"`
	SyncPeriond *time.Duration `json:"syncDuration,omitempty"`
	Platform    string         `json:"platform,omitempty"`
}

// StaticConfig defines the desired state of Config
// TODO: Split to cloud specific configs
// +k8s:openapi-gen=true
type StaticConfig struct {
	TelemeterServerURL string `json:"telemeterServerURL,omitempty"`

	// identity provider config
	IdentityAttrEmail             string `json:"identityAttrEmail,omitempty"`
	IdentityAttrID                string `json:"identityAttrID,omitempty"`
	IdentityAttrName              string `json:"identityAttrName,omitempty"`
	IdentityAttrPreferredUsername string `json:"identityAttrPreferredUsername,omitempty"`
	IdentityBindName              string `json:"identityBindName,omitempty"`
	IdentityURL                   string `json:"identityURL,omitempty"`
	IdentityName                  string `json:"identityName,omitempty"`
	IdentityMappingMethod         string `json:"identityMappingMethod,omitempty"`

	// other configuration values
	OSDLdapCA           string `json:"osdLdapCA,omitempty"`
	ValeroOperatorImage string `json:"valeroOperatorImage,omitempty"`
}

// ConfigStatus defines the observed state of Config
// +k8s:openapi-gen=true
type ConfigStatus struct{}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Config is the Schema for the configs API
// +k8s:openapi-gen=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=configs,scope=Namespaced
type Config struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ConfigSpec   `json:"spec,omitempty"`
	Status ConfigStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ConfigList contains a list of Config
type ConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Config `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Config{}, &ConfigList{})
}
