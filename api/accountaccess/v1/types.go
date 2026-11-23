/*
Copyright (c) 2026 Red Hat, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1

import "encoding/json"

type AWSAccountAccessState string

const (
	AWSAccountAccessPending AWSAccountAccessState = "Pending"
	AWSAccountAccessReady   AWSAccountAccessState = "Ready"
)

// InitializeAWSAccountAccessRequest contains the customer-selected setup values.
type InitializeAWSAccountAccessRequest struct {
	Prefix string `json:"prefix"`
	Path   string `json:"path"`
}

// AWSAccountAccessResponse is shared by initialization and successful validation.
type AWSAccountAccessResponse struct {
	AssignmentID    string                `json:"assignmentId"`
	ResourceVersion string                `json:"resourceVersion"`
	State           AWSAccountAccessState `json:"state"`
}

// InstallerRoleConfigResponse contains everything ROSA needs to display, create,
// and tag the installer role.
type InstallerRoleConfigResponse struct {
	Assignment                AWSAccountAccessResponse `json:"assignment"`
	AccountID                 string                   `json:"accountId"`
	EnvironmentID             string                   `json:"environmentId"`
	RoleName                  string                   `json:"roleName"`
	Path                      string                   `json:"path"`
	InstallerRoleARN          string                   `json:"installerRoleArn"`
	ExternalID                string                   `json:"externalId"`
	JumpRoleARN               string                   `json:"jumpRoleArn"`
	InstallerManagedPolicyARN string                   `json:"installerManagedPolicyArn"`
	TrustPolicy               json.RawMessage          `json:"trustPolicy"`
	RequiredTags              map[string]string        `json:"requiredTags"`
	ConfigurationRevision     string                   `json:"configurationRevision"`
}

// ValidateAWSAccountAccessRequest identifies the assignment and configuration
// ROSA used to create the role.
type ValidateAWSAccountAccessRequest struct {
	AssignmentID          string `json:"assignmentId"`
	ResourceVersion       string `json:"resourceVersion"`
	ConfigurationRevision string `json:"configurationRevision"`
}

type ListVPCsResponse struct {
	Items []VPC `json:"items"`
}

type VPC struct {
	ID        string `json:"id"`
	CIDRBlock string `json:"cidrBlock"`
	IsDefault bool   `json:"isDefault"`
}
