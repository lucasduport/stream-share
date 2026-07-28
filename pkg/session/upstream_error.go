/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package session

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"strconv"
)

// Synthetic catalog keys for failures that never produced an HTTP response.
const (
	KeyUnreachable = "UNREACHABLE"
	KeyTimeout     = "TIMEOUT"
	KeyDNS         = "DNS"
	KeyTLS         = "TLS"
)

// UpstreamError describes why an upstream stream could not be started. Key is
// the lookup key into the error catalog: the HTTP status as a string ("403") for
// responses, or one of the synthetic constants above for transport failures.
type UpstreamError struct {
	Key        string
	StatusCode int // 0 when the failure was not an HTTP response
	Err        error
}

// Error implements the error interface for logging.
func (e *UpstreamError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err != nil {
		return e.Key + ": " + e.Err.Error()
	}
	return e.Key
}

// classifyUpstream maps a transport error or an HTTP status onto a catalog key.
// Exactly one of err / status is expected to be meaningful; when both are set the
// transport error wins, since it happened first.
func classifyUpstream(err error, status int) *UpstreamError {
	if err != nil {
		return &UpstreamError{Key: transportKey(err), Err: err}
	}
	if status > 0 {
		return &UpstreamError{Key: strconv.Itoa(status), StatusCode: status}
	}
	return &UpstreamError{Key: KeyUnreachable}
}

// transportKey distinguishes the transport failure modes worth telling a viewer
// apart. Order matters: a DNS failure is also a net.Error, and a timeout wrapping
// a DNS error should still read as a timeout.
func transportKey(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return KeyTimeout
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return KeyTimeout
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return KeyDNS
	}

	// Certificate problems are worth separating from a plain refused connection:
	// they usually mean a misconfigured provider rather than an outage. These
	// x509 types are stable across Go versions, unlike tls wrapper errors.
	var unknownAuthority x509.UnknownAuthorityError
	var certInvalid x509.CertificateInvalidError
	var hostnameErr x509.HostnameError
	if errors.As(err, &unknownAuthority) || errors.As(err, &certInvalid) || errors.As(err, &hostnameErr) {
		return KeyTLS
	}

	return KeyUnreachable
}
