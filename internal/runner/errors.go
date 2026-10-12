package runner

import "errors"

// Failure kinds recorded as Step.ErrorKind, so reports can tell "the system
// under test is wrong" apart from "we couldn't reach it" and "the flow is wrong".
const (
	// KindAssertion: the system responded but not as the flow expected.
	KindAssertion = "assertion"
	// KindContract: the call disagrees with the published OpenAPI contract.
	KindContract = "contract"
	// KindTimeout: a poll's condition never became true within its window.
	KindTimeout = "timeout"
	// KindInfra: no usable response (timeout, connection refused, DNS, read error).
	KindInfra = "infrastructure"
	// KindSpec: the flow itself is broken (bad template, regex, URL, timeout value).
	KindSpec = "spec"
)

type kindError struct {
	kind string
	err  error
}

func (e *kindError) Error() string { return e.err.Error() }
func (e *kindError) Unwrap() error { return e.err }

func assertErr(err error) error    { return &kindError{KindAssertion, err} }
func contractErr(err error) error  { return &kindError{KindContract, err} }
func timeoutErr(err error) error   { return &kindError{KindTimeout, err} }
func transportErr(err error) error { return &kindError{KindInfra, err} }
func specErr(err error) error      { return &kindError{KindSpec, err} }

func kindOf(err error) string {
	var ke *kindError
	if errors.As(err, &ke) {
		return ke.kind
	}
	return KindSpec
}
