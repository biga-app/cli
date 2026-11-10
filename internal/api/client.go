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
	if _, err := c.do(ctx, bigav1.RoutePutSessionMethod, routePath(bigav1.RoutePutSessionPath, sessionID), putSessionToPB(in), &grant, request{attempts: 2, token: token}); err != nil {
		return nil, err
	}
	return sessionFromPB(&grant)
}

// EndSession marks the session as ended.
func (c *Client) EndSession(ctx context.Context, sessionID, token string) error {
	_, err := c.do(ctx, bigav1.RouteEndSessionMethod, routePath(bigav1.RouteEndSessionPath, sessionID), nil, nil, request{token: token})
	return err
}

// Heartbeat reports that the broadcast is still live. It is not retried: the
// next heartbeat is the retry.
func (c *Client) Heartbeat(ctx context.Context, sessionID, token string) error {
	_, err := c.do(ctx, bigav1.RouteHeartbeatMethod, routePath(bigav1.RouteHeartbeatPath, sessionID), nil, nil, request{attempts: 1, token: token})
	return err
}

// Manifest fetches the path and hash of every file the server holds for the
// session, so a rejoining CLI can upload only what changed.
func (c *Client) Manifest(ctx context.Context, sessionID, token string) (*Manifest, error) {
	var m bigav1.Manifest
	_, err := c.do(ctx, bigav1.RouteGetManifestMethod, routePath(bigav1.RouteGetManifestPath, sessionID), nil, &m, request{attempts: 2, token: token})
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
	return c.do(ctx, bigav1.RoutePostCheckpointMethod, routePath(bigav1.RoutePostCheckpointPath, u.SessionID), uploadToPB(u), nil, request{compress: true, token: token})
}

// UploadSnapshot replaces the session's file tree and returns the size of
// the request body as sent. onProgress, if not nil, is called as the body is
// written with the bytes sent so far and the total.
func (c *Client) UploadSnapshot(ctx context.Context, token string, s *FileSnapshot, onProgress func(sent, total int)) (int, error) {
	if s.SessionID == "" || s.Tree == nil {
		return 0, errors.New("invalid snapshot: missing session id or tree")
	}
	return c.do(ctx, bigav1.RoutePostSnapshotMethod, routePath(bigav1.RoutePostSnapshotPath, s.SessionID), snapshotToPB(s), nil, request{
		timeout:  uploadTimeout,
		compress: true,
		progress: onProgress,
		token:    token,
	})
}

// MissingBlobs asks which of the content hashes the server has not stored for
// the session, so only those need uploading.
func (c *Client) MissingBlobs(ctx context.Context, sessionID, token string, hashes []string) ([]string, error) {
	var res bigav1.MissingBlobsResponse
	_, err := c.do(ctx, bigav1.RouteMissingBlobsMethod, routePath(bigav1.RouteMissingBlobsPath, sessionID),
		&bigav1.MissingBlobsRequest{Hashes: hashes}, &res, request{attempts: 2, token: token})
	if err != nil {
		return nil, err
	}
	return res.GetHashes(), nil
}

// UploadBlobs stores file content ahead of the snapshot that will refer to it,
// and returns the size of the request body as sent.
func (c *Client) UploadBlobs(ctx context.Context, sessionID, token string, blobs []Blob) (int, error) {
	if len(blobs) == 0 {
		return 0, errors.New("invalid blob upload: no blobs")
	}
	return c.do(ctx, bigav1.RouteUploadBlobsMethod, routePath(bigav1.RouteUploadBlobsPath, sessionID), blobsToPB(blobs), nil, request{
		timeout: uploadTimeout, compress: true, token: token,
	})
}

// routePath fills the {placeholders} of a route's path template, in order,
// with values escaped for a URL path.
func routePath(template string, values ...string) string {
	var b strings.Builder
	for len(values) > 0 {
		start, end := strings.IndexByte(template, '{'), strings.IndexByte(template, '}')
		if start < 0 || end < start {
			break
		}
		b.WriteString(template[:start])
		b.WriteString(url.PathEscape(values[0]))
		template, values = template[end+1:], values[1:]
	}
	b.WriteString(template)
	return b.String()
}
