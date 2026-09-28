package mqtt

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"

	"github.com/eclipse/paho.golang/paho"
	packetsv3 "github.com/eclipse/paho.mqtt.golang/packets"

	"github.com/ravenmk2/muxcat/internal/output"
)

// sanitizeErr renders an error for output with the plaintext password (and
// its percent-encoded forms) scrubbed out. Dial errors may embed the
// credential through a wrapped dial URI.
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

// classifyDial maps transport/handshake-phase failures shared by both
// protocols. password scrubs credentials out of any embedded dial URI.
func classifyDial(err error, password string) error {
	low := strings.ToLower(sanitizeErr(err, password))
	switch {
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(low, "i/o timeout"),
		strings.Contains(low, "deadline exceeded"):
		return output.NewError(output.CodeTimeout,
			"connection timed out: "+sanitizeErr(err, password), "increase --timeout or check the server")
	case isNetFail(err):
		return output.NewError(output.CodeConnectFailed,
			"failed to connect: "+sanitizeErr(err, password),
			"check the url of the instance and that the server is reachable")
	default:
		return output.NewError(output.CodeConnectFailed,
			"failed to connect: "+sanitizeErr(err, password),
			"check the url, protocol version and credentials of the connection")
	}
}

// classifyDialV3 maps 3.1.1 connect-phase failures: the connack refusal
// sentinels, or the shared transport classifier.
func classifyDialV3(err error, password string) error {
	switch {
	case errors.Is(err, packetsv3.ErrorRefusedBadUsernameOrPassword),
		errors.Is(err, packetsv3.ErrorRefusedNotAuthorised):
		return output.NewError(output.CodeAuthFailed,
			"authentication failed: "+sanitizeErr(err, password),
			"check the username/password of the connection")
	case errors.Is(err, packetsv3.ErrorRefusedBadProtocolVersion):
		return output.NewError(output.CodeConnectFailed,
			"protocol version refused: "+sanitizeErr(err, password),
			"the broker rejected MQTT 3.1.1; check --protocol-version")
	case errors.Is(err, packetsv3.ErrorRefusedIDRejected):
		return output.NewError(output.CodeConnectFailed,
			"client id rejected: "+sanitizeErr(err, password),
			"check the connection's clientId (or --client-id)")
	case errors.Is(err, packetsv3.ErrorRefusedServerUnavailable):
		return output.NewError(output.CodeConnectFailed,
			"server unavailable: "+sanitizeErr(err, password),
			"the broker refused the connection; check that the listener serves MQTT")
	default:
		return classifyDial(err, password)
	}
}

// classifyDialV5 maps 5.0 connect-phase failures. A connack with a reason
// code >= 0x80 is classified by the reason code; everything else is a
// transport failure.
func classifyDialV5(err error, ca *paho.Connack, password string) error {
	if ca != nil && ca.ReasonCode >= 0x80 {
		return classifyConnackV5(ca.ReasonCode)
	}
	return classifyDial(err, password)
}

// classifyConnackV5 maps MQTT 5.0 connack reason codes.
func classifyConnackV5(rc byte) error {
	switch rc {
	case 0x86: // Bad User Name or Password
		return output.NewError(output.CodeAuthFailed,
			"authentication failed: bad username or password",
			"check the username/password of the connection")
	case 0x87: // Not authorized
		return output.NewError(output.CodeAuthFailed,
			"authentication failed: not authorized",
			"check the username/password of the connection and the ACL permissions")
	case 0x84: // Unsupported Protocol Version
		return output.NewError(output.CodeConnectFailed,
			"protocol version refused: the broker does not support MQTT 5.0",
			"retry with --protocol-version 3")
	case 0x85: // Client Identifier not valid
		return output.NewError(output.CodeConnectFailed,
			"client id rejected", "check the connection's clientId (or --client-id)")
	case 0x88, 0x89: // Server unavailable / Server busy
		return output.NewError(output.CodeConnectFailed,
			"server unavailable or busy", "retry later, or check the broker")
	case 0x8A: // Banned
		return output.NewError(output.CodeAuthFailed,
			"connection refused: the client is banned", "check the broker's ban list")
	default:
		return output.NewError(output.CodeConnectFailed,
			"connection refused by the broker (reason code 0x"+hexByte(rc)+")",
			"check the broker logs for the refusal reason")
	}
}

// classifyV3 maps 3.1.1 operation-phase errors (publish/subscribe).
func classifyV3(err error, password string) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return output.NewError(output.CodeTimeout, "operation timed out: "+sanitizeErr(err, password),
			"increase --timeout or check the server")
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return output.NewError(output.CodeTimeout, "operation timed out: "+sanitizeErr(err, password),
			"increase --timeout or check the server")
	}
	var oerr *output.Error
	if errors.As(err, &oerr) {
		return oerr
	}
	return output.NewError(output.CodeQueryError, truncate(sanitizeErr(err, password), 300), "")
}

// classifyV5 maps 5.0 operation-phase errors (publish/subscribe).
func classifyV5(err error, password string) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return output.NewError(output.CodeTimeout, "operation timed out: "+sanitizeErr(err, password),
			"increase --timeout or check the server")
	}
	return output.NewError(output.CodeQueryError, truncate(sanitizeErr(err, password), 300), "")
}

// hexByte renders a byte as two hex digits (for reason codes).
func hexByte(b byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[b>>4], digits[b&0x0f]})
}
