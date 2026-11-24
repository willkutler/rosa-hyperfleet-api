// Package awsaccess resolves customer credentials through the configured jump role.
// It is independent of HTTP, enrollment, and Cedar authorization.
package awsaccess

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/google/uuid"
	accountv1 "github.com/openshift-online/rosa-seaplane-api/api/accountaccess/v1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/accountaccess"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
)

var (
	ErrAssumeRole = errors.New("customer role assumption failed")
	ErrValidation = errors.New("installer role validation failed")
	ErrDiscovery  = errors.New("customer VPC discovery failed")
)

type STSAPI interface {
	AssumeRole(context.Context, *sts.AssumeRoleInput, ...func(*sts.Options)) (*sts.AssumeRoleOutput, error)
}

type IAMAPI interface {
	GetRole(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	ListAttachedRolePolicies(context.Context, *iam.ListAttachedRolePoliciesInput, ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error)
}

type EC2API interface {
	DescribeVpcs(context.Context, *ec2.DescribeVpcsInput, ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error)
}

// Clients permits fake AWS clients while exercising the real credential providers.
type Clients struct {
	STS func(aws.Config) STSAPI
	IAM func(aws.Config) IAMAPI
	EC2 func(aws.Config) EC2API
}

type Access struct {
	workload aws.Config
	config   config.InstallerRoleConfig
	clients  Clients
}

func New(workload aws.Config, cfg config.InstallerRoleConfig) (*Access, error) {
	return NewWithClients(workload, cfg, Clients{
		STS: func(c aws.Config) STSAPI { return sts.NewFromConfig(c) },
		IAM: func(c aws.Config) IAMAPI { return iam.NewFromConfig(c) },
		EC2: func(c aws.Config) EC2API { return ec2.NewFromConfig(c) },
	})
}

func NewWithClients(workload aws.Config, cfg config.InstallerRoleConfig, clients Clients) (*Access, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if workload.Region == "" || clients.STS == nil || clients.IAM == nil || clients.EC2 == nil {
		return nil, fmt.Errorf("service region and AWS client factories are required")
	}
	return &Access{workload: workload.Copy(), config: cfg, clients: clients}, nil
}

func assumeOptions(externalID *string) func(*stscreds.AssumeRoleOptions) {
	return func(o *stscreds.AssumeRoleOptions) {
		o.RoleSessionName = "seaplane-" + uuid.NewString()
		o.Duration = time.Hour // Maximum for role chaining.
		o.ExternalID = externalID
	}
}

// customer constructs fresh, refreshable providers per request. The verified
// account determines ExternalId; the target ARN can never select its own owner.
func (a *Access) customer(ctx context.Context, record accountaccess.AWSAccountAccess, account string, ready bool) (aws.Config, STSAPI, error) {
	if err := record.Validate(a.config.EnvironmentID, account); err != nil {
		return aws.Config{}, nil, err
	}
	if ready && (record.State != accountv1.AWSAccountAccessReady || record.ValidatedConfigurationRevision != accountaccess.ConfigurationRevision(a.config)) {
		return aws.Config{}, nil, accountaccess.ErrSetupRequired
	}
	jump := a.workload.Copy()
	jump.Credentials = aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(a.clients.STS(a.workload.Copy()), a.config.JumpRoleARN, assumeOptions(nil)))
	if _, err := jump.Credentials.Retrieve(ctx); err != nil {
		return aws.Config{}, nil, fmt.Errorf("%w: jump hop: %w", ErrAssumeRole, err)
	}
	jumpSTS := a.clients.STS(jump)
	customer := a.workload.Copy()
	customer.Credentials = aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(jumpSTS, record.InstallerRoleARN, assumeOptions(aws.String(account))))
	if _, err := customer.Credentials.Retrieve(ctx); err != nil {
		return aws.Config{}, nil, fmt.Errorf("%w: installer hop: %w", ErrAssumeRole, err)
	}
	return customer, jumpSTS, nil
}

