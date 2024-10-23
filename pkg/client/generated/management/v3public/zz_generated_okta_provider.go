package client

const (
	OKTAProviderType                    = "oktaProvider"
	OKTAProviderFieldAnnotations        = "annotations"
	OKTAProviderFieldCreated            = "created"
	OKTAProviderFieldCreatorID          = "creatorId"
	OKTAProviderFieldLabels             = "labels"
	OKTAProviderFieldLogoutAllEnabled   = "logoutAllEnabled"
	OKTAProviderFieldLogoutAllForced    = "logoutAllForced"
	OKTAProviderFieldLogoutAllSupported = "logoutAllSupported"
	OKTAProviderFieldName               = "name"
	OKTAProviderFieldOwnerReferences    = "ownerReferences"
	OKTAProviderFieldRedirectURL        = "redirectUrl"
	OKTAProviderFieldRemoved            = "removed"
	OKTAProviderFieldType               = "type"
	OKTAProviderFieldUUID               = "uuid"
)

type OKTAProvider struct {
	Annotations        map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`
	Created            string            `json:"created,omitempty" yaml:"created,omitempty"`
	CreatorID          string            `json:"creatorId,omitempty" yaml:"creatorId,omitempty"`
	Labels             map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	LogoutAllEnabled   bool              `json:"logoutAllEnabled,omitempty" yaml:"logoutAllEnabled,omitempty"`
	LogoutAllForced    bool              `json:"logoutAllForced,omitempty" yaml:"logoutAllForced,omitempty"`
	LogoutAllSupported bool              `json:"logoutAllSupported,omitempty" yaml:"logoutAllSupported,omitempty"`
	Name               string            `json:"name,omitempty" yaml:"name,omitempty"`
	OwnerReferences    []OwnerReference  `json:"ownerReferences,omitempty" yaml:"ownerReferences,omitempty"`
	RedirectURL        string            `json:"redirectUrl,omitempty" yaml:"redirectUrl,omitempty"`
	Removed            string            `json:"removed,omitempty" yaml:"removed,omitempty"`
	Type               string            `json:"type,omitempty" yaml:"type,omitempty"`
	UUID               string            `json:"uuid,omitempty" yaml:"uuid,omitempty"`
}
