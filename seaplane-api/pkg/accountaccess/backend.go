// Package accountaccess contains the installer-role PoC's account access dependencies.
package accountaccess

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
)

// Backend holds an approved configuration snapshot and the workload's dedicated
// table client. Store operations must use Config.AccountAccess.TableARN, including
// for a table in another account. Customer role credentials never access this table.
type Backend struct {
	Config   config.InstallerRoleConfig
	DynamoDB *dynamodb.Client
}

// NewBackend preserves the workload's AWS settings and credentials. Only the
// DynamoDB region changes; the original config remains the service/discovery config.
func NewBackend(workload aws.Config, cfg config.InstallerRoleConfig) (*Backend, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	tableConfig := workload.Copy()
	tableConfig.Region = cfg.AccountAccess.TableRegion
	return &Backend{Config: cfg, DynamoDB: dynamodb.NewFromConfig(tableConfig)}, nil
}
