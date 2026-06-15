package drive

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"golang.org/x/oauth2"
)

// Client wraps the Google Drive API service and provides high-level operations
// for file and folder management within the gcrypt root folder.
type Client struct {
	svc      *drive.Service
	config   *oauth2.Config
	token    *oauth2.Token
	mu       sync.Mutex
	folderID string
}

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

// wrapAPIError wraps a Google Drive API error, returning a RateLimitError
// for 429 responses or a generic wrapped error otherwise.
func wrapAPIError(err error) error {
	if err == nil {
		return nil
	}
	if isRateLimitError(err) {
		return &RateLimitError{Err: err}
	}
	return fmt.Errorf("drive: %w", err)
}

// NewClient creates a new Google Drive API client using the provided OAuth2
// configuration, token, and root folder ID. It validates the credentials,
// builds an HTTP client from the token, and initializes the Drive service.
func NewClient(ctx context.Context, oauthCfg OAuthConfig, token *oauth2.Token, folderID string) (*Client, error) {
	// Log entry point for diagnostics
	fmt.Println("[DEBUG] NewClient: starting client creation")

	config, err := NewOAuthConfig(oauthCfg)
	if err != nil {
		return nil, fmt.Errorf("drive: failed to create OAuth config: %w", err)
	}
	fmt.Println("[DEBUG] NewClient: OAuth config created successfully")

	// This call may trigger token refresh if the token is expired
	fmt.Println("[DEBUG] NewClient: creating HTTP client (may trigger token refresh)")
	httpClient := config.Client(ctx, token)
	fmt.Println("[DEBUG] NewClient: HTTP client created successfully")

	// This call may perform API discovery
	fmt.Println("[DEBUG] NewClient: creating Drive service (may perform API discovery)")
	svc, err := drive.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("drive: failed to create Drive service: %w", err)
	}
	fmt.Println("[DEBUG] NewClient: Drive service created successfully")

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
	c.mu.Lock()
	defer c.mu.Unlock()

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
		MimeType: "application/vnd.google-apps.folder",
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
func (c *Client) UploadFile(ctx context.Context, name string, parentID string, content io.Reader) (*DriveFile, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	file := &drive.File{
		Name:    name,
		Parents: []string{parentID},
	}

	result, err := c.svc.Files.Create(file).
		Media(limitUploadReader(content)).
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
	c.mu.Lock()
	defer c.mu.Unlock()

	resp, err := c.svc.Files.Get(fileID).
		Context(ctx).
		Download()
	if err != nil {
		return nil, wrapAPIError(err)
	}

	return limitDownloadReadCloser(resp.Body), nil
}

// ListFiles lists files in the specified parent folder, supporting pagination.
// It returns a slice of DriveFile, the next page token (empty if no more
// pages), and any error.
func (c *Client) ListFiles(ctx context.Context, parentID string, pageToken string) ([]*DriveFile, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

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

// GetFile retrieves metadata for a single file by its ID.
func (c *Client) GetFile(ctx context.Context, fileID string) (*DriveFile, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

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
func (c *Client) DeleteFile(ctx context.Context, fileID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	err := c.svc.Files.Delete(fileID).
		Context(ctx).
		Do()
	if err != nil {
		return wrapAPIError(err)
	}

	return nil
}

// UpdateFile replaces the content of an existing file on Google Drive.
func (c *Client) UpdateFile(ctx context.Context, fileID string, content io.Reader) (*DriveFile, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	result, err := c.svc.Files.Update(fileID, &drive.File{}).
		Media(limitUploadReader(content)).
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
	c.mu.Lock()
	defer c.mu.Unlock()

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
