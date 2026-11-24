package accountaccess_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/smithy-go"
	"github.com/google/uuid"
	accountv1 "github.com/openshift-online/rosa-seaplane-api/api/accountaccess/v1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/accountaccess"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
)

const owner = "123456789012"

type wireItem map[string]map[string]string

type storeRequest struct {
	TableName                             string
	Key, Item                             wireItem
	ConsistentRead                        bool
	ConditionExpression, UpdateExpression string
	ExpressionAttributeNames              map[string]string
	ExpressionAttributeValues             wireItem
}

func approvedConfig(t *testing.T) config.InstallerRoleConfig {
	t.Helper()
	cfg, err := config.LoadInstallerRoleConfig("../config/testdata/installer-role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return *cfg
}

func store(t *testing.T, client *dynamodb.Client, cfg config.InstallerRoleConfig) *accountaccess.DynamoDBStore {
	t.Helper()
	s, err := accountaccess.NewStore(client, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// storeClient exercises the real SDK against a scripted HTTP endpoint. Persistence
// and concurrent conditional-write behavior are tested with DynamoDB Local.
func storeClient(t *testing.T, respond func(http.ResponseWriter, string, storeRequest)) *dynamodb.Client {
	t.Helper()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in storeRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		respond(w, strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "DynamoDB_20120810."), in)
	}))
	t.Cleanup(endpoint.Close)
	return dynamodb.NewFromConfig(aws.Config{
		Region: "us-west-2", Credentials: credentials.NewStaticCredentialsProvider("local", "local", ""), RetryMaxAttempts: 1,
	}, func(o *dynamodb.Options) { o.BaseEndpoint = aws.String(endpoint.URL) })
}

func storedRecord(t *testing.T, cfg config.InstallerRoleConfig) accountaccess.AWSAccountAccess {
	t.Helper()
	_, roleARN, err := accountaccess.DeriveRole(owner, "Example", "/custom/")
	if err != nil {
		t.Fatal(err)
	}
	return accountaccess.AWSAccountAccess{
		AssignmentID: uuid.NewString(), ResourceVersion: uuid.NewString(), AccountID: owner, EnvironmentID: cfg.EnvironmentID,
		Prefix: "Example", Path: "/custom/", InstallerRoleARN: roleARN, State: accountv1.AWSAccountAccessPending,
	}
}

func storedItem(t *testing.T, record accountaccess.AWSAccountAccess) wireItem {
	t.Helper()
	payload, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return wireItem{
		"pk": {"S": "ENV#" + record.EnvironmentID + "#ACCOUNT#" + record.AccountID}, "sk": {"S": "INSTALLER"},
		"assignmentId": {"S": record.AssignmentID}, "resourceVersion": {"S": record.ResourceVersion}, "record": {"S": string(payload)},
	}
}

func conditionalFailure(w http.ResponseWriter) {
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(`{"__type":"com.amazonaws.dynamodb.v20120810#ConditionalCheckFailedException"}`))
}

func TestStoreInitializesConditionally(t *testing.T) {
	cfg := approvedConfig(t)
	requests := make(chan storeRequest, 1)
	client := storeClient(t, func(w http.ResponseWriter, operation string, in storeRequest) {
		if operation != "PutItem" {
			t.Errorf("unexpected operation %s", operation)
		}
		requests <- in
		_, _ = w.Write([]byte(`{}`))
	})
	record, created, err := store(t, client, cfg).InitializeAWSAccountAccess(t.Context(), owner, "Example", "/custom/")
	if err != nil || !created || record.State != accountv1.AWSAccountAccessPending {
		t.Fatalf("initialize: %+v %v %v", record, created, err)
	}
	in := <-requests
	if in.TableName != cfg.AccountAccess.TableARN || in.ConditionExpression != "attribute_not_exists(pk)" || in.Item["pk"]["S"] != "ENV#"+cfg.EnvironmentID+"#ACCOUNT#"+owner || in.Item["sk"]["S"] != "INSTALLER" {
		t.Fatalf("incorrect conditional write: %+v", in)
	}
	var saved accountaccess.AWSAccountAccess
	if err := json.Unmarshal([]byte(in.Item["record"]["S"]), &saved); err != nil || saved != record {
		t.Fatalf("incorrect stored payload: %+v %v", saved, err)
	}
}

