package awsaccess

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws/arn"
)

var trustRolePrincipalPattern = regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/([!-~]+/)?[A-Za-z0-9_+=,.@-]{1,64}$`)

// trustValues normalizes IAM's equivalent scalar/array forms during policy
// validation. JSON decoding itself uses encoding/json without custom unmarshaling.
func trustValues(raw json.RawMessage) ([]string, error) {
	var single string
	var values []string
	if json.Unmarshal(raw, &single) == nil {
		values = []string{single}
	} else if err := json.Unmarshal(raw, &values); err != nil {
		return nil, invalid("trust values must be strings or arrays of strings")
	}
	if len(values) == 0 {
		return nil, invalid("empty trust value")
	}
	for _, value := range values {
		if value == "" {
			return nil, invalid("empty trust value")
		}
	}
	return values, nil
}

type trustStatement struct {
	Sid       string                                `json:"Sid,omitempty"`
	Effect    string                                `json:"Effect"`
	Principal map[string]json.RawMessage            `json:"Principal"`
	Action    json.RawMessage                       `json:"Action"`
	Condition map[string]map[string]json.RawMessage `json:"Condition,omitempty"`
}

// ValidateTrust accepts explicit AWS role principals, including other environments,
// while rejecting broad/ambiguous trust and every applicable ExternalId bypass.
// Inspection is intentionally conservative: unsupported shapes require manual repair.
func ValidateTrust(document, jumpRoleARN, account string) error {
	if !strings.HasPrefix(strings.TrimSpace(document), "{") {
		decoded, err := url.QueryUnescape(document)
		if err != nil {
			return invalid("invalid URL-encoded trust document")
		}
		document = decoded
	}
	var policy struct {
		Version   string          `json:"Version"`
		ID        string          `json:"Id,omitempty"`
		Statement json.RawMessage `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(document), &policy); err != nil || policy.Version != "2012-10-17" {
		return invalid("unsupported or ambiguous trust document")
	}
	if err := validateTrustFields(document); err != nil {
		return err
	}
	raw := bytes.TrimSpace(policy.Statement)
	var statements []trustStatement
	if len(raw) > 0 && raw[0] == '{' {
		var statement trustStatement
		if err := json.Unmarshal(raw, &statement); err != nil {
			return &ValidationError{Reason: "unsupported or ambiguous trust statement", Cause: err}
		}
		statements = []trustStatement{statement}
	} else if err := json.Unmarshal(raw, &statements); err != nil {
		return &ValidationError{Reason: "unsupported or ambiguous trust statements", Cause: err}
	}
	found := false
	for _, statement := range statements {
		if (statement.Effect != "Allow" && statement.Effect != "Deny") || len(statement.Principal) != 1 || len(statement.Principal["AWS"]) == 0 {
			return invalid("trust requires explicit AWS role principals and Allow/Deny effects")
		}
		actions, err := trustValues(statement.Action)
		if err != nil {
			return err
		}
		principals, err := trustValues(statement.Principal["AWS"])
		if err != nil {
			return err
		}
		assume := false
		for _, action := range actions {
			switch strings.ToLower(action) {
			case "sts:assumerole":
				assume = true
			case "sts:tagsession", "sts:setsourceidentity":
			default:
				return invalid("unsupported or wildcard trust action")
			}
		}
		applies := false
		for _, principal := range principals {
			parsed, err := arn.Parse(principal)
			if err != nil || parsed.Partition != "aws" || parsed.Service != "iam" || parsed.Region != "" || !trustRolePrincipalPattern.MatchString(principal) || strings.ContainsAny(principal, "*?") {
				return invalid("wildcard, account-wide or unsupported trust principal")
			}
			applies = applies || principal == jumpRoleARN
		}
		for operator, conditions := range statement.Condition {
			if operator == "" || len(conditions) == 0 {
				return invalid("unsupported empty trust condition")
			}
			for key, rawValues := range conditions {
				if key == "" {
					return invalid("unsupported empty trust condition")
				}
				if _, err := trustValues(rawValues); err != nil {
					return err
				}
			}
		}
		if statement.Effect == "Allow" && assume && applies {
			values, err := trustValues(statement.Condition["StringEquals"]["sts:ExternalId"])
			if err != nil || len(values) != 1 || values[0] != account {
				return invalid("every Allow for this jump role must require StringEquals sts:ExternalId equal to the customer account")
			}
			found = true
		}
	}
	if !found {
		return invalid("no explicit ExternalId-protected Allow for this environment's jump role")
	}
	return nil
}

// validateTrustFields rejects ambiguous or unsupported IAM policy fields before
// the normalized statements are evaluated. This is specific to trust validation;
// HTTP requests and stored records use ordinary JSON decoding.
func validateTrustFields(document string) error {
	return validateTrustObjectFields(json.NewDecoder(strings.NewReader(document)), "", 0)
}

func validateTrustObjectFields(d *json.Decoder, path string, depth int) error {
	if depth > 64 {
		return invalid("trust policy nesting exceeds 64 levels")
	}
	token, err := d.Token()
	if err != nil {
		return invalid("malformed trust policy")
	}
	switch token {
	case json.Delim('{'):
		seen := map[string]bool{}
		for d.More() {
			token, err := d.Token()
			if err != nil {
				return invalid("malformed trust policy field")
			}
			key, ok := token.(string)
			if !ok || seen[key] {
				return invalid("duplicate or invalid trust policy field")
			}
			seen[key] = true
			switch path {
			case "":
				if key != "Version" && key != "Id" && key != "Statement" {
					return invalid("unsupported trust policy field")
				}
			case "Statement":
				if key != "Sid" && key != "Effect" && key != "Principal" && key != "Action" && key != "Condition" {
					return invalid("unsupported trust statement field")
				}
			case "Statement.Principal":
				if key != "AWS" {
					return invalid("unsupported trust principal field")
				}
			}
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			if err := validateTrustObjectFields(d, childPath, depth+1); err != nil {
				return err
			}
		}
		_, err = d.Token()
	case json.Delim('['):
		for d.More() {
			if err := validateTrustObjectFields(d, path, depth+1); err != nil {
				return err
			}
		}
		_, err = d.Token()
	}
	if err != nil {
		return invalid("malformed trust policy")
	}
	return nil
}
