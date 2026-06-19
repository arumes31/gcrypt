package drive

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"golang.org/x/oauth2"
)

// uploadChunkSize is the chunk size used for media uploads. Files at least this
// large are uploaded resumably: the Google API client buffers one chunk at a
// time and retries each chunk internally with backoff on transient (5xx/429)
// failures. Smaller files are sent as a single buffered request that the client
// can likewise rewind and retry. Either way, transient network errors no longer
// abort the whole operation even though our media source is a non-seekable pipe.
// The trade-off is up to this many bytes buffered in memory per concurrent
// upload.
const uploadChunkSize = 8 * 1024 * 1024 // 8 MiB

// driveAPIQueriesPerSec caps the Drive API request rate across the whole
// process. Google's per-user quota is ~12,000 queries/minute (~200/s); we stay
// well under it because a single sync operation issues several queries (a dedup
// search, creating the encrypted parent-folder chain, then the upload itself)
// and failed requests are retried. Crucially the cap is enforced at the HTTP
// transport, so EVERY request counts against it — across all sync pairs, plus
// resumable-upload chunks and token refreshes — which is exactly what Google's
// quota measures. A previous per-operation limiter under-counted (one token per
// op, but several queries per op), so a large backlog blew the per-minute quota.
const driveAPIQueriesPerSec = 100

// apiLimiter is process-global because Google's quota is per user, not per
// client or per sync pair.
var apiLimiter = newAPIRateLimiter(driveAPIQueriesPerSec)

// apiRateLimiter is a simple ticker-based token source shared by every Drive
// HTTP request.
type apiRateLimiter struct{ ticker *time.Ticker }

func newAPIRateLimiter(qps int) *apiRateLimiter {
	if qps < 1 {
		qps = 1
	}
	return &apiRateLimiter{ticker: time.NewTicker(time.Second / time.Duration(qps))}
}

// wait blocks until the next token is available or ctx is cancelled.
func (l *apiRateLimiter) wait(ctx context.Context) error {
	select {
	case <-l.ticker.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// rateLimitedRoundTripper gates every outgoing HTTP request through apiLimiter
// so the Drive API query rate stays within Google's per-user quota regardless of
// how many queries each high-level operation performs.
type rateLimitedRoundTripper struct {
	base    http.RoundTripper
	limiter *apiRateLimiter
}

func (rt *rateLimitedRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := rt.limiter.wait(req.Context()); err != nil {
		return nil, err
	}
	return rt.base.RoundTrip(req)
}

// Client wraps the Google Drive API service and provides high-level operations
// for file and folder management within the gcrypt root folder. The underlying
// *drive.Service (an http.Client) is safe for concurrent use, and every field is
// set once at construction and only read afterwards, so Client methods may be
// called concurrently — that is what lets the sync engine upload files in
// parallel.
type Client struct {
	svc      *drive.Service
	config   *oauth2.Config
	token    *oauth2.Token
	folderID string
}

// folderMimeType is the Google Drive MIME type used for folders.
const folderMimeType = "application/vnd.google-apps.folder"

// DriveFile holds the subset of Google Drive file metadata that gcrypt cares about.
type DriveFile struct {
	ID       string
	Name     string
	MimeType string
	Size     int64
	ModTime  time.Time
	MD5Hash  string
	Parents  []string
}

// IsFolder reports whether this Drive object is a folder.
func (f *DriveFile) IsFolder() bool {
	return f.MimeType == folderMimeType
}

// RateLimitError is returned when the Google Drive API returns a 429 status.
// Callers can check for this error type to implement retry logic.
type RateLimitError struct {
	Err error
}

func (e *RateLimitError) Error() string { return fmt.Sprintf("drive: rate limit exceeded: %v", e.Err) }
func (e *RateLimitError) Unwrap() error { return e.Err }

// isRateLimitError checks whether a Google API error is a 429 rate limit.
func isRateLimitError(err error) bool {
	if gerr, ok := err.(*googleapi.Error); ok && gerr.Code == 429 {
		return true
	}
	return false
}

// QuotaExceededError is returned when Google Drive rejects an upload because the
// account's storage quota is full. Callers can detect it to surface a clear
// "storage full" message and to avoid pointless retries (it won't self-resolve).
type QuotaExceededError struct {
	Err error
}

func (e *QuotaExceededError) Error() string {
	return "drive: Google Drive storage is full — free up space or upgrade your plan"
}
func (e *QuotaExceededError) Unwrap() error { return e.Err }

// isQuotaExceeded reports whether a Google API error is a storage-quota-full
// rejection (HTTP 403 with reason "storageQuotaExceeded").
func isQuotaExceeded(err error) bool {
	if gerr, ok := err.(*googleapi.Error); ok && gerr.Code == 403 {
		for _, e := range gerr.Errors {
			if e.Reason == "storageQuotaExceeded" {
				return true
			}
		}
	}
	return false
}

// wrapAPIError wraps a Google Drive API error, returning a typed error for
// recognised conditions (429 rate limit, storage-quota-full) or a generic
// wrapped error otherwise.
func wrapAPIError(err error) error {
	if err == nil {
		return nil
	}
	if isRateLimitError(err) {
		return &RateLimitError{Err: err}
	}
	if isQuotaExceeded(err) {
		return &QuotaExceededError{Err: err}
	}
	return fmt.Errorf("drive: %w", err)
}

// NewClient creates a new Google Drive API client using the provided OAuth2
// configuration, token, and root folder ID. It validates the credentials,
// builds an HTTP client from the token, and initializes the Drive service.
func NewClient(ctx context.Context, oauthCfg OAuthConfig, token *oauth2.Token, folderID string) (*Client, error) {
	config, err := NewOAuthConfig(oauthCfg)
	if err != nil {
		return nil, fmt.Errorf("drive: failed to create OAuth config: %w", err)
	}

	// Build the OAuth HTTP client on top of a transport with finite timeouts so a
	// stalled Drive request can never hang forever (uploads run concurrently, so a
	// wedged connection must not be able to tie up a worker indefinitely). These
	// are transport-level timeouts that abort dead connections without capping the
	// total duration of a legitimately long, still-progressing transfer.
	base := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}

	// IMPORTANT: the oauth2 HTTP client retains this context for the entire
	// lifetime of the client and uses it for every background token refresh. It
	// must therefore be long-lived — passing the caller's short-lived ctx (e.g. a
	// 30s timeout used for the initial connectivity check) would make every token
	// refresh fail with "context canceled" once the access token expires (~1h),
	// silently breaking all Drive operations and stalling the sync. Use
	// context.Background() for refreshes; per-request deadlines are applied via
	// each API call's own Context() instead.
	// Gate every outgoing request through the process-global API rate limiter so
	// the Drive query rate (across all pairs, upload chunks and token refreshes)
	// stays within Google's per-user quota.
	limited := &rateLimitedRoundTripper{base: base, limiter: apiLimiter}
	refreshCtx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Transport: limited})
	httpClient := config.Client(refreshCtx, token)

	// NewService may perform API discovery — bound that to the caller's ctx.
	svc, err := drive.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("drive: failed to create Drive service: %w", err)
	}

	return &Client{
		svc:      svc,
		config:   config,
		token:    token,
		folderID: folderID,
	}, nil
}

