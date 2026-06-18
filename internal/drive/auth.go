// Package drive implements the Google Drive API client, OAuth2 authentication,
// and local metadata storage for the gcrypt sync engine.
package drive

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/arumes31/gcrypt/internal/crypto"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// TokenFile is the filename used for the encrypted OAuth2 token on disk.
const TokenFile = "token.json"

// OAuthConfig holds the OAuth2 client credentials needed to authenticate
// with the Google Drive API.
type OAuthConfig struct {
	ClientID     string
	ClientSecret string
}

// NewOAuthConfig creates an *oauth2.Config for the Google Drive API using the
// provided client credentials. The config is set up for the drive.file scope
// with a loopback redirect on localhost:8089 and offline access so that
// refresh tokens are returned.
func NewOAuthConfig(oauthCfg OAuthConfig) (*oauth2.Config, error) {
	if oauthCfg.ClientID == "" {
		return nil, fmt.Errorf("drive: OAuth client ID is required")
	}
	if oauthCfg.ClientSecret == "" {
		return nil, fmt.Errorf("drive: OAuth client secret is required")
	}

	return &oauth2.Config{
		ClientID:     oauthCfg.ClientID,
		ClientSecret: oauthCfg.ClientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       []string{"https://www.googleapis.com/auth/drive.file"},
		RedirectURL:  "http://localhost:8089/callback",
	}, nil
}

// GetTokenFromWebBrowser performs the OAuth2 authorization code flow designed
// for GUI/tray usage where there is no terminal. It:
//   - Generates an auth URL with a random state parameter for CSRF protection
//   - Automatically opens the browser to the auth URL
//   - Starts a local HTTP server on port 8089 to receive the callback
//   - Verifies the state parameter and exchanges the code for a token
//   - Uses a 5-minute timeout and returns an error if the user does not finish
func GetTokenFromWebBrowser(config *oauth2.Config) (*oauth2.Token, error) {
	// Generate random state for CSRF protection.
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return nil, fmt.Errorf("drive: failed to generate state: %w", err)
	}
	state := hex.EncodeToString(stateBytes)

	authURL := config.AuthCodeURL(state, oauth2.AccessTypeOffline)

	// Open the browser automatically.
	openBrowser(authURL)

	// Listen on port 8089 and wait for the callback.
	var lc net.ListenConfig
	listener, err := lc.Listen(context.Background(), "tcp", "localhost:8089")
	if err != nil {
		return nil, fmt.Errorf("drive: failed to listen on localhost:8089: %w", err)
	}

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	// ReadHeaderTimeout is set in the struct literal (not assigned afterwards) so
	// static analysers can see this server is not vulnerable to Slowloris.
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second}
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}

		receivedState := r.URL.Query().Get("state")
		if receivedState != state {
			errCh <- fmt.Errorf("drive: state mismatch: expected %s, got %s", state, receivedState)
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}

		code := r.URL.Query().Get("code")
		if code == "" {
			errCh <- fmt.Errorf("drive: authorization code not returned")
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}

		_, _ = fmt.Fprintln(w, "Authorization successful! You can close this tab.")
		codeCh <- code
	})

	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("drive: callback server error: %w", err)
		}
	}()

	// Wait for the code or an error with a 5-minute timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var code string
	select {
	case code = <-codeCh:
		// Success — continue to exchange.
	case err := <-errCh:
		_ = srv.Close()
		return nil, err
	case <-ctx.Done():
		_ = srv.Shutdown(context.Background())
		return nil, fmt.Errorf("drive: OAuth flow timed out after 5 minutes")
	}

	// Shutdown the temporary server.
	_ = srv.Shutdown(context.Background())

	// Exchange the authorization code for a token.
	token, err := config.Exchange(context.Background(), code)
	if err != nil {
		return nil, fmt.Errorf("drive: failed to exchange token: %w", err)
	}

	return token, nil
}

