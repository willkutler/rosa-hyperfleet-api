package awsaccess

// ValidationError exposes a service-authored explanation without AWS response
// bodies or credentials. Cause remains available for error classification only.
type ValidationError struct {
	Reason string
	Cause  error
}

func (e *ValidationError) Error() string { return ErrValidation.Error() + ": " + e.Reason }

func (e *ValidationError) Unwrap() []error {
	if e.Cause == nil {
		return []error{ErrValidation}
	}
	return []error{ErrValidation, e.Cause}
}

func invalid(reason string) error { return &ValidationError{Reason: reason} }
