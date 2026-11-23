package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
	"github.com/spf13/cobra"
)

func loadInstallerRoleConfig(cmd *cobra.Command) (*config.InstallerRoleConfig, error) {
	enabled, err := cmd.Flags().GetBool("installer-role-enabled")
	if err != nil {
		return nil, err
	}
	if !cmd.Flags().Changed("installer-role-enabled") {
		if value, exists := os.LookupEnv("INSTALLER_ROLE_ENABLED"); exists {
			enabled, err = strconv.ParseBool(value)
			if err != nil {
				return nil, fmt.Errorf("INSTALLER_ROLE_ENABLED must be a boolean: %w", err)
			}
		}
	}
	if !enabled {
		return nil, nil
	}
	path, err := cmd.Flags().GetString("installer-role-config-file")
	if err != nil {
		return nil, err
	}
	if !cmd.Flags().Changed("installer-role-config-file") {
		path = os.Getenv("INSTALLER_ROLE_CONFIG_FILE")
	}
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("--installer-role-config-file or INSTALLER_ROLE_CONFIG_FILE is required when installer roles are enabled")
	}
	cfg, err := config.LoadInstallerRoleConfig(path)
	if err != nil {
		return nil, fmt.Errorf("failed to load installer-role configuration: %w", err)
	}
	return cfg, nil
}