// Validate permits Pending assignments, inspects shared-role trust, and supplements
// inspection with actual authorization denials for absent/incorrect ExternalIds.
func (a *Access) Validate(ctx context.Context, record accountaccess.AWSAccountAccess, account string) error {
	customer, jumpSTS, err := a.customer(ctx, record, account, false)
	if err != nil {
		return err
	}
	client := a.clients.IAM(customer)
	out, err := client.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(accountaccess.RoleName(record))})
	if err != nil {
		return &ValidationError{Reason: "IAM GetRole failed; retry after IAM changes propagate", Cause: err}
	}
	if out == nil || out.Role == nil || aws.ToString(out.Role.Arn) != record.InstallerRoleARN || aws.ToString(out.Role.Path) != record.Path || aws.ToString(out.Role.RoleName) != accountaccess.RoleName(record) {
		return invalid("IAM role identity/path differs from the assignment")
	}
	tags := map[string]string{}
	for _, tag := range out.Role.Tags {
		key := aws.ToString(tag.Key)
		if _, exists := tags[key]; exists {
			return invalid("duplicate role tags")
		}
		tags[key] = aws.ToString(tag.Value)
	}
	for key, value := range accountaccess.RequiredTags() {
		if tags[key] != value {
			return invalid("required managed-role tag " + key + " is missing or incompatible")
		}
	}
	if err := ValidateTrust(aws.ToString(out.Role.AssumeRolePolicyDocument), a.config.JumpRoleARN, account); err != nil {
		return err
	}
	found := false
	var marker *string
	seen := map[string]bool{}
	for {
		policies, err := client.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{RoleName: aws.String(accountaccess.RoleName(record)), Marker: marker})
		if err != nil {
			return &ValidationError{Reason: "IAM ListAttachedRolePolicies failed", Cause: err}
		}
		if policies == nil {
			return invalid("missing IAM policy response")
		}
		for _, policy := range policies.AttachedPolicies {
			found = found || aws.ToString(policy.PolicyArn) == a.config.InstallerManagedPolicyARN
		}
		if !policies.IsTruncated {
			break
		}
		marker = policies.Marker
		if aws.ToString(marker) == "" || seen[aws.ToString(marker)] {
			return invalid("invalid IAM pagination")
		}
		seen[*marker] = true
	}
	if !found {
		return invalid("ROSAInstallerPolicy is not attached")
	}
	wrongID := "000000000000"
	if account == wrongID {
		wrongID = "111111111111"
	}
	for _, externalID := range []*string{nil, &wrongID} {
		_, err := jumpSTS.AssumeRole(ctx, &sts.AssumeRoleInput{
			RoleArn: aws.String(record.InstallerRoleARN), RoleSessionName: aws.String("seaplane-negative-" + uuid.NewString()),
			DurationSeconds: aws.Int32(3600), ExternalId: externalID,
		})
		if err == nil {
			return invalid("installer role accepted an absent or incorrect ExternalId")
		}
		var denied smithy.APIError
		if !errors.As(err, &denied) || (denied.ErrorCode() != "AccessDenied" && denied.ErrorCode() != "AccessDeniedException") {
			return &ValidationError{Reason: "negative ExternalId check was inconclusive; an actual authorization denial is required", Cause: err}
		}
	}
	return nil
}

func (a *Access) ListVPCs(ctx context.Context, record accountaccess.AWSAccountAccess, account string) ([]accountv1.VPC, error) {
	customer, _, err := a.customer(ctx, record, account, true)
	if err != nil {
		return nil, err
	}
	client := a.clients.EC2(customer) // Region remains the API's region.
	items := []accountv1.VPC{}
	var token *string
	seen := map[string]bool{}
	for {
		out, err := client.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{NextToken: token})
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrDiscovery, err)
		}
		if out == nil {
			return nil, fmt.Errorf("%w: missing EC2 response", ErrDiscovery)
		}
		for _, vpc := range out.Vpcs {
			items = append(items, accountv1.VPC{ID: aws.ToString(vpc.VpcId), CIDRBlock: aws.ToString(vpc.CidrBlock), IsDefault: aws.ToBool(vpc.IsDefault)})
		}
		token = out.NextToken
		if aws.ToString(token) == "" {
			return items, nil
		}
		if seen[*token] {
			return nil, fmt.Errorf("%w: invalid EC2 pagination", ErrDiscovery)
		}
		seen[*token] = true
	}
}