func TestStoreInitializesExistingAssignment(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "same selection", true: "conflicting selection"}[conflict], func(t *testing.T) {
			cfg := approvedConfig(t)
			existing := storedRecord(t, cfg)
			client := storeClient(t, func(w http.ResponseWriter, operation string, in storeRequest) {
				switch operation {
				case "PutItem":
					conditionalFailure(w)
				case "GetItem":
					if !in.ConsistentRead || in.TableName != cfg.AccountAccess.TableARN || in.Key["pk"]["S"] != "ENV#"+cfg.EnvironmentID+"#ACCOUNT#"+owner {
						t.Errorf("incorrect setup read: %+v", in)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"Item": storedItem(t, existing)})
				default:
					t.Errorf("unexpected operation %s", operation)
				}
			})
			prefix := existing.Prefix
			if conflict {
				prefix = "Other"
			}
			got, created, err := store(t, client, cfg).InitializeAWSAccountAccess(t.Context(), owner, prefix, existing.Path)
			if conflict {
				if !errors.Is(err, accountaccess.ErrConflict) {
					t.Fatalf("want conflict, got %v", err)
				}
			} else if err != nil || created || got != existing {
				t.Fatalf("want existing assignment, got %+v %v %v", got, created, err)
			}
		})
	}
}

func TestStoreCheckedUpdates(t *testing.T) {
	for _, ready := range []bool{false, true} {
		cfg := approvedConfig(t)
		expected := storedRecord(t, cfg)
		if !ready {
			expected.State = accountv1.AWSAccountAccessReady
			expected.ValidatedConfigurationRevision = accountaccess.ConfigurationRevision(cfg)
		}
		client := storeClient(t, func(w http.ResponseWriter, operation string, in storeRequest) {
			if operation != "UpdateItem" || in.TableName != cfg.AccountAccess.TableARN || in.Key["pk"]["S"] != "ENV#"+cfg.EnvironmentID+"#ACCOUNT#"+owner || in.ConditionExpression != "assignmentId = :assignment AND resourceVersion = :expected" || in.ExpressionAttributeValues[":assignment"]["S"] != expected.AssignmentID || in.ExpressionAttributeValues[":expected"]["S"] != expected.ResourceVersion {
				t.Errorf("incorrect conditional update: %+v", in)
			}
			var updated accountaccess.AWSAccountAccess
			if err := json.Unmarshal([]byte(in.ExpressionAttributeValues[":record"]["S"]), &updated); err != nil {
				t.Error(err)
			}
			if updated.AssignmentID != expected.AssignmentID || updated.ResourceVersion == expected.ResourceVersion || in.ExpressionAttributeValues[":next"]["S"] != updated.ResourceVersion || in.ExpressionAttributeNames["#record"] != "record" {
				t.Errorf("incorrect updated payload: %+v", updated)
			}
			want := accountv1.AWSAccountAccessPending
			if ready {
				want = accountv1.AWSAccountAccessReady
			}
			if updated.State != want || (updated.ValidatedConfigurationRevision != "") != ready {
				t.Errorf("incorrect state: %+v", updated)
			}
			_, _ = w.Write([]byte(`{}`))
		})
		s := store(t, client, cfg)
		var updated accountaccess.AWSAccountAccess
		var err error
		if ready {
			updated, err = s.MarkAWSAccountAccessReady(t.Context(), expected, accountaccess.ConfigurationRevision(cfg))
		} else {
			updated, err = s.MarkAWSAccountAccessPending(t.Context(), expected)
		}
		if err != nil || updated.ResourceVersion == expected.ResourceVersion {
			t.Fatalf("update failed: %+v %v", updated, err)
		}
	}
}

