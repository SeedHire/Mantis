// Package setup handles first-run onboarding: GitHub login + Ollama API key.
// Global config is stored at ~/.mantis/credentials.json.
package setup

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"
)

// githubClientID is the GitHub OAuth App client ID — this is NOT a secret.
// OAuth device flow requires only the client ID (no client secret).
// It is safe to commit. Override at build time via ldflags if needed:
// -ldflags "-X github.com/seedhire/mantis/internal/setup.githubClientID=xxx"
var githubClientID = "Ov23liWbzDpXAJi3Fdzy"

// supabaseTrackUserURL is the edge function endpoint for sign-in tracking.
// Override at build time: -ldflags "-X github.com/seedhire/mantis/internal/setup.supabaseTrackUserURL=xxx"
var supabaseTrackUserURL = "https://vkimmiebehlgzlgrbwyo.supabase.co/functions/v1/track-user"

// supabaseAnonKey is the public anon key for the Supabase project.
// Injected at build time: -ldflags "-X github.com/seedhire/mantis/internal/setup.supabaseAnonKey=<anon-key>"
// Falls back to the SUPABASE_ANON_KEY environment variable.
var supabaseAnonKey = ""

// AppVersion is set at build time via ldflags to the release tag (e.g. "v0.1.0").
var AppVersion = "dev"

const (
	colorReset  = "\033[0m"
	colorCopper = "\033[38;5;214m"
	colorGold   = "\033[38;5;220m"
	colorDim    = "\033[38;5;244m"
	colorGreen  = "\033[38;5;43m"
	colorRed    = "\033[38;5;197m"
	colorBold   = "\033[1m"
)

// Credentials holds global user credentials persisted at ~/.mantis/credentials.json.
type Credentials struct {
	GitHubUser   string `json:"github_user"`
	GitHubToken  string `json:"github_token,omitempty"`
	OllamaAPIKey string `json:"ollama_api_key,omitempty"`
}

func credPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".mantis", "credentials.json")
}

