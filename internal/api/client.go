// Package api is a client for the biga session API.
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	bigav1 "github.com/biga-app/cli/internal/api/apipb/biga/v1"
)

// Client talks to a biga server. It is safe for concurrent use.
type Client struct {
	baseURL   string
	http      *http.Client
	userAgent string
	accessKey string
	backoff   func(attempt int) time.Duration

	// gzip records whether the server accepts gzip request bodies, which it
	// advertises with an Accept-Encoding response header (RFC 7694). Until
	// it does, bodies are sent uncompressed, so older servers keep working.
	gzip atomic.Bool
}

// Option configures a [Client].
type Option func(*Client)

// WithHTTPClient replaces the default HTTP client.
func WithHTTPClient(c *http.Client) Option {
	return func(cl *Client) { cl.http = c }
}

// WithBackoff replaces the delay schedule between retries; attempt starts
// at 1 for the wait after the first failure.
func WithBackoff(fn func(attempt int) time.Duration) Option {
	return func(cl *Client) { cl.backoff = fn }
}

// WithUserAgent sets the User-Agent header sent with every request.
func WithUserAgent(ua string) Option {
	return func(cl *Client) { cl.userAgent = ua }
}

// WithAccessKey sends key with every request, for servers that only accept
// broadcasts from people holding one.
func WithAccessKey(key string) Option {
	return func(cl *Client) { cl.accessKey = key }
}

// New returns a client for the server at baseURL, e.g. "https://biga.n10nce.dev".
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		http:      http.DefaultClient,
		userAgent: "biga-cli",
		backoff:   backoff,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Compressing reports whether request bodies are being gzip-compressed.
func (c *Client) Compressing() bool { return c.gzip.Load() }

// EnsureSession creates the session if it does not exist, or reactivates it
// if it has ended. It is idempotent: re-running it for the same ID keeps the
// session's history and share URL.
//
// token is the session's owner token, empty for a session this client has
// not created yet. When the call creates the session, the returned Session
// carries the new token. A session owned by someone else, or created before
// owner tokens existed, fails with an error [IsOwnershipError] reports.
func (c *Client) EnsureSession(ctx context.Context, sessionID, token string, in CreateSessionInput) (*Session, error) {
	var grant bigav1.SessionGrant
	// Startup is interactive, so fail fast instead of walking the full backoff
	// schedule against an unreachable server.
	if _, err := c.do(ctx, http.MethodPut, sessionPath(sessionID, ""), putSessionToPB(in), &grant, request{attempts: 2, token: token}); err != nil {
		return nil, err
	}
	return sessionFromPB(&grant)
}

// EndSession marks the session as ended.
func (c *Client) EndSession(ctx context.Context, sessionID, token string) error {
	_, err := c.do(ctx, http.MethodDelete, sessionPath(sessionID, ""), nil, nil, request{token: token})
	return err
}

// Heartbeat reports that the broadcast is still live. It is not retried: the
// next heartbeat is the retry.
func (c *Client) Heartbeat(ctx context.Context, sessionID, token string) error {
	_, err := c.do(ctx, http.MethodPost, sessionPath(sessionID, "heartbeat"), nil, nil, request{attempts: 1, token: token})
	return err
}

// Manifest fetches the path and hash of every file the server holds for the
// session, so a rejoining CLI can upload only what changed.
func (c *Client) Manifest(ctx context.Context, sessionID, token string) (*Manifest, error) {
	var m bigav1.Manifest
	_, err := c.do(ctx, http.MethodGet, sessionPath(sessionID, "manifest"), nil, &m, request{attempts: 2, token: token})
	if HasStatus(err, http.StatusNotFound) {
		return nil, ErrManifestUnavailable
	}
	if err != nil {
		return nil, err
	}
	return manifestFromPB(&m), nil
}

// PushCheckpoint uploads one batch of file changes and returns the size of
// the request body as sent.
func (c *Client) PushCheckpoint(ctx context.Context, token string, u *CheckpointUpload) (int, error) {
	if err := u.Validate(); err != nil {
		return 0, fmt.Errorf("invalid checkpoint: %w", err)
	}
	return c.do(ctx, http.MethodPost, sessionPath(u.SessionID, "checkpoint"), uploadToPB(u), nil, request{compress: true, token: token})
}

// UploadSnapshot replaces the session's file tree and returns the size of
// the request body as sent. onProgress, if not nil, is called as the body is
// written with the bytes sent so far and the total.
func (c *Client) UploadSnapshot(ctx context.Context, token string, s *FileSnapshot, onProgress func(sent, total int)) (int, error) {
	if s.SessionID == "" || s.Tree == nil {
		return 0, errors.New("invalid snapshot: missing session id or tree")
	}
	return c.do(ctx, http.MethodPost, sessionPath(s.SessionID, "snapshot"), snapshotToPB(s), nil, request{
		timeout:  uploadTimeout,
		compress: true,
		progress: onProgress,
		token:    token,
	})
}

func sessionPath(sessionID, action string) string {
	p := "/api/sessions/" + url.PathEscape(sessionID)
	if action != "" {
		p += "/" + action
	}
	return p
}
