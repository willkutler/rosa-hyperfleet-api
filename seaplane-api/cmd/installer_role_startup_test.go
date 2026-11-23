package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallerRoleStartup(t *testing.T) {
	valid, err := filepath.Abs("../pkg/config/testdata/installer-role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	invalid := startupFile(t, "formatVersion: 2\n")
	for _, tc := range []struct {
		name, enabled, file, wantError string
		args                           []string
		wantEnabled                    bool
	}{
		{name: "disabled by default"},
		{name: "disabled ignores invalid file", enabled: "false", file: invalid},
		{name: "file alone does not enable", file: valid},
		{name: "enabled requires file", enabled: "true", wantError: "INSTALLER_ROLE_CONFIG_FILE"},
		{name: "enabled missing file", enabled: "true", file: filepath.Join(t.TempDir(), "missing"), wantError: "load installer-role"},
		{name: "enabled invalid config", enabled: "true", file: invalid, wantError: "formatVersion"},
		{name: "enabled valid config", enabled: "true", file: valid, wantEnabled: true},
		{name: "invalid enabled env", enabled: "typo", file: valid, wantError: "must be a boolean"},
		{name: "flag enables", enabled: "false", file: valid, args: []string{"--installer-role-enabled"}, wantEnabled: true},
		{name: "flag disables", enabled: "true", file: invalid, args: []string{"--installer-role-enabled=false"}},
		{name: "flag beats invalid enabled env", enabled: "typo", file: valid, args: []string{"--installer-role-enabled"}, wantEnabled: true},
		{name: "file flag overrides env", enabled: "true", file: invalid, args: []string{"--installer-role-config-file=" + valid}, wantEnabled: true},
		{name: "empty file flag overrides env", enabled: "true", file: valid, args: []string{"--installer-role-config-file="}, wantError: "INSTALLER_ROLE_CONFIG_FILE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.enabled == "" {
				t.Setenv("INSTALLER_ROLE_ENABLED", "")
				if err := os.Unsetenv("INSTALLER_ROLE_ENABLED"); err != nil {
					t.Fatal(err)
				}
			} else {
				t.Setenv("INSTALLER_ROLE_ENABLED", tc.enabled)
			}
			t.Setenv("INSTALLER_ROLE_CONFIG_FILE", tc.file)
			t.Setenv("AUTHZ_CONFIG_FILE", startupFile(t, startupBundle))
			cfg, authorizer, err := loadStartupConfig(startupCommand(t, tc.args...), "us-east-1")
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) || cfg != nil || authorizer != nil {
					t.Fatalf("want error %q and no dependencies, got config=%+v, authorizer=%v, err=%v", tc.wantError, cfg, authorizer, err)
				}
				return
			}
			if err != nil || cfg == nil || (cfg.InstallerRole != nil) != tc.wantEnabled || cfg.Regional.AWSRegion != "us-east-1" {
				t.Fatalf("unexpected startup config: %+v, %v", cfg, err)
			}
		})
	}
}

func TestInvalidInstallerRoleConfigStopsServeBeforeDatabase(t *testing.T) {
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AUTHZ_CONFIG_FILE", startupFile(t, startupBundle))
	t.Setenv("INSTALLER_ROLE_ENABLED", "true")
	t.Setenv("INSTALLER_ROLE_CONFIG_FILE", startupFile(t, "formatVersion: 2\n"))
	t.Setenv("POSTGRES_DSN", "")
	if err := runServe(startupCommand(t), nil); err == nil || !strings.Contains(err.Error(), "formatVersion") {
		t.Fatalf("expected configuration failure before database setup, got %v", err)
	}
}
