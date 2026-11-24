package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/google/uuid"
	accountv1 "github.com/openshift-online/rosa-seaplane-api/api/accountaccess/v1"
	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/accountaccess"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/awsaccess"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/seaplanedb"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/middleware"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const pocAccount = "333333333333" // Not registered in the Cedar bundle or allowlist.

type accountAccessHTTPClient func(*http.Request) (*http.Response, error)

func (f accountAccessHTTPClient) Do(r *http.Request) (*http.Response, error) { return f(r) }

type customerAWSFake struct {
	validateCalls, listCalls int
	err                      error
	validate                 func()
}

func (f *customerAWSFake) Validate(ctx context.Context, r accountaccess.AWSAccountAccess, account string) error {
	f.validateCalls++
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("AWS validation needs an operation deadline")
	}
	if r.AccountID != account {
		return errors.New("wrong owner")
	}
	if f.validate != nil {
		f.validate()
	}
	return f.err
}

func (f *customerAWSFake) ListVPCs(_ context.Context, r accountaccess.AWSAccountAccess, account string) ([]accountv1.VPC, error) {
	f.listCalls++
	if r.AccountID != account {
		return nil, errors.New("wrong owner")
	}
	return []accountv1.VPC{{ID: "vpc-known", CIDRBlock: "10.0.0.0/16", IsDefault: true}}, f.err
}

func pocServer(t *testing.T, dynamo *dynamodb.Client, cfg config.InstallerRoleConfig, customer *customerAWSFake) (*Server, accountaccess.Store) {
	t.Helper()
	store, err := accountaccess.NewStore(dynamo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	serviceConfig := config.NewConfig()
	serviceConfig.InstallerRole = &cfg
	authorizer := configuredAuthorizer(t, serviceConfig)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scheme := runtime.NewScheme()
	if err := seaplanev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	db := seaplanedb.NewClientFrom(fake.NewClientBuilder().WithScheme(scheme).Build(), logger)
	srv, err := New(serviceConfig, db, authorizer, nil, logger, WithAccountAccess(&accountaccess.Backend{Config: cfg, Store: store}, customer))
	if err != nil {
		t.Fatal(err)
	}
	return srv, store
}

func pocConfig(t *testing.T) config.InstallerRoleConfig {
	t.Helper()
	cfg, err := config.LoadInstallerRoleConfig("../config/testdata/installer-role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return *cfg
}

func pocRequest(t *testing.T, srv *Server, method, path, account, body string, want int) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if account != "" {
		r.Header.Set(middleware.HeaderAccountID, account)
		r.Header.Set(middleware.HeaderCallerARN, "arn:aws:sts::"+account+":assumed-role/customer/session")
	}
	w := httptest.NewRecorder()
	srv.apiServer.Handler.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s %s: want %d, got %d %s", method, path, want, w.Code, w.Body)
	}
	if want >= 400 {
		var status struct{ Kind, APIVersion, Message string }
		if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || status.Kind != "Status" || status.APIVersion != "v1" {
			t.Fatalf("error is not a Kubernetes Status: %s", w.Body)
		}
	}
	return w
}

