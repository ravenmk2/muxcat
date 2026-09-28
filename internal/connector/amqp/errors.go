package amqp

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"

	amqp10 "github.com/Azure/go-amqp"
	amqp091 "github.com/rabbitmq/amqp091-go"
	"github.com/rabbitmq/rabbitmq-amqp-go-client/pkg/rabbitmqamqp"

	"github.com/ravenmk2/muxcat/internal/output"
)

// sanitizeErr renders an error for output with the plaintext password (and
// its percent-encoded form, as it appears in dial URIs) scrubbed out. Dial
// errors may embed the full URI, which carries the credential.
func sanitizeErr(err error, password string) string {
	s := err.Error()
	if password == "" {
		return s
	}
	s = strings.ReplaceAll(s, password, "***")
	if enc := url.QueryEscape(password); enc != password {
		s = strings.ReplaceAll(s, enc, "***")
	}
	if enc := url.PathEscape(password); enc != password {
		s = strings.ReplaceAll(s, enc, "***")
	}
	return s
}

// isNetFail reports a TCP-level connect failure (refused, no such host,
// reset).
func isNetFail(err error) bool {
	low := strings.ToLower(err.Error())
	return strings.Contains(low, "connection refused") ||
		strings.Contains(low, "no such host") ||
		strings.Contains(low, "connection reset") ||
		strings.Contains(low, "forcibly closed")
}

// classifyDial maps handshake-phase failures (both protocols). password
// scrubs credentials out of any embedded dial URI.
func classifyDial(err error, password, protocol, vhost string) error {
	var aerr *amqp091.Error
	if errors.As(err, &aerr) {
		return classify091(aerr, password)
	}
	var aerr10 *amqp10.Error
	if errors.As(err, &aerr10) {
		return classifyDial10(aerr10, err, password, vhost)
	}
	low := strings.ToLower(sanitizeErr(err, password))
	switch {
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(low, "i/o timeout"),
		strings.Contains(low, "deadline exceeded"):
		return output.NewError(output.CodeTimeout,
			"connection timed out: "+sanitizeErr(err, password), "increase --timeout or check the server")
	case strings.Contains(low, "unauthorized-access"):
		return output.NewError(output.CodeAuthFailed,
			"authentication failed: "+sanitizeErr(err, password),
			"check the username/password of the connection")
	case strings.Contains(low, "auth failed"), strings.Contains(low, "access_refused"):
		// 1.0: SASL-layer credential rejection.
		return output.NewError(output.CodeAuthFailed,
			"authentication failed: "+sanitizeErr(err, password),
			"check the username/password of the connection")
	case strings.Contains(low, "sasl"):
		// An old broker that cannot complete the 1.0 handshake at all.
		if protocol == Protocol10 {
			return output.NewError(output.CodeConnectFailed,
				"AMQP 1.0 handshake failed: "+sanitizeErr(err, password),
				"AMQP 1.0 requires RabbitMQ >= 4.0; for older brokers use --protocol 0.9.1")
		}
		return output.NewError(output.CodeConnectFailed,
			"handshake failed: "+sanitizeErr(err, password), "check that the endpoint speaks AMQP "+protocol)
	case strings.Contains(low, "not-found"):
		// AMQP 1.0 open on a vhost that does not exist.
		return output.NewError(output.CodeConnectFailed,
			"connection failed: "+sanitizeErr(err, password),
			"check the vhost name (effective vhost: "+vhost+"); list vhosts with rmq vhost ls")
	case strings.Contains(low, "not_allowed") || strings.Contains(low, "not-allowed"):
		return output.NewError(output.CodeAuthFailed,
			"access refused: "+sanitizeErr(err, password),
			"check the (user, vhost) permissions; list them with rmq permission ls")
	case isNetFail(err):
		hint := "check the url of the instance and that the server is reachable"
		if protocol == Protocol10 &&
			(strings.Contains(low, "forcibly closed") || strings.Contains(low, "connection reset")) {
			// RabbitMQ answers an AMQP 1.0 open on a nonexistent vhost by
			// dropping the TCP connection mid-handshake.
			hint = "the vhost may not exist (effective vhost: " + vhost + "); list vhosts with rmq vhost ls. " + hint
		}
		return output.NewError(output.CodeConnectFailed,
			"failed to connect: "+sanitizeErr(err, password), hint)
	default:
		return output.NewError(output.CodeConnectFailed,
			"failed to connect: "+sanitizeErr(err, password),
			"check the url, protocol and credentials of the connection")
	}
}