// Load reads credentials from disk. Returns empty struct if not found.
func Load() (*Credentials, error) {
	data, err := os.ReadFile(credPath())
	if os.IsNotExist(err) {
		return &Credentials{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Save writes credentials to disk (permissions 0600).
func (c *Credentials) Save() error {
	path := credPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// NeedsSetup returns true if first-run setup has not been completed.
func NeedsSetup() bool {
	creds, err := Load()
	if err != nil {
		return true
	}
	return !IsLoggedIn(creds)
}

// IsLoggedIn returns true only when credentials contain a real GitHub username.
func IsLoggedIn(creds *Credentials) bool {
	if creds == nil {
		return false
	}
	u := creds.GitHubUser
	return u != "" && u != "guest" && u != "unknown"
}

// ApplyToEnv sets OLLAMA_API_KEY in the process environment from saved credentials
// (only if not already set by the user).
func ApplyToEnv(creds *Credentials) {
	if os.Getenv("OLLAMA_API_KEY") == "" && creds.OllamaAPIKey != "" {
		os.Setenv("OLLAMA_API_KEY", creds.OllamaAPIKey)
	}
}

// Run walks the user through first-run setup and saves credentials.
func Run() (*Credentials, error) {
	reader := bufio.NewReader(os.Stdin)

	fmt.Printf("\n%s%s  Welcome to Mantis%s\n", colorCopper, colorBold, colorReset)
	fmt.Printf("%s  Free AI coding assistant — one-time setup.%s\n\n", colorDim, colorReset)

	// ── Quick stats box ───────────────────────────────────────────────────────
	fmt.Printf("%s  ┌─────────────────────────────────────────────────┐%s\n", colorDim, colorReset)
	fmt.Printf("%s  │  %sWhat you get%s%s                                   │%s\n", colorDim, colorGold, colorDim, colorReset, colorReset)
	fmt.Printf("%s  │  %s✓%s  Unlimited AI — no monthly bill              │%s\n", colorDim, colorGreen, colorDim, colorReset)
	fmt.Printf("%s  │  %s✓%s  3 model tiers (fast / smart / heavy)        │%s\n", colorDim, colorGreen, colorDim, colorReset)
	fmt.Printf("%s  │  %s✓%s  Multimodal — paste screenshots              │%s\n", colorDim, colorGreen, colorDim, colorReset)
	fmt.Printf("%s  │  %s✓%s  Persistent project memory                   │%s\n", colorDim, colorGreen, colorDim, colorReset)
	fmt.Printf("%s  │  %s✓%s  Hallucination check on every response       │%s\n", colorDim, colorGreen, colorDim, colorReset)
	fmt.Printf("%s  │  %s✓%s  Token savings vs GPT-4o / Claude / Copilot  │%s\n", colorDim, colorGreen, colorDim, colorReset)
	fmt.Printf("%s  └─────────────────────────────────────────────────┘%s\n\n", colorDim, colorReset)

	creds := &Credentials{}

	// ── Step 1: GitHub login ─────────────────────────────────────────────────
	fmt.Printf("%s● Step 1/2 — GitHub login%s\n", colorGold, colorReset)
	fmt.Printf("%s  Mantis uses GitHub to identify you — no billing, no extra account.\n", colorDim)
	fmt.Printf("  Works instantly if you have GitHub CLI (gh) installed.\n")
	fmt.Printf("  Otherwise a browser tab opens — just click Authorize.%s\n\n", colorReset)

	user, token, err := githubLogin(reader)
	if err != nil {
		return nil, err
	}
	creds.GitHubUser = user
	creds.GitHubToken = token
	fmt.Printf("%s  ✓ Logged in as %s%s\n\n", colorGreen, user, colorReset)

	// Fire tracking in the background — never block the user.
	go func() {
		profile := resolveGitHubProfile(token)
		trackUserSignIn(profile)
	}()

	// ── Step 2: Ollama API key ───────────────────────────────────────────────
	fmt.Printf("%s● Step 2/2 — Ollama Cloud API key%s\n", colorGold, colorReset)
	fmt.Printf("%s  Ollama Cloud gives you free access to open-source AI models\n", colorDim)
	fmt.Printf("  (Llama, Qwen, Mistral and more) — no credit card needed.\n\n")
	fmt.Printf("  Get your free key:\n")
	fmt.Printf("    1. Go to  %shttps://ollama.com/cloud%s\n", colorCopper, colorDim)
	fmt.Printf("    2. Sign in with GitHub\n")
	fmt.Printf("    3. Copy your API key\n\n")
	fmt.Printf("  Or press Enter to use a local Ollama install instead\n")
	fmt.Printf("  (requires: ollama serve  running in another terminal)%s\n", colorReset)

	key := readSecret("  API key")
	if key != "" {
		creds.OllamaAPIKey = key
		fmt.Printf("%s  ✓ Ollama Cloud configured%s\n\n", colorGreen, colorReset)
	} else {
		fmt.Printf("%s  ✓ Using local Ollama — make sure `ollama serve` is running%s\n\n", colorDim, colorReset)
	}

	if err := creds.Save(); err != nil {
		return nil, fmt.Errorf("save credentials: %w", err)
	}

	printSetupComplete()
	return creds, nil
}

// printSetupComplete shows a summary of what Mantis can do now.
func printSetupComplete() {
	fmt.Printf("%s%s  ✓ All set!%s\n\n", colorGreen, colorBold, colorReset)
	fmt.Printf("%s  ┌───────────────────────────────────────────────────────────┐%s\n", colorGold, colorReset)
	fmt.Printf("%s  │  %sQuick start%s%s                                            │%s\n", colorGold, colorBold, colorGold, colorReset, colorReset)
	fmt.Printf("%s  │                                                           │%s\n", colorGold, colorReset)
	fmt.Printf("%s  │  %smantis%s             open AI session                     │%s\n", colorGold, colorCopper, colorGold, colorReset)
	fmt.Printf("%s  │  %smantis \"question\"%s  one-shot answer                    │%s\n", colorGold, colorCopper, colorGold, colorReset)
	fmt.Printf("%s  │  %smantis --model heavy%s  use the big model               │%s\n", colorGold, colorCopper, colorGold, colorReset)
	fmt.Printf("%s  │                                                           │%s\n", colorGold, colorReset)
	fmt.Printf("%s  │  Inside the session:  /help  /cost  /brain  /quit        │%s\n", colorGold, colorReset)
	fmt.Printf("%s  └───────────────────────────────────────────────────────────┘%s\n\n", colorGold, colorReset)
}

// githubLogin runs the GitHub Device OAuth flow — opens a browser tab,
// user clicks Authorize, token arrives automatically. No copy-paste needed.
// Falls back to `gh` CLI if already authenticated (zero extra steps).
func githubLogin(reader *bufio.Reader) (user, token string, err error) {
	// If gh CLI is already authenticated, just reuse that.
	if ghPath, e := exec.LookPath("gh"); e == nil {
		out, e := exec.Command(ghPath, "auth", "status", "--hostname", "github.com").CombinedOutput()
		if e == nil && strings.Contains(string(out), "Logged in") {
			u := ghUser(ghPath)
			t := ghToken(ghPath)
			if u != "" {
				return u, t, nil
			}
		}
	}

	// GitHub Device Flow.
	return deviceFlow(reader)
}

// deviceFlow implements https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps#device-flow
func deviceFlow(reader *bufio.Reader) (user, token string, err error) {
	// Step 1: request device + user code.
	resp, err := http.PostForm("https://github.com/login/device/code", url.Values{
		"client_id": {githubClientID},
		"scope":     {"read:user"},
	})
	if err != nil {
		printTokenFallback()
		return requiredTokenEntry(reader)
	}
	defer resp.Body.Close()

	var dc struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dc); err != nil || dc.UserCode == "" {
		printTokenFallback()
		return requiredTokenEntry(reader)
	}

	if dc.Interval == 0 {
		dc.Interval = 5
	}

	// Step 2: open browser and show the code.
	fmt.Printf("\n  %s%sYour activation code: %s%s\n", colorBold, colorGold, dc.UserCode, colorReset)
	fmt.Printf("  %sOpening: %s%s\n\n", colorDim, dc.VerificationURI, colorReset)
	openBrowser(dc.VerificationURI)
	fmt.Printf("  %sWaiting for you to authorize in the browser…%s\n", colorDim, colorReset)

	// Step 3: poll until authorized or expired.
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(time.Duration(dc.Interval) * time.Second)

		r, err := http.PostForm("https://github.com/login/oauth/access_token", url.Values{
			"client_id":   {githubClientID},
			"device_code": {dc.DeviceCode},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		})
		if err != nil {
			continue
		}

		var result struct {
			AccessToken string `json:"access_token"`
			Error       string `json:"error"`
		}
		json.NewDecoder(r.Body).Decode(&result)
		r.Body.Close()

		switch result.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			dc.Interval += 5
			continue
		case "expired_token", "access_denied":
			return "", "", fmt.Errorf("GitHub login %s", result.Error)
		}

		if result.AccessToken != "" {
			u := resolveGitHubUser(result.AccessToken)
			return u, result.AccessToken, nil
		}
	}
	return "", "", fmt.Errorf("GitHub login timed out")
}

// openBrowser opens url in the default system browser.
func openBrowser(u string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd, args = "open", []string{u}
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler", u}
	default:
		cmd, args = "xdg-open", []string{u}
	}
	_ = exec.Command(cmd, args...).Start()
}