func completionBody(t *testing.T, record accountaccess.AWSAccountAccess, cfg config.InstallerRoleConfig) string {
	t.Helper()
	body, err := json.Marshal(accountv1.ValidateAWSAccountAccessRequest{AssignmentID: record.AssignmentID, ResourceVersion: record.ResourceVersion, ConfigurationRevision: accountaccess.ConfigurationRevision(cfg)})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestAccountAccessHTTPWorkflow(t *testing.T) {
	dynamo := managementDynamo(t)
	aws := &customerAWSFake{}
	cfg := pocConfig(t)
	srv, store := pocServer(t, dynamo, cfg, aws)
	pocRequest(t, srv, "GET", "/api/v0/installer-role-config", pocAccount, "", 409)
	first := pocRequest(t, srv, "POST", "/api/v0/aws-account-access", pocAccount, `{"prefix":"Example","path":"/custom/"}`, 201)
	again := pocRequest(t, srv, "POST", "/api/v0/aws-account-access", pocAccount, `{"prefix":"Example","path":"/custom/"}`, 200)
	if !bytes.Equal(first.Body.Bytes(), again.Body.Bytes()) {
		t.Fatal("initialization did not preserve assignment/version")
	}
	pocRequest(t, srv, "POST", "/api/v0/aws-account-access", pocAccount, `{"prefix":"Different","path":"/"}`, 409)
	pocRequest(t, srv, "GET", "/api/v0/cloud_discovery/vpcs", pocAccount, "", 409)
	if aws.listCalls != 0 {
		t.Fatal("Pending discovery accessed AWS")
	}
	response := pocRequest(t, srv, "GET", "/api/v0/installer-role-config", pocAccount, "", 200)
	var approved accountv1.InstallerRoleConfigResponse
	if err := json.Unmarshal(response.Body.Bytes(), &approved); err != nil {
		t.Fatal(err)
	}
	if approved.AccountID != pocAccount || approved.ExternalID != pocAccount || approved.EnvironmentID != cfg.EnvironmentID || approved.InstallerRoleARN != "arn:aws:iam::"+pocAccount+":role/custom/Example-HCP-ROSA-Installer-Role" || approved.RequiredTags["seaplane-access"] != "true" {
		t.Fatalf("wrong configuration: %+v", approved)
	}
	if err := awsaccess.ValidateTrust(string(approved.TrustPolicy), cfg.JumpRoleARN, pocAccount); err != nil {
		t.Fatal(err)
	}
	record, err := store.GetAWSAccountAccess(t.Context(), pocAccount)
	if err != nil {
		t.Fatal(err)
	}
	body := completionBody(t, record, cfg)
	ready := pocRequest(t, srv, "PUT", "/api/v0/aws-account-access", pocAccount, body, 200)
	var completed accountv1.AWSAccountAccessResponse
	if err := json.Unmarshal(ready.Body.Bytes(), &completed); err != nil || completed.State != accountv1.AWSAccountAccessReady || completed.AssignmentID != record.AssignmentID || completed.ResourceVersion == record.ResourceVersion {
		t.Fatalf("invalid completion: %s", ready.Body)
	}
	pocRequest(t, srv, "PUT", "/api/v0/aws-account-access", pocAccount, body, 409)
	if aws.validateCalls != 1 {
		t.Fatal("stale completion reached AWS")
	}
	vpcs := pocRequest(t, srv, "GET", "/api/v0/cloud_discovery/vpcs", pocAccount, "", 200)
	if !strings.Contains(vpcs.Body.String(), "vpc-known") || aws.listCalls != 1 {
		t.Fatal("known VPC missing")
	}
	// A second API instance sees the same assignment and Ready state.
	restarted, _ := pocServer(t, dynamo, cfg, aws)
	pocRequest(t, restarted, "GET", "/api/v0/cloud_discovery/vpcs", pocAccount, "", 200)
	// Another environment may use the same customer role but needs its own setup.
	otherCfg := cfg
	otherCfg.EnvironmentID = "ephemeral-second"
	other, otherStore := pocServer(t, dynamo, otherCfg, aws)
	pocRequest(t, other, "GET", "/api/v0/installer-role-config", pocAccount, "", 409)
	pocRequest(t, other, "POST", "/api/v0/aws-account-access", pocAccount, `{"prefix":"Example","path":"/custom/"}`, 201)
	otherRecord, err := otherStore.GetAWSAccountAccess(t.Context(), pocAccount)
	if err != nil || otherRecord.AssignmentID == record.AssignmentID || otherRecord.InstallerRoleARN != record.InstallerRoleARN {
		t.Fatal("cross-environment reuse failed")
	}
	// Existing cluster routes still require enrollment/grants.
	pocRequest(t, srv, "GET", "/api/v0/clusters", pocAccount, "", 403)
	// A different account cannot read or complete this account's assignment.
	pocRequest(t, srv, "GET", "/api/v0/installer-role-config", "444444444444", "", 409)
	pocRequest(t, srv, "PUT", "/api/v0/aws-account-access", "444444444444", body, 409)
}

func TestAccountAccessHTTPRejectsOverridesAndInvalidIdentity(t *testing.T) {
	dynamo := managementDynamo(t)
	var storageCalls atomic.Int32
	transport := dynamo.Options().HTTPClient
	dynamo = dynamodb.New(dynamo.Options(), func(o *dynamodb.Options) {
		o.HTTPClient = accountAccessHTTPClient(func(r *http.Request) (*http.Response, error) {
			storageCalls.Add(1)
			return transport.Do(r)
		})
	})
	aws := &customerAWSFake{}
	srv, _ := pocServer(t, dynamo, pocConfig(t), aws)
	for _, path := range []string{"/api/v0/aws-account-access", "/api/v0/installer-role-config", "/api/v0/cloud_discovery/vpcs"} {
		method, body := "GET", ""
		if strings.HasSuffix(path, "aws-account-access") {
			method, body = "POST", `{"prefix":"Example","path":"/"}`
		}
		for _, identity := range [][2]string{{"", ""}, {pocAccount, ""}, {"123", "arn:aws:iam::123:user/test"}, {pocAccount, "arn:aws:iam::444444444444:user/test"}, {pocAccount, "arn:aws:iam::333333333333:root"}} {
			r := httptest.NewRequest(method, path, strings.NewReader(body))
			r.Header.Set(middleware.HeaderAccountID, identity[0])
			r.Header.Set(middleware.HeaderCallerARN, identity[1])
			w := httptest.NewRecorder()
			srv.apiServer.Handler.ServeHTTP(w, r)
			if w.Code != 403 {
				t.Fatalf("invalid identity admitted: %v %d %s", identity, w.Code, w.Body)
			}
		}
	}
	for _, body := range []string{
		`null`, `{}`, `[]`, `{"prefix":"Example"}`, `{"prefix":"Example","path":"/"} {}`,
		`{"prefix":"Example","path":"/","accountId":"444444444444"}`,
		`{"prefix":"Example","path":"/","environmentId":"other"}`,
		`{"prefix":"Example","path":"/","installerRoleArn":"other"}`,
		`{"prefix":"Example","path":"/","externalId":"other"}`,
		`{"prefix":"Bad Prefix","path":"/"}`,
	} {
		pocRequest(t, srv, "POST", "/api/v0/aws-account-access", pocAccount, body, 400)
	}
	for _, path := range []string{"/api/v0/installer-role-config?accountId=444444444444", "/api/v0/cloud_discovery/vpcs?region=us-west-2"} {
		pocRequest(t, srv, "GET", path, pocAccount, "", 400)
	}
	pocRequest(t, srv, "GET", "/api/v0/installer-role-config", pocAccount, `{}`, 400)
	pocRequest(t, srv, "PUT", "/api/v0/aws-account-access", pocAccount, `{"assignmentId":"invalid"}`, 400)
	if storageCalls.Load() != 0 || aws.validateCalls != 0 || aws.listCalls != 0 {
		t.Fatal("invalid requests reached storage or AWS")
	}
}

func TestAccountAccessRevalidationAndErrors(t *testing.T) {
	for _, scenario := range []string{"failed revalidation", "concurrent validation", "configuration changed", "assignment changed", "EC2 error", "assume error", "storage error"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := pocConfig(t)
			dynamo := managementDynamo(t)
			var storageUnavailable atomic.Bool
			transport := dynamo.Options().HTTPClient
			dynamo = dynamodb.New(dynamo.Options(), func(o *dynamodb.Options) {
				o.RetryMaxAttempts = 1
				o.HTTPClient = accountAccessHTTPClient(func(r *http.Request) (*http.Response, error) {
					if storageUnavailable.Load() {
						return nil, errors.New("SECRET_TOKEN")
					}
					return transport.Do(r)
				})
			})
			aws := &customerAWSFake{}
			srv, store := pocServer(t, dynamo, cfg, aws)
			r, _, err := store.InitializeAWSAccountAccess(t.Context(), pocAccount, "Example", "/")
			if err != nil {
				t.Fatal(err)
			}
			r, err = store.MarkAWSAccountAccessReady(t.Context(), r, accountaccess.ConfigurationRevision(cfg))
			if err != nil {
				t.Fatal(err)
			}
			body := completionBody(t, r, cfg)
			switch scenario {
			case "failed revalidation", "concurrent validation":
				aws.err = &awsaccess.ValidationError{Reason: "unsafe trust", Cause: errors.New("SECRET_TOKEN")}
				want := 422
				if scenario == "concurrent validation" {
					want = 409
					aws.validate = func() {
						if _, err := store.MarkAWSAccountAccessReady(t.Context(), r, accountaccess.ConfigurationRevision(cfg)); err != nil {
							t.Fatal(err)
						}
					}
				}
				result := pocRequest(t, srv, "PUT", "/api/v0/aws-account-access", pocAccount, body, want)
				if strings.Contains(result.Body.String(), "SECRET_TOKEN") {
					t.Fatal("AWS details leaked")
				}
				current, err := store.GetAWSAccountAccess(t.Context(), pocAccount)
				if err != nil {
					t.Fatal(err)
				}
				if (current.State == accountv1.AWSAccountAccessPending) != (scenario == "failed revalidation") {
					t.Fatal("failed revalidation overwrote a newer result or remained Ready")
				}
			case "configuration changed":
				cfg.JumpRoleARN = "arn:aws:iam::123456789012:role/changed-jump"
				changed, _ := pocServer(t, dynamo, cfg, aws)
				pocRequest(t, changed, "PUT", "/api/v0/aws-account-access", pocAccount, body, 409)
				pocRequest(t, changed, "GET", "/api/v0/cloud_discovery/vpcs", pocAccount, "", 409)
				if aws.validateCalls != 0 || aws.listCalls != 0 {
					t.Fatal("stale config reached AWS")
				}
			case "assignment changed":
				body = strings.Replace(body, r.AssignmentID, uuid.NewString(), 1)
				pocRequest(t, srv, "PUT", "/api/v0/aws-account-access", pocAccount, body, 409)
				if aws.validateCalls != 0 {
					t.Fatal("wrong assignment reached AWS")
				}
			case "EC2 error", "assume error":
				aws.err = awsaccess.ErrDiscovery
				if scenario == "assume error" {
					aws.err = awsaccess.ErrAssumeRole
				}
				result := pocRequest(t, srv, "GET", "/api/v0/cloud_discovery/vpcs", pocAccount, "", 502)
				code := "ACCOUNT-ACCESS-EC2"
				if scenario == "assume error" {
					code = "ACCOUNT-ACCESS-ASSUME"
				}
				if !strings.Contains(result.Body.String(), code) {
					t.Fatal("error stages are not distinct")
				}
			case "storage error":
				storageUnavailable.Store(true)
				result := pocRequest(t, srv, "GET", "/api/v0/installer-role-config", pocAccount, "", 503)
				if strings.Contains(result.Body.String(), "SECRET_TOKEN") {
					t.Fatal("storage details leaked")
				}
			}
		})
	}
}
