package accountaccess

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	accountv1 "github.com/openshift-online/rosa-seaplane-api/api/accountaccess/v1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
)

var (
	ErrInvalidInput  = errors.New("invalid account-access input")
	ErrConflict      = errors.New("account-access assignment or configuration changed")
	ErrSetupRequired = errors.New("installer-role setup is required")
	ErrCorruptRecord = errors.New("invalid stored account-access record")
	accountPattern   = regexp.MustCompile(`^[0-9]{12}$`)
	prefixPattern    = regexp.MustCompile(`^[A-Za-z0-9_+=,.@-]+$`)
	revisionPattern  = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

const InstallerRoleSuffix = "-HCP-ROSA-Installer-Role"

// AWSAccountAccess is an environment/account association, not a Kubernetes resource.
type AWSAccountAccess struct {
	AssignmentID                   string                          `json:"assignmentId"`
	ResourceVersion                string                          `json:"resourceVersion"`
	AccountID                      string                          `json:"accountId"`
	EnvironmentID                  string                          `json:"environmentId"`
	Prefix                         string                          `json:"prefix"`
	Path                           string                          `json:"path"`
	InstallerRoleARN               string                          `json:"installerRoleArn"`
	State                          accountv1.AWSAccountAccessState `json:"state"`
	ValidatedConfigurationRevision string                          `json:"validatedConfigurationRevision"`
}

// DeriveRole validates the customer-selected name/path and binds the role to an
// independently verified owner. Neither role ARN nor ExternalId is customer input.
func DeriveRole(account, prefix, path string) (string, string, error) {
	if !accountPattern.MatchString(account) {
		return "", "", fmt.Errorf("%w: account must contain 12 digits", ErrInvalidInput)
	}
	if !prefixPattern.MatchString(prefix) || len(prefix)+len(InstallerRoleSuffix) > 64 {
		return "", "", fmt.Errorf("%w: prefix must contain 1-%d IAM role-name characters", ErrInvalidInput, 64-len(InstallerRoleSuffix))
	}
	if path == "" || path == "//" || len(path) > 512 || path[0] != '/' || path[len(path)-1] != '/' {
		return "", "", fmt.Errorf("%w: IAM path must start and end with / and contain at most 512 bytes", ErrInvalidInput)
	}
	for _, c := range path {
		if c < 0x21 || c > 0x7e || c == '*' || c == '?' {
			return "", "", fmt.Errorf("%w: IAM path must contain printable ASCII without wildcards", ErrInvalidInput)
		}
	}
	name := prefix + InstallerRoleSuffix
	return name, "arn:aws:iam::" + account + ":role" + path + name, nil
}

// Validate binds every read and AWS operation to the deployment and verified owner.
func (r AWSAccountAccess) Validate(environment, account string) error {
	_, expectedARN, err := DeriveRole(account, r.Prefix, r.Path)
	if err != nil || r.AccountID != account || r.EnvironmentID != environment || r.InstallerRoleARN != expectedARN || !ValidVersion(r.AssignmentID) || !ValidVersion(r.ResourceVersion) {
		return ErrCorruptRecord
	}
	switch r.State {
	case accountv1.AWSAccountAccessPending:
		if r.ValidatedConfigurationRevision != "" {
			return ErrCorruptRecord
		}
	case accountv1.AWSAccountAccessReady:
		if !revisionPattern.MatchString(r.ValidatedConfigurationRevision) {
			return ErrCorruptRecord
		}
	default:
		return ErrCorruptRecord
	}
	return nil
}

func ValidVersion(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

func (r AWSAccountAccess) Response() accountv1.AWSAccountAccessResponse {
	return accountv1.AWSAccountAccessResponse{AssignmentID: r.AssignmentID, ResourceVersion: r.ResourceVersion, State: r.State}
}

// RequiredTags identifies a shared managed role, without exclusive environment ownership.
func RequiredTags() map[string]string {
	return map[string]string{"red-hat-managed": "true", "seaplane-access": "true"}
}

// ConfigurationRevision is stable across regions and YAML formatting. It includes
// the environment and table location as well as all approved IAM configuration.
func ConfigurationRevision(cfg config.InstallerRoleConfig) string {
	content, _ := json.Marshal(struct {
		Config config.InstallerRoleConfig
		Tags   map[string]string
	}{cfg, RequiredTags()})
	return fmt.Sprintf("%x", sha256.Sum256(content))
}

func RoleName(r AWSAccountAccess) string { return r.Prefix + InstallerRoleSuffix }

// TrustPolicy returns the complete initial trust, or the statement to merge into
// compatible existing trust. Existing statements for other environments survive.
func TrustPolicy(jump, account string) json.RawMessage {
	policy, _ := json.Marshal(map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{map[string]any{
			"Effect": "Allow", "Principal": map[string]string{"AWS": jump}, "Action": "sts:AssumeRole",
			"Condition": map[string]any{"StringEquals": map[string]string{"sts:ExternalId": account}},
		}},
	})
	return policy
}

func recordKey(environment, account string) string {
	return strings.Join([]string{"ENV", environment, "ACCOUNT", account}, "#")
}
