package awsaccess

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/aws/smithy-go"
	"github.com/google/uuid"
	accountv1 "github.com/openshift-online/rosa-seaplane-api/api/accountaccess/v1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/accountaccess"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
)

type stsFunc func(context.Context, *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error)

func (f stsFunc) AssumeRole(c context.Context, in *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	return f(c, in)
}

type awsFixture struct {
	t                    *testing.T
	cfg                  config.InstallerRoleConfig
	record               accountaccess.AWSAccountAccess
	jumpCalls, roleCalls int
	negativeCalls        int
	iamCalls, ec2Calls   int
	negativeOverride     bool
	negativeErr          error
	jumpErr, roleErr     error
	iamErr, ec2Err       error
	policyMissing        bool
	trust                string
	missingTag           bool
}

func fixture(t *testing.T) (*awsFixture, *Access) {
	t.Helper()
	cfg, err := config.LoadInstallerRoleConfig("../config/testdata/installer-role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, roleARN, _ := accountaccess.DeriveRole(customerAccount, "Example", "/custom/")
	f := &awsFixture{t: t, cfg: *cfg, record: accountaccess.AWSAccountAccess{
		AssignmentID: uuid.NewString(), ResourceVersion: uuid.NewString(), AccountID: customerAccount, EnvironmentID: cfg.EnvironmentID,
		Prefix: "Example", Path: "/custom/", InstallerRoleARN: roleARN, State: accountv1.AWSAccountAccessPending,
	}}
	f.trust = string(accountaccess.TrustPolicy(cfg.JumpRoleARN, customerAccount))
	workload := aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("workload", "secret", "")}
	a, err := NewWithClients(workload, *cfg, Clients{
		STS: func(c aws.Config) STSAPI {
			return stsFunc(func(ctx context.Context, in *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error) {
				creds, err := c.Credentials.Retrieve(ctx)
				if err != nil {
					return nil, err
				}
				if aws.ToInt32(in.DurationSeconds) != 3600 || len(aws.ToString(in.RoleSessionName)) > 64 {
					t.Error("invalid session duration/name")
				}
				key := "customer"
				if aws.ToString(in.RoleArn) == cfg.JumpRoleARN {
					f.jumpCalls++
					if creds.AccessKeyID != "workload" || in.ExternalId != nil {
						t.Error("jump hop must use workload credentials without ExternalId")
					}
					if f.jumpErr != nil {
						return nil, f.jumpErr
					}
					key = "jump"
				} else {
					if creds.AccessKeyID != "jump" || aws.ToString(in.RoleArn) != roleARN {
						t.Error("customer hop must use jump credentials and the registered role")
					}
					if aws.ToString(in.ExternalId) != customerAccount {
						f.negativeCalls++
						if !f.negativeOverride {
							return nil, &smithy.GenericAPIError{Code: "AccessDenied", Message: "denied"}
						}
						if f.negativeErr != nil {
							return nil, f.negativeErr
						}
					} else {
						f.roleCalls++
						if f.roleErr != nil {
							return nil, f.roleErr
						}
					}
				}
				return &sts.AssumeRoleOutput{Credentials: &ststypes.Credentials{AccessKeyId: aws.String(key), SecretAccessKey: aws.String("secret"), SessionToken: aws.String("token"), Expiration: aws.Time(time.Now().Add(time.Hour))}}, nil
			})
		},
		IAM: func(c aws.Config) IAMAPI { f.checkCustomer(c); return f },
		EC2: func(c aws.Config) EC2API { f.checkCustomer(c); return f },
	})
	if err != nil {
		t.Fatal(err)
	}
	return f, a
}

func (f *awsFixture) checkCustomer(c aws.Config) {
	creds, err := c.Credentials.Retrieve(f.t.Context())
	if err != nil || creds.AccessKeyID != "customer" || c.Region != "us-east-1" {
		f.t.Fatal("customer client must use customer credentials and the API region")
	}
}

func (f *awsFixture) GetRole(_ context.Context, in *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	f.iamCalls++
	if f.iamErr != nil {
		return nil, f.iamErr
	}
	tags := []iamtypes.Tag{}
	for key, value := range accountaccess.RequiredTags() {
		if !f.missingTag {
			tags = append(tags, iamtypes.Tag{Key: aws.String(key), Value: aws.String(value)})
		}
	}
	return &iam.GetRoleOutput{Role: &iamtypes.Role{Arn: aws.String(f.record.InstallerRoleARN), Path: aws.String(f.record.Path), RoleName: in.RoleName, AssumeRolePolicyDocument: aws.String(f.trust), Tags: tags}}, nil
}

