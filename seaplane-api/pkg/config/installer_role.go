package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"gopkg.in/yaml.v3"
)

const InstallerManagedPolicyARN = "arn:aws:iam::aws:policy/service-role/ROSAInstallerPolicy"

var (
	installerEnvironmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	installerAccountPattern     = regexp.MustCompile(`^[0-9]{12}$`)
	installerRegionPattern      = regexp.MustCompile(`^[a-z]{2}(-[a-z0-9]+)+-[0-9]+$`)
	installerRoleNamePattern    = regexp.MustCompile(`^[A-Za-z0-9_+=,.@-]{1,64}$`)
	installerTablePattern       = regexp.MustCompile(`^table/[A-Za-z0-9_.-]{3,255}$`)
)

// LoadInstallerRoleConfig reads one strict YAML document. Configuration is a
// startup snapshot; the chart's pod checksum handles subsequent changes.
func LoadInstallerRoleConfig(path string) (*InstallerRoleConfig, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode installer-role configuration: %w", err)
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("installer-role configuration must contain exactly one YAML document")
	}
	if len(document.Content) != 1 {
		return nil, fmt.Errorf("installer-role configuration must be an object")
	}
	if err := checkInstallerRoleYAML(document.Content[0], ""); err != nil {
		return nil, err
	}
	decoder = yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	var cfg InstallerRoleConfig
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode installer-role configuration: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Reject YAML coercions, aliases, and merges so approved values are explicit.
func checkInstallerRoleYAML(node *yaml.Node, path string) error {
	if node.Anchor != "" || node.Kind == yaml.AliasNode {
		return fmt.Errorf("YAML anchors and aliases are not supported at %q", path)
	}
	if path == "" || path == "accountAccess" {
		if node.Kind != yaml.MappingNode || node.Tag != "!!map" {
			return fmt.Errorf("%q must be an object", path)
		}
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Anchor != "" || key.Value == "<<" {
				return fmt.Errorf("invalid configuration key at %q", path)
			}
			childPath := key.Value
			if path != "" {
				childPath = path + "." + key.Value
			}
			if err := checkInstallerRoleYAML(node.Content[i+1], childPath); err != nil {
				return err
			}
		}
		return nil
	}
	tag := "!!str"
	if path == "formatVersion" {
		tag = "!!int"
	}
	if node.Kind != yaml.ScalarNode || node.Tag != tag {
		return fmt.Errorf("invalid YAML type for %q; expected %s", path, tag)
	}
	return nil
}

// Validate checks the service-owned v1 configuration without contacting AWS.
// The PoC supports the commercial AWS partition and only ROSAInstallerPolicy.
func (cfg InstallerRoleConfig) Validate() error {
	if cfg.FormatVersion != 1 {
		return fmt.Errorf("unsupported installer-role formatVersion %d; expected 1", cfg.FormatVersion)
	}
	if cfg.TrustTemplateVersion != "1" {
		return fmt.Errorf("unsupported installer-role trustTemplateVersion %q; expected 1", cfg.TrustTemplateVersion)
	}
	if !installerEnvironmentPattern.MatchString(cfg.EnvironmentID) {
		return fmt.Errorf("environmentId must be 1-128 letters, digits, underscores or hyphens, starting with a letter or digit")
	}
	jump, err := arn.Parse(cfg.JumpRoleARN)
	if err != nil || jump.Partition != "aws" || jump.Service != "iam" || jump.Region != "" || !installerAccountPattern.MatchString(jump.AccountID) || !validInstallerJumpRole(jump.Resource) {
		return fmt.Errorf("jumpRoleArn must be a commercial AWS IAM role ARN with a 12-digit account ID")
	}
	if cfg.InstallerManagedPolicyARN != InstallerManagedPolicyARN {
		return fmt.Errorf("installerManagedPolicyArn must be %s", InstallerManagedPolicyARN)
	}
	if !installerRegionPattern.MatchString(cfg.AccountAccess.TableRegion) {
		return fmt.Errorf("accountAccess.tableRegion must be an AWS region")
	}
	table, err := arn.Parse(cfg.AccountAccess.TableARN)
	if err != nil || table.Partition != "aws" || table.Service != "dynamodb" || !installerAccountPattern.MatchString(table.AccountID) || !installerTablePattern.MatchString(table.Resource) {
		return fmt.Errorf("accountAccess.tableArn must be a commercial AWS DynamoDB table ARN")
	}
	if table.Region != cfg.AccountAccess.TableRegion {
		return fmt.Errorf("accountAccess.tableRegion must match the region in accountAccess.tableArn")
	}
	return nil
}

func validInstallerJumpRole(resource string) bool {
	if !strings.HasPrefix(resource, "role/") {
		return false
	}
	pathAndName := strings.TrimPrefix(resource, "role/")
	name := pathAndName
	if i := strings.LastIndexByte(pathAndName, '/'); i >= 0 {
		path := pathAndName[:i+1]
		// IAM paths include both leading and trailing slashes (512 bytes max).
		if len(path)+1 > 512 {
			return false
		}
		for _, c := range path {
			if c < 0x21 || c > 0x7e || c == '*' || c == '?' {
				return false
			}
		}
		name = pathAndName[i+1:]
	}
	return installerRoleNamePattern.MatchString(name)
}
