package auth

import "time"

const TargetGeminiAntigravity = "gemini:antigravity"

type RawCredential struct {
	Target  string
	User    string
	Type    uint32
	Persist uint32
	Blob    []byte
}

type StoredCred struct {
	Name        string    `json:"name"`
	Email       string    `json:"email"`
	RefreshFP   string    `json:"refresh_fp,omitempty"`
	Target      string    `json:"target"`
	User        string    `json:"user"`
	Type        uint32    `json:"type"`
	Persist     uint32    `json:"persist"`
	RawJSON     string    `json:"raw_json"`
	SavedAt     time.Time `json:"saved_at"`
	TokenExpiry string    `json:"token_expiry,omitempty"`
}

type AccountInfo struct {
	Name        string `json:"name"`
	Email       string `json:"email"`
	RefreshFP   string `json:"refresh_fp,omitempty"`
	Active      bool   `json:"active"`
	TokenExpiry string `json:"token_expiry,omitempty"`
	SavedAt     string `json:"saved_at,omitempty"`
}

type ReloadOptions struct {
	SkipPaneID  string
	ForceBusy   bool
	ExitTimeout time.Duration
}

type ReloadResult struct {
	Reloaded    []string
	SkippedBusy []string
	Failed      []string
}

type oauthPayload struct {
	Token struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		RefreshToken string `json:"refresh_token"`
		Expiry       string `json:"expiry"`
		ExpiresIn    int    `json:"expires_in"`
	} `json:"token"`
	AuthMethod   string `json:"auth_method"`
	IDTokenEmail string `json:"id_token_email"`
}
