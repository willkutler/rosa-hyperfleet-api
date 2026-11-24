package main

import (
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
		name, file, wantError string
		args                  []string
	}{
		{name: "requires file", wantError: "INSTALLER_ROLE_CONFIG_FILE"},
		{name: "missing file", file: filepath.Join(t.TempDir(), "missing"), wantError: "load installer-role"},
		{name: "invalid config", file: invalid, wantError: "formatVersion"},
		{name: "valid config", file: valid},
		{name: "file flag overrides env", file: invalid, args: []string{"--installer-role-config-file=" + valid}},
		{name: "empty file flag overrides env", file: valid, args: []string{"--installer-role-config-file="}, wantError: "INSTALLER_ROLE_CONFIG_FILE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("INSTALLER_ROLE_CONFIG_FILE", tc.file)
			t.Setenv("AUTHZ_CONFIG_FILE", startupFile(t, startupBundle))
			cfg, authorizer, err := loadStartupConfig(startupCommand(t, tc.args...), "us-east-1")
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) || cfg != nil || authorizer != nil {
					t.Fatalf("want error %q and no dependencies, got config=%+v, authorizer=%v, err=%v", tc.wantError, cfg, authorizer, err)
				}
				return
			}
			if err != nil || cfg == nil || cfg.InstallerRole == nil || cfg.Regional.AWSRegion != "us-east-1" {
				t.Fatalf("unexpected startup config: %+v, %v", cfg, err)
			}
		})
	}
}

func TestInvalidInstallerRoleConfigStopsServeBeforeDatabase(t *testing.T) {
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AUTHZ_CONFIG_FILE", startupFile(t, startupBundle))
	t.Setenv("INSTALLER_ROLE_CONFIG_FILE", startupFile(t, "formatVersion: 2\n"))
	t.Setenv("POSTGRES_DSN", "")
	if err := runServe(startupCommand(t), nil); err == nil || !strings.Contains(err.Error(), "formatVersion") {
		t.Fatalf("expected configuration failure before database setup, got %v", err)
	}
}