// EnsureFolder checks whether a folder with the given name exists under the
// client's root folder. If found, it returns the existing folder's ID. If not,
// it creates the folder and returns the new ID.
func (c *Client) EnsureFolder(ctx context.Context, name string) (string, error) {
	parentID := c.folderID

	// Search for an existing folder with this name in the parent.
	query := fmt.Sprintf(
		"name='%s' and '%s' in parents and mimeType='application/vnd.google-apps.folder' and trashed=false",
		name, parentID,
	)

	list, err := c.svc.Files.List().
		Q(query).
		Spaces("drive").
		Fields("files(id)").
		Context(ctx).
		Do()
	if err != nil {
		return "", wrapAPIError(err)
	}

	if len(list.Files) > 0 {
		return list.Files[0].Id, nil
	}

	// Folder not found — create it.
	folder := &drive.File{
		Name:     name,
		MimeType: folderMimeType,
		Parents:  []string{parentID},
	}

	created, err := c.svc.Files.Create(folder).
		Fields("id").
		Context(ctx).
		Do()
	if err != nil {
		return "", wrapAPIError(err)
	}

	return created.Id, nil
}

// EnsureFolderUnder returns the ID of the folder named name directly under
// parentID, creating it if it does not already exist. Unlike EnsureFolder,
// which always operates under the client's configured root, this works under an
// arbitrary parent and is used to build the encrypted folder chain for the
// hierarchical layout.
func (c *Client) EnsureFolderUnder(ctx context.Context, parentID, name string) (string, error) {
	query := fmt.Sprintf(
		"name='%s' and '%s' in parents and mimeType='%s' and trashed=false",
		name, parentID, folderMimeType,
	)

	list, err := c.svc.Files.List().
		Q(query).
		Spaces("drive").
		Fields("files(id)").
		Context(ctx).
		Do()
	if err != nil {
		return "", wrapAPIError(err)
	}
	if len(list.Files) > 0 {
		return list.Files[0].Id, nil
	}

	folder := &drive.File{
		Name:     name,
		MimeType: folderMimeType,
		Parents:  []string{parentID},
	}
	created, err := c.svc.Files.Create(folder).
		Fields("id").
		Context(ctx).
		Do()
	if err != nil {
		return "", wrapAPIError(err)
	}

	return created.Id, nil
}

