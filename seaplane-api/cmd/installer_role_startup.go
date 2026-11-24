package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
	"github.com/spf13/cobra"
)

func loadInstallerRoleConfig(cmd *cobra.Command) (*config.InstallerRoleConfig, error) {
	path, err := cmd.Flags().GetString("installer-role-config-file")
	if err != nil {
		return nil, err
	}
	if !cmd.Flags().Changed("installer-role-config-file") {
		path = os.Getenv("INSTALLER_ROLE_CONFIG_FILE")
	}
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("--installer-role-config-file or INSTALLER_ROLE_CONFIG_FILE is required on this PoC branch")
	}
	cfg, err := config.LoadInstallerRoleConfig(path)
	if err != nil {
		return nil, fmt.Errorf("failed to load installer-role configuration: %w", err)
	}
	return cfg, nil
}
