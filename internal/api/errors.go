package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	bigav1 "github.com/biga-app/cli/internal/api/apipb/biga/v1"
)

// ErrManifestUnavailable is returned by [Client.Manifest] when the server
// predates the manifest endpoint, so callers can fall back to a full upload.
var ErrManifestUnavailable = errors.New("session manifest endpoint is unavailable")

// StatusError reports a non-2xx response.
type StatusError struct {
	StatusCode int
	// Code is the machine-readable error code from the response envelope,
	// when the server sent one.
	Code    string
	Message string
	// RetryAfter is the delay the server asked for, if any.
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("request failed (%d %s)", e.StatusCode, http.StatusText(e.StatusCode))
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// Temporary reports whether retrying the request might succeed.
func (e *StatusError) Temporary() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// UnreachableError reports that the server could not be reached at all:
// refused, unresolvable, timed out, or reset.
type UnreachableError struct {
	URL string
	Err error
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("cannot reach %s (%s)", e.URL, cause(e.Err))
}

func (e *UnreachableError) Unwrap() error { return e.Err }

// cause returns a short description of a transport failure, without the
// method, URL, and address prefixes net/http and net stack on top of it.
func cause(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	for {
		inner := errors.Unwrap(err)
		if inner == nil {
			return err.Error()
		}
		err = inner
	}
}

// HasStatus reports whether err is a [StatusError] with one of the codes.
func HasStatus(err error, codes ...int) bool {
	se, ok := errors.AsType[*StatusError](err)
	if !ok {
		return false
	}
	for _, code := range codes {
		if se.StatusCode == code {
			return true
		}
	}
	return false
}

// IsPermanent reports whether err is a client error that retrying cannot fix:
// any 4xx response other than 429.
func IsPermanent(err error) bool {
	se, ok := errors.AsType[*StatusError](err)
	return ok && se.StatusCode >= 400 && se.StatusCode < 500 && !se.Temporary()
}

// IsOwnershipError reports whether the server refused a write because this
// client cannot prove it owns the session: no token, a wrong one, or a
// session created before owner tokens existed. Retrying cannot fix it; the
// broadcast needs a session of its own.
func IsOwnershipError(err error) bool {
	se, ok := errors.AsType[*StatusError](err)
	if !ok {
		return false
	}
	switch se.Code {
	case bigav1.ErrorCodeUnauthorized, bigav1.ErrorCodeForbidden, bigav1.ErrorCodeSessionLocked:
		return se.StatusCode == http.StatusUnauthorized || se.StatusCode == http.StatusForbidden
	}
	return false
}

// IsAccessKeyError reports whether the server wants an access key this
// client does not have, or rejected the one it sent.
func IsAccessKeyError(err error) bool {
	se, ok := errors.AsType[*StatusError](err)
	return ok && (se.Code == bigav1.ErrorCodeAccessKeyRequired || se.Code == bigav1.ErrorCodeAccessKeyInvalid)
}

// IsMissingBlob reports whether the server refused a snapshot because some
// file content it refers to was never uploaded, usually because the file
// changed while uploading.
func IsMissingBlob(err error) bool {
	se, ok := errors.AsType[*StatusError](err)
	return ok && se.Code == bigav1.ErrorCodeMissingBlob
}

// IsUnreachable reports whether err means the server could not be reached.
func IsUnreachable(err error) bool {
	_, ok := errors.AsType[*UnreachableError](err)
	return ok
}

// parseRetryAfter reads a Retry-After header given in seconds or as an HTTP
// date, returning zero when it is absent or malformed.
func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}