// UploadFile uploads content as a new file with the given name under the
// specified parent folder on Google Drive.
func (c *Client) UploadFile(ctx context.Context, name string, parentID string, content io.Reader, modTime time.Time) (*DriveFile, error) {
	file := &drive.File{
		Name:    name,
		Parents: []string{parentID},
	}
	// Preserve the source file's modification time on Drive so it round-trips back
	// to the local copy on download (rather than every synced file showing its
	// upload time). A zero modTime leaves Drive to default it to "now".
	if !modTime.IsZero() {
		file.ModifiedTime = modTime.UTC().Format(time.RFC3339Nano)
	}

	result, err := c.svc.Files.Create(file).
		Media(limitUploadReader(content), googleapi.ChunkSize(uploadChunkSize)).
		Fields("id,name,mimeType,size,modifiedTime,md5Checksum,parents").
		Context(ctx).
		Do()
	if err != nil {
		return nil, wrapAPIError(err)
	}

	return toDriveFile(result), nil
}

// DownloadFile downloads the content of the file with the given ID and returns
// an io.ReadCloser. The caller is responsible for closing the reader when done.
func (c *Client) DownloadFile(ctx context.Context, fileID string) (io.ReadCloser, error) {
	resp, err := c.svc.Files.Get(fileID).
		Context(ctx).
		Download() //nolint:bodyclose // resp.Body is wrapped and returned to the caller, which closes it
	if err != nil {
		return nil, wrapAPIError(err)
	}

	return limitDownloadReadCloser(resp.Body), nil
}

// ListFiles lists files in the specified parent folder, supporting pagination.
// It returns a slice of DriveFile, the next page token (empty if no more
// pages), and any error.
func (c *Client) ListFiles(ctx context.Context, parentID string, pageToken string) ([]*DriveFile, string, error) {
	query := fmt.Sprintf("'%s' in parents and trashed=false", parentID)

	call := c.svc.Files.List().
		Q(query).
		Spaces("drive").
		Fields("nextPageToken,files(id,name,mimeType,size,modifiedTime,md5Checksum,parents)").
		Context(ctx)

	if pageToken != "" {
		call = call.PageToken(pageToken)
	}

	list, err := call.Do()
	if err != nil {
		return nil, "", wrapAPIError(err)
	}

	files := make([]*DriveFile, 0, len(list.Files))
	for _, f := range list.Files {
		files = append(files, toDriveFile(f))
	}

	return files, list.NextPageToken, nil
}

// AccountInfo holds the signed-in user's identity and Drive storage quota,
// as reported by the Drive "about" resource.
type AccountInfo struct {
	Email       string
	DisplayName string
	QuotaUsed   int64 // bytes used across Drive
	QuotaLimit  int64 // total bytes available; 0 means unlimited / not reported
}

// About fetches the signed-in user's email and Drive storage quota. It is a
// single lightweight metadata call, suitable for periodic refresh in the UI.
func (c *Client) About(ctx context.Context) (*AccountInfo, error) {
	about, err := c.svc.About.Get().
		Fields("user(displayName,emailAddress),storageQuota(limit,usage)").
		Context(ctx).
		Do()
	if err != nil {
		return nil, wrapAPIError(err)
	}

	info := &AccountInfo{}
	if about.User != nil {
		info.Email = about.User.EmailAddress
		info.DisplayName = about.User.DisplayName
	}
	if about.StorageQuota != nil {
		info.QuotaUsed = about.StorageQuota.Usage
		info.QuotaLimit = about.StorageQuota.Limit
	}
	return info, nil
}

// GetFile retrieves metadata for a single file by its ID.
func (c *Client) GetFile(ctx context.Context, fileID string) (*DriveFile, error) {
	result, err := c.svc.Files.Get(fileID).
		Fields("id,name,mimeType,size,modifiedTime,md5Checksum,parents").
		Context(ctx).
		Do()
	if err != nil {
		return nil, wrapAPIError(err)
	}

	return toDriveFile(result), nil
}

// DeleteFile permanently deletes the file with the given ID from Google Drive.
// It bypasses the trash, so the file is unrecoverable. Used for housekeeping of
// gcrypt's own empty encrypted folders; user file deletions go through TrashFile.
func (c *Client) DeleteFile(ctx context.Context, fileID string) error {
	err := c.svc.Files.Delete(fileID).
		Context(ctx).
		Do()
	if err != nil {
		return wrapAPIError(err)
	}

	return nil
}

