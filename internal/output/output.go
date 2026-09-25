// Package output defines muxcat's output contract: envelope, renderers,
// TTY/degradation detection, error codes, and exit codes.
package output

import (
	"encoding/json"
	"errors"
	"io"
	"os"
)

// Exit codes. Defined centrally; referenced uniformly across the program.
const (
	ExitOK      = 0 // success
	ExitGeneral = 1 // generic error
	ExitUsage   = 2 // usage errors and missing arguments
	ExitConnect = 3 // connection failure
	ExitAuth    = 4 // authentication failure
	ExitExec    = 5 // execution error
)

// Error code enum.
const (
	CodeMissingArgument      = "MISSING_ARGUMENT"
	CodeConnNotFound         = "CONN_NOT_FOUND"
	CodeConfigInvalid        = "CONFIG_INVALID"
	CodeKeyUnavailable       = "KEY_UNAVAILABLE"
	CodeConnectFailed        = "CONNECT_FAILED"
	CodeAuthFailed           = "AUTH_FAILED"
	CodeTimeout              = "TIMEOUT"
	CodeQueryError           = "QUERY_ERROR"
	CodeReadonlyViolation    = "READONLY_VIOLATION"
	CodeUnsupportedOperation = "UNSUPPORTED_OPERATION"
	CodeConnectorUnknown     = "CONNECTOR_UNKNOWN"
	CodeUpgradeFailed        = "UPGRADE_FAILED"
	CodeGeneral              = "GENERAL" // fallback outside the enum, only for wrapping unknown errors
)

// Error is a structured command error with an error code and optional hint.
type Error struct {
	Code    string
	Message string
	Hint    string
}

func (e *Error) Error() string { return e.Message }

// NewError constructs a structured error. hint may be empty.
func NewError(code, message, hint string) *Error {
	return &Error{Code: code, Message: message, Hint: hint}
}

// ToError normalizes any error into *Error: passes through existing
// *Error values, wraps everything else as CodeGeneral.
func ToError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: CodeGeneral, Message: err.Error()}
}

// ExitCode maps an error code to an exit code.
func ExitCode(err error) int {
	e := ToError(err)
	switch e.Code {
	case CodeMissingArgument, CodeConnNotFound, CodeConfigInvalid,
		CodeUnsupportedOperation, CodeConnectorUnknown:
		return ExitUsage
	case CodeConnectFailed, CodeTimeout:
		return ExitConnect
	case CodeAuthFailed:
		return ExitAuth
	case CodeQueryError, CodeReadonlyViolation, CodeUpgradeFailed:
		return ExitExec
	default:
		return ExitGeneral
	}
}

// Meta is the envelope meta field, filled in by commands.
type Meta struct {
	Connector  string `json:"connector"`
	Connection string `json:"connection"`
	ElapsedMS  int64  `json:"elapsed_ms"`
	Truncated  bool   `json:"truncated"`
}

// ErrorBody is the envelope error field.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// Envelope is muxcat's fixed output shape:
// {ok, data, meta:{connector, connection, elapsed_ms, truncated}, error:{code, message, hint?}}
type Envelope struct {
	OK    bool       `json:"ok"`
	Data  any        `json:"data,omitempty"`
	Meta  Meta       `json:"meta"`
	Error *ErrorBody `json:"error,omitempty"`
}

// Success constructs a success envelope.
func Success(data any, meta Meta) Envelope {
	return Envelope{OK: true, Data: data, Meta: meta}
}

// Failure constructs a failure envelope.
func Failure(e *Error, meta Meta) Envelope {
	return Envelope{
		OK:    false,
		Meta:  meta,
		Error: &ErrorBody{Code: e.Code, Message: e.Message, Hint: e.Hint},
	}
}

// WriteEnvelope writes an envelope as 2-space-indented JSON.
func WriteEnvelope(w io.Writer, env Envelope) error {
	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

// Mode is the output mode.
type Mode string

const (
	ModeAuto  Mode = "auto"
	ModeTable Mode = "table"
	ModePlain Mode = "plain"
	ModeTSV   Mode = "tsv"
	ModeJSON  Mode = "json"
)

func validMode(m string) bool {
	switch Mode(m) {
	case ModeAuto, ModeTable, ModePlain, ModeTSV, ModeJSON:
		return true
	}
	return false
}

// ResolveMode resolves the output mode, priority: --json > --output >
// configured default > auto. Under auto: TTY→table, non-TTY→plain.
func ResolveMode(flagOutput string, flagJSON bool, def string, tty bool) (Mode, error) {
	if flagJSON {
		return ModeJSON, nil
	}
	if flagOutput != "" && flagOutput != string(ModeAuto) {
		if !validMode(flagOutput) {
			return "", NewError(CodeConfigInvalid, "invalid --output value: "+flagOutput, "valid values: auto|table|plain|tsv|json")
		}
		return Mode(flagOutput), nil
	}
	if def != "" && def != string(ModeAuto) {
		if !validMode(def) {
			return "", NewError(CodeConfigInvalid, "invalid props.defaults.output value: "+def, "valid values: auto|table|plain|tsv|json")
		}
		return Mode(def), nil
	}
	if tty {
		return ModeTable, nil
	}
	return ModePlain, nil
}

// ResolveColor decides whether to enable colored output.
// Rules: non-TTY forces no color; --no-color and the NO_COLOR environment
// variable each veto color; props.defaults.color is tri-state
// auto|always|never (auto means color when on a TTY).
func ResolveColor(def string, noColorFlag bool, tty bool) bool {
	if !tty || noColorFlag {
		return false
	}
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	switch def {
	case "always":
		return true
	case "never":
		return false
	default: // auto
		return true
	}
}
