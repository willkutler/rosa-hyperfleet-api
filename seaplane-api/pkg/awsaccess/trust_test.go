package awsaccess

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/accountaccess"
)

const (
	customerAccount = "123456789012"
	firstJump       = "arn:aws:iam::210987654321:role/env/first-jump"
	secondJump      = "arn:aws:iam::210987654321:role/env/second-jump"
)

func TestSharedRoleTrust(t *testing.T) {
	first := string(accountaccess.TrustPolicy(firstJump, customerAccount))
	var policy map[string]any
	if err := json.Unmarshal([]byte(first), &policy); err != nil {
		t.Fatal(err)
	}
	var second map[string]any
	_ = json.Unmarshal(accountaccess.TrustPolicy(secondJump, customerAccount), &second)
	policy["Statement"] = append(policy["Statement"].([]any), second["Statement"].([]any)...)
	shared, _ := json.Marshal(policy)
	for _, jump := range []string{firstJump, secondJump} {
		if err := ValidateTrust(string(shared), jump, customerAccount); err != nil {
			t.Errorf("shared trust for %s: %v", jump, err)
		}
		if err := ValidateTrust(url.QueryEscape(string(shared)), jump, customerAccount); err != nil {
			t.Errorf("URL-encoded trust: %v", err)
		}
	}
	// Equivalent single statement, principal/action/condition arrays are supported.
	statement := policy["Statement"].([]any)[0].(map[string]any)
	statement["Principal"] = map[string]any{"AWS": []string{firstJump}}
	statement["Action"] = []string{"sts:AssumeRole", "sts:TagSession"}
	statement["Condition"] = map[string]any{"StringEquals": map[string]any{"sts:ExternalId": []string{customerAccount}}}
	policy["Statement"] = statement
	one, _ := json.Marshal(policy)
	if err := ValidateTrust(string(one), firstJump, customerAccount); err != nil {
		t.Fatalf("equivalent single/array forms rejected: %v", err)
	}
}

func TestTrustRejectsExternalIDBypassesAndAmbiguity(t *testing.T) {
	valid := string(accountaccess.TrustPolicy(firstJump, customerAccount))
	var policy struct {
		Version   string
		Statement []json.RawMessage
	}
	_ = json.Unmarshal([]byte(valid), &policy)
	withoutID := strings.Replace(valid, `"Condition":{"StringEquals":{"sts:ExternalId":"123456789012"}},`, "", 1)
	// Marshal an unprotected second statement without relying on field order.
	var unprotected map[string]any
	_ = json.Unmarshal(policy.Statement[0], &unprotected)
	delete(unprotected, "Condition")
	extra, _ := json.Marshal(unprotected)
	withBypass := `{"Version":"2012-10-17","Statement":[` + string(policy.Statement[0]) + "," + string(extra) + "]}"
	for _, tc := range []struct{ name, document string }{
		{"without external id", withoutID},
		{"additional bypass", withBypass},
		{"wrong external id", strings.Replace(valid, `"123456789012"`, `"999999999999"`, 1)},
		{"IfExists", strings.Replace(valid, "StringEquals", "StringEqualsIfExists", 1)},
		{"StringLike", strings.Replace(valid, "StringEquals", "StringLike", 1)},
		{"multiple ids", strings.Replace(valid, `"sts:ExternalId":"123456789012"`, `"sts:ExternalId":["123456789012","999999999999"]`, 1)},
		{"wildcard principal", strings.Replace(valid, firstJump, "*", 1)},
		{"wildcard role", strings.Replace(valid, firstJump, "arn:aws:iam::210987654321:role/*", 1)},
		{"account-wide", strings.Replace(valid, firstJump, "arn:aws:iam::210987654321:root", 1)},
		{"unrelated role only", strings.Replace(valid, firstJump, secondJump, 1)},
		{"NotPrincipal", strings.Replace(valid, "Principal", "NotPrincipal", 1)},
		{"NotAction", strings.Replace(valid, "Action", "NotAction", 1)},
		{"wildcard action", strings.Replace(valid, "sts:AssumeRole", "sts:*", 1)},
		{"deny only", strings.Replace(valid, "Allow", "Deny", 1)},
		{"wrong version", strings.Replace(valid, "2012-10-17", "2008-10-17", 1)},
		{"unknown policy field", strings.Replace(valid, `"Version":`, `"Unexpected":true,"Version":`, 1)},
		{"duplicate policy field", strings.Replace(valid, `"Version":`, `"Version":"2012-10-17","Version":`, 1)},
		{"duplicate statement field", strings.Replace(valid, `"Effect":`, `"Effect":"Allow","Effect":`, 1)},
		{"duplicate principal field", strings.Replace(valid, `"AWS":`, `"AWS":"`+firstJump+`","AWS":`, 1)},
		{"duplicate condition", strings.Replace(valid, `"sts:ExternalId":"123456789012"`, `"sts:ExternalId":"123456789012","sts:ExternalId":"other"`, 1)},
		{"empty action", strings.Replace(valid, `"sts:AssumeRole"`, `[]`, 1)},
		{"non-string action", strings.Replace(valid, `"sts:AssumeRole"`, `true`, 1)},
		{"empty principal", strings.Replace(valid, `"`+firstJump+`"`, `[]`, 1)},
		{"empty condition value", strings.Replace(valid, `"sts:ExternalId":"123456789012"`, `"sts:ExternalId":[]`, 1)},
		{"non-string condition value", strings.Replace(valid, `"sts:ExternalId":"123456789012"`, `"sts:ExternalId":true`, 1)},
		{"empty statement", `{"Version":"2012-10-17","Statement":[]}`},
		{"null", "null"},
		{"trailing JSON", valid + "{}"},
		{"malformed", "{"},
		{"malformed URL", "%XX"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateTrust(tc.document, firstJump, customerAccount); err == nil {
				t.Fatalf("unsafe/ambiguous trust accepted: %s", tc.document)
			}
		})
	}
}