// TrashFile moves the file with the given ID to Google Drive's trash instead of
// deleting it permanently. This makes a propagated deletion recoverable: the
// user (or another machine) can restore the file from Drive trash within the
// retention window. Used for user file deletions.
func (c *Client) TrashFile(ctx context.Context, fileID string) error {
	_, err := c.svc.Files.Update(fileID, &drive.File{Trashed: true}).
		Context(ctx).
		Do()
	if err != nil {
		return wrapAPIError(err)
	}

	return nil
}

// ChangeItem is a single entry from the Drive changes feed, reduced to what the
// sync engine needs to decide whether a change is relevant to a sync pair.
type ChangeItem struct {
	FileID  string   // the changed file's ID
	Removed bool     // the file was removed or is no longer accessible
	Trashed bool     // the file was moved to trash
	Parents []string // current parent folder IDs (empty for removed files)
}

// GetStartPageToken returns an opaque token marking "now" in the account-wide
// changes feed. A later ListChanges(token) returns everything that changed
// since. Persist/hold the token between polls to drive incremental syncing.
func (c *Client) GetStartPageToken(ctx context.Context) (string, error) {
	res, err := c.svc.Changes.GetStartPageToken().Context(ctx).Do()
	if err != nil {
		return "", wrapAPIError(err)
	}
	return res.StartPageToken, nil
}

// ListChanges fetches one page of changes since pageToken. It returns the page's
// changes plus two tokens: nextPageToken is non-empty when more pages remain
// (call again with it), and newStartPageToken is non-empty on the final page —
// hold onto it for the next poll cycle. The changes feed is account-wide, so
// callers must filter to the files/folders they care about.
func (c *Client) ListChanges(ctx context.Context, pageToken string) (items []ChangeItem, nextPageToken, newStartPageToken string, err error) {
	res, err := c.svc.Changes.List(pageToken).
		Spaces("drive").
		IncludeRemoved(true).
		Fields("nextPageToken,newStartPageToken,changes(fileId,removed,file(id,trashed,parents))").
		Context(ctx).
		Do()
	if err != nil {
		return nil, "", "", wrapAPIError(err)
	}

	items = make([]ChangeItem, 0, len(res.Changes))
	for _, ch := range res.Changes {
		item := ChangeItem{FileID: ch.FileId, Removed: ch.Removed}
		if ch.File != nil {
			item.Trashed = ch.File.Trashed
			item.Parents = ch.File.Parents
		}
		items = append(items, item)
	}
	return items, res.NextPageToken, res.NewStartPageToken, nil
}

// UpdateFile replaces the content of an existing file on Google Drive. modTime,
// when non-zero, sets the file's modification time so it round-trips on download.
func (c *Client) UpdateFile(ctx context.Context, fileID string, content io.Reader, modTime time.Time) (*DriveFile, error) {
	meta := &drive.File{}
	if !modTime.IsZero() {
		meta.ModifiedTime = modTime.UTC().Format(time.RFC3339Nano)
	}

	result, err := c.svc.Files.Update(fileID, meta).
		Media(limitUploadReader(content), googleapi.ChunkSize(uploadChunkSize)).
		Fields("id,name,mimeType,size,modifiedTime,md5Checksum,parents").
		Context(ctx).
		Do()
	if err != nil {
		return nil, wrapAPIError(err)
	}

	return toDriveFile(result), nil
}

// SearchByName searches for files with the given name in the specified parent
// folder. It returns all matching files (not trashed).
func (c *Client) SearchByName(ctx context.Context, name string, parentID string) ([]*DriveFile, error) {
	query := fmt.Sprintf("name='%s' and '%s' in parents and trashed=false", name, parentID)

	list, err := c.svc.Files.List().
		Q(query).
		Spaces("drive").
		Fields("files(id,name,mimeType,size,modifiedTime,md5Checksum,parents)").
		Context(ctx).
		Do()
	if err != nil {
		return nil, wrapAPIError(err)
	}

	files := make([]*DriveFile, 0, len(list.Files))
	for _, f := range list.Files {
		files = append(files, toDriveFile(f))
	}

	return files, nil
}

// toDriveFile converts a *drive.File from the Google Drive API into a DriveFile.
func toDriveFile(f *drive.File) *DriveFile {
	df := &DriveFile{
		ID:       f.Id,
		Name:     f.Name,
		MimeType: f.MimeType,
		Size:     f.Size,
		MD5Hash:  f.Md5Checksum,
		Parents:  f.Parents,
	}

	if t, err := time.Parse(time.RFC3339, f.ModifiedTime); err == nil {
		df.ModTime = t
	}

	return df
}
