package accountaccess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/google/uuid"
	accountv1 "github.com/openshift-online/rosa-seaplane-api/api/accountaccess/v1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
)

type Store interface {
	InitializeAWSAccountAccess(context.Context, string, string, string) (AWSAccountAccess, bool, error)
	GetAWSAccountAccess(context.Context, string) (AWSAccountAccess, error)
	MarkAWSAccountAccessReady(context.Context, AWSAccountAccess, string) (AWSAccountAccess, error)
	MarkAWSAccountAccessPending(context.Context, AWSAccountAccess) (AWSAccountAccess, error)
}

type DynamoDBStore struct {
	client      *dynamodb.Client
	environment string
	tableARN    string
}

func NewStore(client *dynamodb.Client, cfg config.InstallerRoleConfig) (*DynamoDBStore, error) {
	if client == nil {
		return nil, fmt.Errorf("account-access DynamoDB client is required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &DynamoDBStore{client: client, environment: cfg.EnvironmentID, tableARN: cfg.AccountAccess.TableARN}, nil
}

func (s *DynamoDBStore) key(account string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"pk": textValue(recordKey(s.environment, account)), "sk": textValue("INSTALLER")}
}

func textValue(value string) types.AttributeValue { return &types.AttributeValueMemberS{Value: value} }

func stringValue(item map[string]types.AttributeValue, key string) string {
	v, _ := item[key].(*types.AttributeValueMemberS)
	if v == nil {
		return ""
	}
	return v.Value
}

func (s *DynamoDBStore) InitializeAWSAccountAccess(ctx context.Context, account, prefix, path string) (AWSAccountAccess, bool, error) {
	_, roleARN, err := DeriveRole(account, prefix, path)
	if err != nil {
		return AWSAccountAccess{}, false, err
	}
	record := AWSAccountAccess{AssignmentID: uuid.NewString(), ResourceVersion: uuid.NewString(), AccountID: account, EnvironmentID: s.environment, Prefix: prefix, Path: path, InstallerRoleARN: roleARN, State: accountv1.AWSAccountAccessPending}
	content, err := json.Marshal(record)
	if err != nil {
		return AWSAccountAccess{}, false, err
	}
	item := s.key(account)
	item["assignmentId"], item["resourceVersion"], item["record"] = textValue(record.AssignmentID), textValue(record.ResourceVersion), textValue(string(content))
	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(s.tableARN), Item: item, ConditionExpression: aws.String("attribute_not_exists(pk)")})
	if err == nil {
		return record, true, nil
	}
	var conflict *types.ConditionalCheckFailedException
	if !errors.As(err, &conflict) {
		return AWSAccountAccess{}, false, fmt.Errorf("initialize account-access record: %w", err)
	}
	existing, err := s.GetAWSAccountAccess(ctx, account)
	if err != nil {
		return AWSAccountAccess{}, false, err
	}
	if existing.Prefix != prefix || existing.Path != path {
		return AWSAccountAccess{}, false, ErrConflict
	}
	return existing, false, nil
}

func (s *DynamoDBStore) GetAWSAccountAccess(ctx context.Context, account string) (AWSAccountAccess, error) {
	if !accountPattern.MatchString(account) {
		return AWSAccountAccess{}, ErrInvalidInput
	}
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(s.tableARN), Key: s.key(account), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return AWSAccountAccess{}, fmt.Errorf("read account-access record: %w", err)
	}
	if out == nil {
		return AWSAccountAccess{}, ErrCorruptRecord
	}
	if len(out.Item) == 0 {
		return AWSAccountAccess{}, ErrSetupRequired
	}
	var record AWSAccountAccess
	if err := json.Unmarshal([]byte(stringValue(out.Item, "record")), &record); err != nil {
		return AWSAccountAccess{}, ErrCorruptRecord
	}
	if err := record.Validate(s.environment, account); err != nil || stringValue(out.Item, "pk") != recordKey(s.environment, account) || stringValue(out.Item, "sk") != "INSTALLER" || stringValue(out.Item, "assignmentId") != record.AssignmentID || stringValue(out.Item, "resourceVersion") != record.ResourceVersion {
		return AWSAccountAccess{}, ErrCorruptRecord
	}
	return record, nil
}

func (s *DynamoDBStore) MarkAWSAccountAccessReady(ctx context.Context, expected AWSAccountAccess, configurationRevision string) (AWSAccountAccess, error) {
	if !revisionPattern.MatchString(configurationRevision) {
		return AWSAccountAccess{}, ErrInvalidInput
	}
	return s.update(ctx, expected, accountv1.AWSAccountAccessReady, configurationRevision)
}

func (s *DynamoDBStore) MarkAWSAccountAccessPending(ctx context.Context, expected AWSAccountAccess) (AWSAccountAccess, error) {
	return s.update(ctx, expected, accountv1.AWSAccountAccessPending, "")
}

func (s *DynamoDBStore) update(ctx context.Context, expected AWSAccountAccess, state accountv1.AWSAccountAccessState, configurationRevision string) (AWSAccountAccess, error) {
	if err := expected.Validate(s.environment, expected.AccountID); err != nil {
		return AWSAccountAccess{}, err
	}
	updated := expected
	updated.State, updated.ValidatedConfigurationRevision, updated.ResourceVersion = state, configurationRevision, uuid.NewString()
	content, err := json.Marshal(updated)
	if err != nil {
		return AWSAccountAccess{}, err
	}
	_, err = s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.tableARN), Key: s.key(expected.AccountID),
		ConditionExpression:      aws.String("assignmentId = :assignment AND resourceVersion = :expected"),
		UpdateExpression:         aws.String("SET #record = :record, resourceVersion = :next"),
		ExpressionAttributeNames: map[string]string{"#record": "record"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":assignment": textValue(expected.AssignmentID), ":expected": textValue(expected.ResourceVersion),
			":next": textValue(updated.ResourceVersion), ":record": textValue(string(content)),
		},
	})
	var conflict *types.ConditionalCheckFailedException
	if errors.As(err, &conflict) {
		return AWSAccountAccess{}, ErrConflict
	}
	if err != nil {
		return AWSAccountAccess{}, fmt.Errorf("update account-access record: %w", err)
	}
	return updated, nil
}
