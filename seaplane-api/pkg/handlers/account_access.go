package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	accountv1 "github.com/openshift-online/rosa-seaplane-api/api/accountaccess/v1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/accountaccess"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/api"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/awsaccess"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/middleware"
)

type CustomerAWSAccess interface {
	Validate(context.Context, accountaccess.AWSAccountAccess, string) error
	ListVPCs(context.Context, accountaccess.AWSAccountAccess, string) ([]accountv1.VPC, error)
}

type AccountAccessHandler struct {
	config  config.InstallerRoleConfig
	store   accountaccess.Store
	aws     CustomerAWSAccess
	timeout time.Duration
	logger  *slog.Logger
}

func NewAccountAccessHandler(cfg config.InstallerRoleConfig, store accountaccess.Store, aws CustomerAWSAccess, timeout time.Duration, logger *slog.Logger) *AccountAccessHandler {
	return &AccountAccessHandler{config: cfg, store: store, aws: aws, timeout: timeout, logger: logger}
}

// prepare repeats identity validation at the handler boundary before any storage
// or customer calls. Headers are authenticated only by the private backend's
// gateway-only ingress; this code does not independently verify SigV4.
func (h *AccountAccessHandler) prepare(w http.ResponseWriter, r *http.Request, readOnly bool) (*http.Request, context.CancelFunc, bool) {
	identity := authz.Identity{AccountID: middleware.GetAccountID(r.Context()), CallerARN: middleware.GetCallerARN(r.Context())}
	if err := identity.Validate(); err != nil {
		writeAPIError(w, api.APIError{Code: "AUTH-001", HTTPStatus: 403, Message: "Caller identity is invalid"}, h.logger)
		return r, func() {}, false
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery || (readOnly && (r.ContentLength != 0 || len(r.TransferEncoding) != 0)) {
		h.fail(w, r, accountaccess.ErrInvalidInput)
		return r, func() {}, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	return r.WithContext(ctx), cancel, true
}

func (h *AccountAccessHandler) Initialize(w http.ResponseWriter, r *http.Request) {
	r, cancel, ok := h.prepare(w, r, false)
	defer cancel()
	if !ok {
		return
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var input *accountv1.InitializeAWSAccountAccessRequest
	if err := decoder.Decode(&input); err != nil || input == nil {
		h.fail(w, r, accountaccess.ErrInvalidInput)
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		h.fail(w, r, accountaccess.ErrInvalidInput)
		return
	}
	if _, _, err := accountaccess.DeriveRole(middleware.GetAccountID(r.Context()), input.Prefix, input.Path); err != nil {
		h.fail(w, r, err)
		return
	}
	record, created, err := h.store.InitializeAWSAccountAccess(r.Context(), middleware.GetAccountID(r.Context()), input.Prefix, input.Path)
	if err == nil {
		err = record.Validate(h.config.EnvironmentID, middleware.GetAccountID(r.Context()))
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	h.respond(w, r, status, record.Response())
}

func (h *AccountAccessHandler) Configuration(w http.ResponseWriter, r *http.Request) {
	r, cancel, ok := h.prepare(w, r, true)
	defer cancel()
	if !ok {
		return
	}
	account := middleware.GetAccountID(r.Context())
	record, err := h.store.GetAWSAccountAccess(r.Context(), account)
	if err == nil {
		err = record.Validate(h.config.EnvironmentID, account)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.respond(w, r, http.StatusOK, accountv1.InstallerRoleConfigResponse{
		Assignment: record.Response(), AccountID: account, EnvironmentID: h.config.EnvironmentID,
		RoleName: accountaccess.RoleName(record), Path: record.Path, InstallerRoleARN: record.InstallerRoleARN,
		ExternalID: account, JumpRoleARN: h.config.JumpRoleARN, InstallerManagedPolicyARN: h.config.InstallerManagedPolicyARN,
		TrustPolicy: accountaccess.TrustPolicy(h.config.JumpRoleARN, account), RequiredTags: accountaccess.RequiredTags(),
		ConfigurationRevision: accountaccess.ConfigurationRevision(h.config),
	})
}

func (h *AccountAccessHandler) Complete(w http.ResponseWriter, r *http.Request) {
	r, cancel, ok := h.prepare(w, r, false)
	defer cancel()
	if !ok {
		return
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var input *accountv1.ValidateAWSAccountAccessRequest
	if err := decoder.Decode(&input); err != nil || input == nil {
		h.fail(w, r, accountaccess.ErrInvalidInput)
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		h.fail(w, r, accountaccess.ErrInvalidInput)
		return
	}
	if !accountaccess.ValidVersion(input.AssignmentID) || !accountaccess.ValidVersion(input.ResourceVersion) || input.ConfigurationRevision == "" {
		h.fail(w, r, accountaccess.ErrInvalidInput)
		return
	}
	if input.ConfigurationRevision != accountaccess.ConfigurationRevision(h.config) {
		h.fail(w, r, accountaccess.ErrConflict)
		return
	}
	account := middleware.GetAccountID(r.Context())
	record, err := h.store.GetAWSAccountAccess(r.Context(), account)
	if err == nil {
		err = record.Validate(h.config.EnvironmentID, account)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if record.AssignmentID != input.AssignmentID || record.ResourceVersion != input.ResourceVersion {
		h.fail(w, r, accountaccess.ErrConflict)
		return
	}
	if err := h.aws.Validate(r.Context(), record, account); err != nil {
		if record.State == accountv1.AWSAccountAccessReady {
			// Reserve a bounded cleanup window within the HTTP operation budget even
			// when AWS exhausted/canceled its context. CAS cannot overwrite a newer result.
			cleanup, cleanupCancel := context.WithTimeout(context.WithoutCancel(r.Context()), min(time.Second, h.timeout/10))
			_, resetErr := h.store.MarkAWSAccountAccessPending(cleanup, record)
			cleanupCancel()
			if resetErr != nil {
				h.fail(w, r, resetErr)
				return
			}
		}
		h.fail(w, r, err)
		return
	}
	updated, err := h.store.MarkAWSAccountAccessReady(r.Context(), record, input.ConfigurationRevision)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.respond(w, r, http.StatusOK, updated.Response())
}

func (h *AccountAccessHandler) ListVPCs(w http.ResponseWriter, r *http.Request) {
	r, cancel, ok := h.prepare(w, r, true)
	defer cancel()
	if !ok {
		return
	}
	account := middleware.GetAccountID(r.Context())
	record, err := h.store.GetAWSAccountAccess(r.Context(), account)
	if err == nil {
		err = record.Validate(h.config.EnvironmentID, account)
	}
	if err == nil && (record.State != accountv1.AWSAccountAccessReady || record.ValidatedConfigurationRevision != accountaccess.ConfigurationRevision(h.config)) {
		err = accountaccess.ErrSetupRequired
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items, err := h.aws.ListVPCs(r.Context(), record, account)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if items == nil {
		items = []accountv1.VPC{}
	}
	h.respond(w, r, http.StatusOK, accountv1.ListVPCsResponse{Items: items})
}

func (h *AccountAccessHandler) respond(w http.ResponseWriter, r *http.Request, status int, value any) {
	h.log(r, status, "")
	if err := api.Write(w, status, value); err != nil {
		h.logger.Error("failed to write account-access response", "request_id", middleware.GetRequestID(r.Context()))
	}
}

func (h *AccountAccessHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	def := api.APIError{Code: "ACCOUNT-ACCESS-STORAGE", HTTPStatus: 503, Message: "Account-access storage is unavailable"}
	switch {
	case errors.Is(err, accountaccess.ErrInvalidInput):
		def = api.APIError{Code: "ACCOUNT-ACCESS-INPUT", HTTPStatus: 400, Message: "Invalid account-access request; supply only the documented required fields"}
	case errors.Is(err, accountaccess.ErrConflict):
		def = api.APIError{Code: "ACCOUNT-ACCESS-CONFLICT", HTTPStatus: 409, Message: "Assignment selection, resourceVersion or approved configuration changed; read the configuration and retry"}
	case errors.Is(err, accountaccess.ErrSetupRequired):
		def = api.APIError{Code: "ACCOUNT-SETUP-REQUIRED", HTTPStatus: 409, Message: "Initialize and validate this account's installer role before discovery"}
	case errors.Is(err, accountaccess.ErrCorruptRecord):
		def = api.APIError{Code: "ACCOUNT-ACCESS-RECORD", HTTPStatus: 500, Message: "Stored assignment does not match the owning account and environment"}
	case errors.Is(err, awsaccess.ErrAssumeRole):
		def = api.APIError{Code: "ACCOUNT-ACCESS-ASSUME", HTTPStatus: 502, Message: "Unable to assume the jump or installer role"}
	case errors.Is(err, awsaccess.ErrValidation):
		def = api.APIError{Code: "ACCOUNT-ACCESS-VALIDATION", HTTPStatus: 422, Message: "Installer role failed IAM trust, policy, tag or ExternalId validation; retry after IAM changes propagate"}
		var validation *awsaccess.ValidationError
		if errors.As(err, &validation) {
			def.Message = validation.Error()
		}
	case errors.Is(err, awsaccess.ErrDiscovery):
		def = api.APIError{Code: "ACCOUNT-ACCESS-EC2", HTTPStatus: 502, Message: "Customer EC2 VPC discovery failed"}
	}
	// SDK errors may contain sensitive request/response details. Log only the stage.
	h.log(r, def.HTTPStatus, def.Code)
	writeAPIError(w, def, h.logger)
}

func (h *AccountAccessHandler) log(r *http.Request, status int, code string) {
	h.logger.Info("account-access request", "method", r.Method, "path", r.URL.Path, "status", status, "code", code,
		"account_id", middleware.GetAccountID(r.Context()), "caller_arn", middleware.GetCallerARN(r.Context()),
		"environment_id", h.config.EnvironmentID, "request_id", middleware.GetRequestID(r.Context()))
}
