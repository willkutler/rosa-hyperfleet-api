package accountaccess

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
)

type tableTransport func(*http.Request) (*http.Response, error)

func (f tableTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBackendUsesTableRegionAndWorkloadCredentials(t *testing.T) {
	approved, err := config.LoadInstallerRoleConfig("../config/testdata/installer-role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, tableRegion := range []string{"us-west-2", "eu-west-1"} {
		t.Run(tableRegion, func(t *testing.T) {
			cfg := *approved
			cfg.AccountAccess.TableRegion = tableRegion
			cfg.AccountAccess.TableARN = strings.Replace(cfg.AccountAccess.TableARN, "us-west-2", tableRegion, 1)
			called := false
			httpClient := &http.Client{Transport: tableTransport(func(r *http.Request) (*http.Response, error) {
				called = true
				if !strings.Contains(r.Header.Get("Authorization"), "Credential=workload/") || !strings.Contains(r.Header.Get("Authorization"), "/"+tableRegion+"/dynamodb/aws4_request") {
					t.Errorf("request must use workload credentials and table signing region")
				}
				var body struct{ TableName string }
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TableName != cfg.AccountAccess.TableARN {
					t.Errorf("wrong table: %+v, %v", body, err)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/x-amz-json-1.0"}}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			})}
			workload := aws.Config{
				Region: "us-east-1", Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("workload", "secret", "token")),
				HTTPClient: httpClient, RetryMaxAttempts: 3, BaseEndpoint: aws.String("https://dynamodb.test"),
			}
			backend, err := NewBackend(workload, cfg)
			if err != nil {
				t.Fatal(err)
			}
			options := backend.DynamoDB.Options()
			if workload.Region != "us-east-1" || options.Region != tableRegion || options.Credentials != workload.Credentials || options.HTTPClient != workload.HTTPClient || options.RetryMaxAttempts != 3 || aws.ToString(options.BaseEndpoint) != aws.ToString(workload.BaseEndpoint) {
				t.Fatal("table client changed workload settings beyond its region")
			}
			// Exercise the SDK's request signing and table ARN serialization without AWS.
			_, err = backend.DynamoDB.GetItem(context.Background(), &dynamodb.GetItemInput{
				TableName: aws.String(backend.Config.AccountAccess.TableARN),
				Key: map[string]types.AttributeValue{
					"pk": &types.AttributeValueMemberS{Value: "ENV#ephemeral-example-123#ACCOUNT#123456789012"},
					"sk": &types.AttributeValueMemberS{Value: "INSTALLER"},
				},
			})
			if err != nil || !called {
				t.Fatalf("table request failed: %v", err)
			}
		})
	}
}

func TestBackendRejectsInvalidConfig(t *testing.T) {
	if backend, err := NewBackend(aws.Config{Region: "us-east-1"}, config.InstallerRoleConfig{}); err == nil || backend != nil {
		t.Fatal("invalid configuration accepted")
	}
}
