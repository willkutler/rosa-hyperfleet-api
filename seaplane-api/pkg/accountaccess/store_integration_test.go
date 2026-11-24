//go:build integration

package accountaccess_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/google/uuid"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/accountaccess"
)

const dynamoLocalImage = "public.ecr.aws/aws-dynamodb-local/aws-dynamodb-local@sha256:744bf7138f0e893a6438de89bc3374501105b42bc7473cef6d561f30ff2dec6a"

func integrationDynamoDB(t *testing.T) *dynamodb.Client {
	t.Helper()
	endpoint := os.Getenv("ACCOUNT_ACCESS_DYNAMODB_ENDPOINT")
	if endpoint == "" {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		_ = listener.Close()
		name := "installer-role-test-" + uuid.NewString()
		out, err := exec.Command("podman", "run", "-d", "--rm", "--name", name, "-p", fmt.Sprintf("127.0.0.1:%d:8000", port), dynamoLocalImage, "-jar", "DynamoDBLocal.jar", "-inMemory", "-sharedDb").CombinedOutput()
		if err != nil {
			t.Fatalf("start DynamoDB Local: %v\n%s", err, out)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if out, err := exec.CommandContext(ctx, "podman", "stop", name).CombinedOutput(); err != nil {
				t.Errorf("stop DynamoDB Local: %v %s", err, out)
			}
		})
		endpoint = fmt.Sprintf("http://127.0.0.1:%d", port)
	}
	client := dynamodb.NewFromConfig(aws.Config{Region: "us-west-2", Credentials: credentials.NewStaticCredentialsProvider("local", "local", ""), RetryMaxAttempts: 1}, func(o *dynamodb.Options) { o.BaseEndpoint = aws.String(endpoint) })
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		_, err := client.ListTables(ctx, &dynamodb.ListTablesInput{})
		cancel()
		if err == nil {
			return client
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("DynamoDB Local did not become ready within 30 seconds")
	return nil
}

func TestAccountAccessDynamoDBLocal(t *testing.T) {
	client := integrationDynamoDB(t)
	name := "account-access-" + uuid.NewString()
	_, err := client.CreateTable(t.Context(), &dynamodb.CreateTableInput{
		TableName: &name, BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}, {AttributeName: aws.String("sk"), AttributeType: types.ScalarAttributeTypeS}},
		KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}, {AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := client.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{TableName: &name}); err != nil {
			t.Error(err)
		}
	})
	cfg := approvedConfig(t)
	// DynamoDB Local reports an emulator-only region in its ARN. Exercise store
	// operations with an explicitly configured AWS table ARN, as production does.
	cfg.AccountAccess.TableARN = "arn:aws:dynamodb:us-west-2:000000000000:table/" + name
	s := store(t, client, cfg)
	var wg sync.WaitGroup
	var createdCount atomic.Int32
	assignments := make(chan accountaccess.AWSAccountAccess, 20)
	for range 20 {
		wg.Go(func() {
			r, created, err := s.InitializeAWSAccountAccess(t.Context(), owner, "Example", "/custom/")
			if err != nil {
				t.Error(err)
				return
			}
			if created {
				createdCount.Add(1)
			}
			assignments <- r
		})
	}
	wg.Wait()
	close(assignments)
	var first accountaccess.AWSAccountAccess
	for r := range assignments {
		if first.AssignmentID != "" && first != r {
			t.Fatal("concurrent initialization returned different assignments")
		}
		first = r
	}
	if createdCount.Load() != 1 {
		t.Fatalf("created %d assignments", createdCount.Load())
	}
	// A new store represents a restart or another service region using this table.
	reloaded, err := store(t, client, cfg).GetAWSAccountAccess(t.Context(), owner)
	if err != nil || reloaded != first {
		t.Fatalf("record did not persist: %+v %v", reloaded, err)
	}
	if _, _, err := s.InitializeAWSAccountAccess(t.Context(), owner, "Different", "/custom/"); !errors.Is(err, accountaccess.ErrConflict) {
		t.Fatalf("competing selection: %v", err)
	}
	var readyCount atomic.Int32
	for range 10 {
		wg.Go(func() {
			_, err := s.MarkAWSAccountAccessReady(t.Context(), first, accountaccess.ConfigurationRevision(cfg))
			if err == nil {
				readyCount.Add(1)
			} else if !errors.Is(err, accountaccess.ErrConflict) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if readyCount.Load() != 1 {
		t.Fatalf("CAS allowed %d writers", readyCount.Load())
	}
	if _, err := s.MarkAWSAccountAccessPending(t.Context(), first); !errors.Is(err, accountaccess.ErrConflict) {
		t.Fatalf("stale revision accepted: %v", err)
	}
	ready, err := s.GetAWSAccountAccess(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkAWSAccountAccessPending(t.Context(), ready); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAWSAccountAccess(t.Context(), "999999999999"); !errors.Is(err, accountaccess.ErrSetupRequired) {
		t.Fatalf("account isolation failed: %v", err)
	}
	cfg.EnvironmentID = "ephemeral-second"
	other := store(t, client, cfg)
	if _, err := other.GetAWSAccountAccess(t.Context(), owner); !errors.Is(err, accountaccess.ErrSetupRequired) {
		t.Fatalf("environment isolation failed: %v", err)
	}
	shared, _, err := other.InitializeAWSAccountAccess(t.Context(), owner, first.Prefix, first.Path)
	if err != nil || shared.InstallerRoleARN != first.InstallerRoleARN || shared.AssignmentID == first.AssignmentID {
		t.Fatalf("shared-role assignment failed: %+v %v", shared, err)
	}
}