// classifyDial10 maps an AMQP 1.0 open-phase error by its condition.
func classifyDial10(aerr *amqp10.Error, err error, password, vhost string) error {
	desc := sanitizeErr(errors.New(aerr.Description), password)
	if desc == "" {
		desc = sanitizeErr(err, password)
	}
	switch string(aerr.Condition) {
	case "amqp:unauthorized-access":
		return output.NewError(output.CodeAuthFailed,
			"authentication failed: "+desc,
			"check the username/password of the connection and the (user, vhost) permissions")
	case "amqp:not-found":
		return output.NewError(output.CodeConnectFailed,
			"connection failed: "+desc,
			"check the vhost name (effective vhost: "+vhost+"); list vhosts with rmq vhost ls")
	case "amqp:not-allowed":
		return output.NewError(output.CodeAuthFailed,
			"access refused: "+desc,
			"check the (user, vhost) permissions; list them with rmq permission ls")
	default:
		return output.NewError(output.CodeConnectFailed,
			"failed to connect: "+desc, "")
	}
}

// classify091 maps AMQP 0.9.1 errors to muxcat error codes.
func classify091(err error, password string) error {
	var aerr *amqp091.Error
	if !errors.As(err, &aerr) {
		if errors.Is(err, context.DeadlineExceeded) {
			return output.NewError(output.CodeTimeout, "operation timed out: "+sanitizeErr(err, password),
				"increase --timeout or check the server")
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return output.NewError(output.CodeTimeout, "operation timed out: "+sanitizeErr(err, password),
				"increase --timeout or check the server")
		}
		return output.NewError(output.CodeQueryError, sanitizeErr(err, password), "")
	}
	reason := sanitizeErr(errors.New(aerr.Reason), password)
	switch aerr.Code {
	case 403: // ACCESS_REFUSED
		// RabbitMQ uses 403 both for bad credentials and for a vhost that
		// does not exist or is not accessible to the user.
		if strings.Contains(aerr.Reason, "no access to this vhost") {
			return output.NewError(output.CodeAuthFailed,
				"access refused: "+reason,
				"check the vhost name and the (user, vhost) permissions; list vhosts with rmq vhost ls")
		}
		return output.NewError(output.CodeAuthFailed,
			"access refused: "+reason, "check the username/password of the connection")
	case 530: // NOT_ALLOWED (incl. vhost access refused at connection.open)
		return output.NewError(output.CodeAuthFailed,
			"not allowed: "+reason,
			"check the (user, vhost) permissions; list them with rmq permission ls")
	case 404: // NOT_FOUND
		return output.NewError(output.CodeQueryError,
			"not found: "+reason, "check the vhost and object name")
	case 405: // RESOURCE_LOCKED
		return output.NewError(output.CodeQueryError,
			"resource locked: "+reason, "the object is exclusive to another connection")
	case 406: // PRECONDITION_FAILED
		return output.NewError(output.CodeQueryError,
			"precondition failed: "+reason,
			"the object already exists with different parameters; delete it first or match its declaration")
	case 541: // INTERNAL_ERROR (4.x: deprecated feature refusals)
		return output.NewError(output.CodeQueryError,
			"internal server error (541)", reason)
	default:
		return output.NewError(output.CodeQueryError,
			"amqp error "+strconv.Itoa(aerr.Code)+": "+reason, "")
	}
}

// classify10 maps AMQP 1.0 errors to muxcat error codes: the typed
// management errors, amqp.Error conditions, and context timeouts.
func classify10(err error, password string) error {
	if errors.Is(err, rabbitmqamqp.ErrDoesNotExist) {
		return output.NewError(output.CodeQueryError,
			"not found: "+sanitizeErr(err, password), "check the vhost and object name")
	}
	if errors.Is(err, rabbitmqamqp.ErrPreconditionFailed) {
		return output.NewError(output.CodeQueryError,
			"precondition failed: "+sanitizeErr(err, password),
			"the object already exists with different parameters; delete it first or match its declaration")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return output.NewError(output.CodeTimeout, "operation timed out: "+sanitizeErr(err, password),
			"increase --timeout or check the server")
	}
	var aerr *amqp10.Error
	if errors.As(err, &aerr) {
		cond := string(aerr.Condition)
		desc := sanitizeErr(errors.New(aerr.Description), password)
		if desc == "" {
			desc = cond
		}
		switch cond {
		case "amqp:unauthorized-access":
			return output.NewError(output.CodeAuthFailed,
				"unauthorized: "+desc,
				"check the credentials and the (user, vhost) permissions; list them with rmq permission ls")
		case "amqp:not-found":
			return output.NewError(output.CodeQueryError,
				"not found: "+desc, "check the vhost and object name")
		case "amqp:resource-locked":
			return output.NewError(output.CodeQueryError,
				"resource locked: "+desc, "the object is exclusive to another connection")
		case "amqp:precondition-failed":
			return output.NewError(output.CodeQueryError,
				"precondition failed: "+desc,
				"the object already exists with different parameters; delete it first or match its declaration")
		default:
			return output.NewError(output.CodeQueryError, truncate(desc, 300), "")
		}
	}
	return output.NewError(output.CodeQueryError, truncate(sanitizeErr(err, password), 300), "")
}
