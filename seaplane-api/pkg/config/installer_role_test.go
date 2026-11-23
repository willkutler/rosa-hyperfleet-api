package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadInstallerRoleConfig(t *testing.T) {
	content, err := os.ReadFile("testdata/installer-role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	valid := string(content)
	for _, tc := range []struct {
		name, content, wantError string
	}{
		{"valid", valid, ""},
		{"empty", "", "EOF"},
		{"null", "null", "object"},
		{"sequence", "[]", "object"},
		{"syntax", "formatVersion: [", "decode"},
		{"unknown field", valid + "externalId: override\n", "field externalId"},
		{"unknown nested field", valid + "  tableName: override\n", "field tableName"},
		{"duplicate", valid + "formatVersion: 1\n", "already defined"},
		{"duplicate nested", valid + "  tableRegion: us-east-1\n", "already defined"},
		{"multiple documents", valid + "---\n{}\n", "exactly one"},
		{"empty extra document", valid + "---\n", "exactly one"},
		{"trailing syntax", valid + "---\n[", "exactly one"},
		{"format version", strings.Replace(valid, "formatVersion: 1", "formatVersion: 2", 1), "formatVersion"},
		{"string format version", strings.Replace(valid, "formatVersion: 1", `formatVersion: "1"`, 1), "YAML type"},
		{"template version", strings.Replace(valid, `trustTemplateVersion: "1"`, `trustTemplateVersion: "2"`, 1), "trustTemplateVersion"},
		{"numeric template version", strings.Replace(valid, `trustTemplateVersion: "1"`, "trustTemplateVersion: 1", 1), "YAML type"},
		{"null table config", strings.Replace(valid, "accountAccess:\n", "accountAccess: null\nignored:\n", 1), "object"},
		{"numeric environment", strings.Replace(valid, "environmentId: ephemeral-example-123", "environmentId: 123", 1), "YAML type"},
		{"environment separator", strings.Replace(valid, "environmentId: ephemeral-example-123", "environmentId: 'ENV#ACCOUNT#123'", 1), "environmentId"},
		{"environment whitespace", strings.Replace(valid, "environmentId: ephemeral-example-123", "environmentId: ' example '", 1), "environmentId"},
		{"long environment", strings.Replace(valid, "ephemeral-example-123\n", strings.Repeat("a", 129)+"\n", 1), "environmentId"},
		{"jump user", strings.Replace(valid, ":role/platform/", ":user/platform/", 1), "jumpRoleArn"},
		{"jump session", strings.Replace(valid, "iam::123456789012:role/platform", "sts::123456789012:assumed-role/platform", 1), "jumpRoleArn"},
		{"jump wildcard name", strings.Replace(valid, ":role/platform/ephemeral-example-123-jump", ":role/platform/*", 1), "jumpRoleArn"},
		{"jump wildcard path", strings.Replace(valid, ":role/platform/", ":role/*/", 1), "jumpRoleArn"},
		{"jump account", strings.Replace(valid, "iam::123456789012:", "iam::1234:", 1), "jumpRoleArn"},
		{"jump region", strings.Replace(valid, "iam::123456789012:", "iam:us-east-1:123456789012:", 1), "jumpRoleArn"},
		{"jump partition", strings.Replace(valid, "arn:aws:iam::123", "arn:aws-us-gov:iam::123", 1), "jumpRoleArn"},
		{"unapproved policy", strings.Replace(valid, "service-role/ROSAInstallerPolicy", "AdministratorAccess", 1), "installerManagedPolicyArn"},
		{"customer policy", strings.Replace(valid, "iam::aws:policy/", "iam::123456789012:policy/", 1), "installerManagedPolicyArn"},
		{"region mismatch", strings.Replace(valid, "tableRegion: us-west-2", "tableRegion: us-east-1", 1), "must match"},
		{"invalid region", strings.ReplaceAll(valid, "us-west-2", "invalid"), "tableRegion"},
		{"table name only", strings.Replace(valid, "arn:aws:dynamodb:us-west-2:210987654321:table/example-account-access", "example-account-access", 1), "tableArn"},
		{"table index", strings.Replace(valid, "table/example-account-access", "table/example-account-access/index/example", 1), "tableArn"},
		{"table wildcard", strings.Replace(valid, "table/example-account-access", "table/*", 1), "tableArn"},
		{"table account", strings.Replace(valid, "210987654321", "123", 1), "tableArn"},
		{"table partition", strings.Replace(valid, "arn:aws:dynamodb", "arn:aws-cn:dynamodb", 1), "tableArn"},
		{"anchor", strings.Replace(valid, "environmentId: ephemeral", "environmentId: &env ephemeral", 1), "anchors"},
		{"merge", valid + "<<: {}\n", "key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadInstallerRoleConfig(path)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) || cfg != nil {
					t.Fatalf("want %q and nil config, got %+v, %v", tc.wantError, cfg, err)
				}
				return
			}
			if err != nil || cfg == nil || cfg.AccountAccess.TableRegion != "us-west-2" || cfg.EnvironmentID != "ephemeral-example-123" {
				t.Fatalf("unexpected config %+v, %v", cfg, err)
			}
		})
	}
	for _, line := range strings.Split(strings.TrimSpace(valid), "\n") {
		if line == "accountAccess:" {
			continue
		}
		t.Run("missing "+strings.Split(strings.TrimSpace(line), ":")[0], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "missing.yaml")
			if err := os.WriteFile(path, []byte(strings.Replace(valid, line+"\n", "", 1)), 0600); err != nil {
				t.Fatal(err)
			}
			if cfg, err := LoadInstallerRoleConfig(path); err == nil || cfg != nil {
				t.Fatalf("missing field accepted: %+v, %v", cfg, err)
			}
		})
	}
	if cfg, err := LoadInstallerRoleConfig(filepath.Join(t.TempDir(), "absent")); err == nil || cfg != nil {
		t.Fatal("missing file accepted")
	}
}