// printTokenFallback shows instructions for getting a GitHub token manually.
// Called when browser OAuth is unavailable.
func printTokenFallback() {
	fmt.Printf("\n  %sBrowser login unavailable. Create a GitHub token instead.%s\n\n", colorDim, colorReset)
	fmt.Printf("%s  Steps:\n", colorGold)
	fmt.Printf("    1. Open  %shttps://github.com/settings/tokens/new%s\n", colorCopper, colorGold)
	fmt.Printf("    2. Note: \"Mantis AI\"\n")
	fmt.Printf("    3. Expiration: 90 days (or No expiration)\n")
	fmt.Printf("    4. Scopes — check only:\n")
	fmt.Printf("         %sread:user%s  — to show your username in Mantis%s\n", colorBold, colorGold, colorReset)
	fmt.Printf("%s    5. Click %sGenerate token%s%s — copy it (shown once only)%s\n\n", colorGold, colorBold, colorGold, colorDim, colorReset)
}

// requiredTokenEntry loops until the user provides a valid GitHub token.
// Used when OAuth device flow is unavailable — login is not optional.
func requiredTokenEntry(_ *bufio.Reader) (user, token string, err error) {
	for {
		tok := readSecret("  Paste token")
		if tok == "" {
			fmt.Printf("  %s✗ Token is required. Press Ctrl+C to exit.%s\n", colorRed, colorReset)
			continue
		}
		u := resolveGitHubUser(tok)
		if u == "unknown" {
			fmt.Printf("  %s✗ Token invalid or expired. Check the token and try again.%s\n", colorRed, colorReset)
			continue
		}
		return u, tok, nil
	}
}