// openBrowser opens the given URL in the user's default browser. The
// implementation varies by OS; on Windows it uses rundll32, on Darwin it uses
// the open command, and on Linux/other it uses xdg-open.
func openBrowser(url string) {
	// The command names are fixed constants and the only variable argument is the
	// OAuth authorization URL this package just built — not attacker input — so
	// the G204 "subprocess from variable" warnings are audited and suppressed.
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.CommandContext(context.Background(), "rundll32", "url.dll,FileProtocolHandler", url) // #nosec G204 -- fixed command; url is our own OAuth URL
	case "darwin":
		cmd = exec.CommandContext(context.Background(), "open", url) // #nosec G204 -- fixed command; url is our own OAuth URL
	default:
		cmd = exec.CommandContext(context.Background(), "xdg-open", url) // #nosec G204 -- fixed command; url is our own OAuth URL
	}
	// Best-effort: ignore errors if the browser cannot be opened.
	_ = cmd.Run()
}

// SaveToken encrypts the OAuth2 token with the master key and writes it to
// the file at path. Parent directories are created if needed. The token is
// JSON-marshalled and then encrypted using crypto.EncryptBytes with the path
// "gcrypt://oauth-token" as AAD.
func SaveToken(path string, token *oauth2.Token, masterKey []byte) error {
	// Marshalling the token deliberately includes the access/refresh tokens — the
	// JSON is immediately encrypted with the master key below and never written in
	// the clear, so the G117 "secret in marshalled struct" warning is by design.
	data, err := json.Marshal(token) // #nosec G117 -- token JSON is encrypted before being persisted
	if err != nil {
		return fmt.Errorf("drive: failed to marshal token: %w", err)
	}

	encrypted, err := crypto.EncryptBytes(data, masterKey, "gcrypt://oauth-token")
	if err != nil {
		return fmt.Errorf("drive: failed to encrypt token: %w", err)
	}

	// Create parent directories if needed.
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("drive: failed to create token directory: %w", err)
	}

	if err := os.WriteFile(path, encrypted, 0600); err != nil {
		return fmt.Errorf("drive: failed to write token file: %w", err)
	}

	return nil
}

// LoadToken reads the encrypted token file at path, decrypts it with the
// master key using crypto.DecryptBytes with the path "gcrypt://oauth-token"
// as AAD, and unmarshals the JSON into an *oauth2.Token.
func LoadToken(path string, masterKey []byte) (*oauth2.Token, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is the app's own token file location, not user input
	if err != nil {
		return nil, fmt.Errorf("drive: failed to read token file: %w", err)
	}

	plaintext, err := crypto.DecryptBytes(data, masterKey, "gcrypt://oauth-token")
	if err != nil {
		return nil, fmt.Errorf("drive: failed to decrypt token: %w", err)
	}

	var token oauth2.Token
	if err := json.Unmarshal(plaintext, &token); err != nil {
		return nil, fmt.Errorf("drive: failed to unmarshal token: %w", err)
	}

	return &token, nil
}

// EncryptClientSecret encrypts the OAuth client secret with the master key and
// returns it as a base64 string suitable for storing in the config file. It
// uses crypto.EncryptBytes with "gcrypt://oauth-client-secret" as AAD, mirroring
// how the OAuth token is protected at rest.
func EncryptClientSecret(clientSecret string, masterKey []byte) (string, error) {
	encrypted, err := crypto.EncryptBytes([]byte(clientSecret), masterKey, "gcrypt://oauth-client-secret")
	if err != nil {
		return "", fmt.Errorf("drive: failed to encrypt client secret: %w", err)
	}
	return base64.StdEncoding.EncodeToString(encrypted), nil
}

// DecryptClientSecret reverses EncryptClientSecret: it base64-decodes the stored
// value and decrypts it with the master key.
func DecryptClientSecret(encoded string, masterKey []byte) (string, error) {
	blob, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("drive: failed to base64-decode client secret: %w", err)
	}
	plaintext, err := crypto.DecryptBytes(blob, masterKey, "gcrypt://oauth-client-secret")
	if err != nil {
		return "", fmt.Errorf("drive: failed to decrypt client secret: %w", err)
	}
	return string(plaintext), nil
}

// TokenPath returns the default path for the encrypted OAuth2 token file,
// located at %APPDATA%/gcrypt/token.json on Windows.
func TokenPath() string {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		appData = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Roaming")
	}
	return filepath.Join(appData, "gcrypt", TokenFile)
}
