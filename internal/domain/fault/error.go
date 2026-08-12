package fault

import "fmt"

// Code is a stable machine-readable error category.
type Code string

const (
	InvalidInput      Code = "InvalidInput"
	StaleInput        Code = "StaleInput"
	Unavailable       Code = "Unavailable"
	DeadlineExceeded  Code = "DeadlineExceeded"
	CapabilityMissing Code = "CapabilityMissing"
	PolicyBlocked     Code = "PolicyBlocked"
	PermissionDenied  Code = "PermissionDenied"
	AdapterRejected   Code = "AdapterRejected"
)

// Error adds a stable category and operation to an underlying error.
type Error struct {
	Code Code
	Op   string
	Err  error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("%s: %s", e.Op, e.Code)
	}
	return fmt.Sprintf("%s: %s: %v", e.Op, e.Code, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// New constructs a categorized error.
func New(code Code, op string, err error) error {
	return &Error{Code: code, Op: op, Err: err}
}

// IsCode reports whether err contains a categorized fault with code.
func IsCode(err error, code Code) bool {
	for err != nil {
		if typed, ok := err.(*Error); ok {
			if typed.Code == code {
				return true
			}
			err = typed.Err
			continue
		}
		type unwrapper interface{ Unwrap() error }
		typed, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = typed.Unwrap()
	}
	return false
}