// readSecret reads a line from stdin without echoing characters (password-style).
// Shows "••••••" confirmation so user knows input was received.
// Falls back to normal readline if stdin is not a TTY.
func readSecret(prompt string) string {
	fmt.Printf("%s: ", prompt)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		if err != nil || len(b) == 0 {
			fmt.Println()
			return ""
		}
		// Show placeholder dots so user knows input was captured.
		dots := strings.Repeat("•", min(len(b), 12))
		fmt.Printf("%s\n", dots)
		return strings.TrimSpace(string(b))
	}
	// Non-TTY fallback (pipes, scripts)
	var s string
	fmt.Scanln(&s)
	return strings.TrimSpace(s)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func ghUser(ghPath string) string {
	out, err := exec.Command(ghPath, "api", "user", "--jq", ".login").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func ghToken(ghPath string) string {
	out, err := exec.Command(ghPath, "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// githubProfile holds the GitHub user fields returned by /user.
type githubProfile struct {
	Login     string `json:"login"`
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

// resolveGitHubUser calls the GitHub API to get the username for a token.
func resolveGitHubUser(token string) string {
	return resolveGitHubProfile(token).Login
}

// resolveGitHubProfile fetches full profile info for a token.
func resolveGitHubProfile(token string) githubProfile {
	req, err := http.NewRequest("GET", "https://api.github.com/user", nil)
	if err != nil {
		return githubProfile{Login: "unknown"}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	r, err := http.DefaultClient.Do(req)
	if err != nil {
		return githubProfile{Login: "unknown"}
	}
	defer r.Body.Close()

	var p githubProfile
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.Login == "" {
		return githubProfile{Login: "unknown"}
	}
	return p
}

// trackUserSignIn fires a best-effort POST to the Supabase edge function.
// Failures are silently ignored — tracking must never block the user.
func trackUserSignIn(p githubProfile) {
	if p.ID == 0 || p.Login == "unknown" || supabaseTrackUserURL == "" {
		return
	}
	payload := map[string]any{
		"github_id":       p.ID,
		"github_username": p.Login,
		"github_name":     p.Name,
		"github_email":    p.Email,
		"github_avatar":   p.AvatarURL,
		"cli_version":     AppVersion,
	}
	body, _ := json.Marshal(payload)
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest("POST", supabaseTrackUserURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	key := supabaseAnonKey
	if key == "" {
		key = os.Getenv("SUPABASE_ANON_KEY")
	}
	if key != "" {
		req.Header.Set("apikey", key)
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}