func TestStoreRejectsCorruptRecordsAndStorageErrors(t *testing.T) {
	for _, mutate := range []struct {
		name string
		fn   func(*accountaccess.AWSAccountAccess)
	}{
		{"account", func(r *accountaccess.AWSAccountAccess) { r.AccountID = "999999999999" }},
		{"environment", func(r *accountaccess.AWSAccountAccess) { r.EnvironmentID = "another" }},
		{"role owner", func(r *accountaccess.AWSAccountAccess) {
			r.InstallerRoleARN = strings.Replace(r.InstallerRoleARN, owner, "999999999999", 1)
		}},
		{"role path", func(r *accountaccess.AWSAccountAccess) { r.Path = "/wrong/" }},
		{"assignment", func(r *accountaccess.AWSAccountAccess) { r.AssignmentID = "invalid" }},
		{"version", func(r *accountaccess.AWSAccountAccess) { r.ResourceVersion = "invalid" }},
		{"state", func(r *accountaccess.AWSAccountAccess) { r.State = "unknown" }},
		{"ready without validation", func(r *accountaccess.AWSAccountAccess) { r.State = accountv1.AWSAccountAccessReady }},
		{"pending with validation", func(r *accountaccess.AWSAccountAccess) { r.ValidatedConfigurationRevision = strings.Repeat("a", 64) }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			cfg := approvedConfig(t)
			record := storedRecord(t, cfg)
			mutate.fn(&record)
			item := storedItem(t, record)
			client := storeClient(t, func(w http.ResponseWriter, operation string, _ storeRequest) {
				switch operation {
				case "GetItem":
					_ = json.NewEncoder(w).Encode(map[string]any{"Item": item})
				case "PutItem":
					conditionalFailure(w)
				default:
					t.Errorf("unexpected operation %s", operation)
				}
			})
			s := store(t, client, cfg)
			if _, err := s.GetAWSAccountAccess(t.Context(), owner); !errors.Is(err, accountaccess.ErrCorruptRecord) {
				t.Fatalf("corrupt read accepted: %v", err)
			}
			if _, _, err := s.InitializeAWSAccountAccess(t.Context(), owner, "Example", "/"); !errors.Is(err, accountaccess.ErrCorruptRecord) {
				t.Fatalf("corrupt initialization accepted: %v", err)
			}
		})
	}
	cfg := approvedConfig(t)
	client := storeClient(t, func(w http.ResponseWriter, _ string, _ storeRequest) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"__type":"InternalServerError","message":"backend failure"}`))
	})
	s := store(t, client, cfg)
	for _, operation := range []func() error{
		func() error {
			_, _, err := s.InitializeAWSAccountAccess(t.Context(), owner, "Example", "/")
			return err
		},
		func() error { _, err := s.GetAWSAccountAccess(t.Context(), owner); return err },
		func() error {
			_, err := s.MarkAWSAccountAccessReady(t.Context(), storedRecord(t, cfg), accountaccess.ConfigurationRevision(cfg))
			return err
		},
	} {
		var backend smithy.APIError
		if err := operation(); !errors.As(err, &backend) || backend.ErrorCode() != "InternalServerError" {
			t.Fatalf("storage error swallowed: %v", err)
		}
	}
}

func TestRoleSelectionValidation(t *testing.T) {
	for _, tc := range []struct{ prefix, path string }{
		{"", "/"}, {strings.Repeat("x", 65-len(accountaccess.InstallerRoleSuffix)), "/"}, {"Bad Prefix", "/"}, {"Bad#prefix", "/"},
		{"Valid", ""}, {"Valid", "//"}, {"Valid", "missing/"}, {"Valid", "/missing"}, {"Valid", "/with space/"}, {"Valid", "/wild*/"}, {"Valid", "/unicode-é/"}, {"Valid", "/" + strings.Repeat("a", 511) + "/"},
	} {
		if _, _, err := accountaccess.DeriveRole(owner, tc.prefix, tc.path); !errors.Is(err, accountaccess.ErrInvalidInput) {
			t.Errorf("accepted prefix=%q path=%q: %v", tc.prefix, tc.path, err)
		}
	}
	if _, _, err := accountaccess.DeriveRole("bad-account", "Example", "/"); !errors.Is(err, accountaccess.ErrInvalidInput) {
		t.Fatal("invalid account accepted")
	}
}