func (f *awsFixture) ListAttachedRolePolicies(_ context.Context, in *iam.ListAttachedRolePoliciesInput, _ ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error) {
	if in.Marker == nil {
		return &iam.ListAttachedRolePoliciesOutput{IsTruncated: true, Marker: aws.String("next")}, nil
	}
	policies := []iamtypes.AttachedPolicy{}
	if !f.policyMissing {
		policies = append(policies, iamtypes.AttachedPolicy{PolicyArn: aws.String(f.cfg.InstallerManagedPolicyARN)})
	}
	return &iam.ListAttachedRolePoliciesOutput{AttachedPolicies: policies}, nil
}

func (f *awsFixture) DescribeVpcs(_ context.Context, in *ec2.DescribeVpcsInput, _ ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	f.ec2Calls++
	if f.ec2Err != nil {
		return nil, f.ec2Err
	}
	if in.NextToken == nil {
		return &ec2.DescribeVpcsOutput{NextToken: aws.String("next"), Vpcs: []ec2types.Vpc{{VpcId: aws.String("vpc-first"), CidrBlock: aws.String("10.0.0.0/16"), IsDefault: aws.Bool(true)}}}, nil
	}
	return &ec2.DescribeVpcsOutput{Vpcs: []ec2types.Vpc{{VpcId: aws.String("vpc-second"), CidrBlock: aws.String("10.1.0.0/16")}}}, nil
}

func TestTwoHopsValidationAndDiscovery(t *testing.T) {
	f, access := fixture(t)
	if err := access.Validate(t.Context(), f.record, customerAccount); err != nil {
		t.Fatal(err)
	}
	if f.jumpCalls != 1 || f.roleCalls != 1 || f.negativeCalls != 2 || f.iamCalls != 1 {
		t.Fatalf("wrong calls: %+v", f)
	}
	f.record.State = accountv1.AWSAccountAccessReady
	f.record.ValidatedConfigurationRevision = accountaccess.ConfigurationRevision(f.cfg)
	items, err := access.ListVPCs(t.Context(), f.record, customerAccount)
	if err != nil || len(items) != 2 || !items[0].IsDefault || items[1].ID != "vpc-second" || f.ec2Calls != 2 || f.jumpCalls != 2 || f.roleCalls != 2 {
		t.Fatalf("fresh discovery credentials/pagination failed: %+v %v", items, err)
	}
}

func TestAccountEnvironmentAndReadyChecksBeforeAssumption(t *testing.T) {
	for _, name := range []string{"pending", "wrong account", "wrong role account", "wrong environment", "old configuration"} {
		t.Run(name, func(t *testing.T) {
			f, access := fixture(t)
			account := customerAccount
			switch name {
			case "wrong account":
				account = "999999999999"
			case "wrong role account":
				f.record.InstallerRoleARN = strings.Replace(f.record.InstallerRoleARN, customerAccount, "999999999999", 1)
			case "wrong environment":
				f.record.EnvironmentID = "other"
			case "old configuration":
				f.record.State = accountv1.AWSAccountAccessReady
				f.record.ValidatedConfigurationRevision = strings.Repeat("a", 64)
			}
			if _, err := access.ListVPCs(t.Context(), f.record, account); err == nil || f.jumpCalls != 0 || f.roleCalls != 0 || f.ec2Calls != 0 {
				t.Fatalf("unsafe assumption attempted: %v", err)
			}
		})
	}
}

func TestValidationFailures(t *testing.T) {
	for _, name := range []string{"missing tag", "missing policy", "bypass", "negative succeeded", "negative timeout", "negative throttled", "jump denied", "customer denied", "IAM failed"} {
		t.Run(name, func(t *testing.T) {
			f, access := fixture(t)
			want := ErrValidation
			switch name {
			case "missing tag":
				f.missingTag = true
			case "missing policy":
				f.policyMissing = true
			case "bypass":
				f.trust = strings.Replace(f.trust, "StringEquals", "StringEqualsIfExists", 1)
			case "negative succeeded":
				f.negativeOverride = true
			case "negative timeout":
				f.negativeOverride, f.negativeErr = true, context.DeadlineExceeded
			case "negative throttled":
				f.negativeOverride, f.negativeErr = true, &smithy.GenericAPIError{Code: "Throttling"}
			case "jump denied":
				f.jumpErr, want = errors.New("denied"), ErrAssumeRole
			case "customer denied":
				f.roleErr, want = errors.New("denied"), ErrAssumeRole
			case "IAM failed":
				f.iamErr = errors.New("IAM unavailable")
			}
			if err := access.Validate(t.Context(), f.record, customerAccount); !errors.Is(err, want) {
				t.Fatalf("expected %v, got %v", want, err)
			}
		})
	}
}
