package credentials

import (
	"encoding/json"
	"fmt"
	"os"
)

type Credential struct {
	User       string `json:"user"`
	TokenName  string `json:"token_name"`
	TokenValue string `json:"token_value"`
	ConsoleUser string `json:"console_user,omitempty"`
	ConsolePassword string `json:"console_password,omitempty"`
}
type BundleTarget struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Type       string     `json:"type"`
	Enabled    bool       `json:"enabled"`
	VerifyTLS  bool       `json:"verify_tls"`
	DetectHA   *bool      `json:"detect_ha,omitempty"`
	DetectCeph *bool      `json:"detect_ceph,omitempty"`
	Endpoints  []string   `json:"endpoints"`
	Credential Credential `json:"credential"`
}
type Bundle struct {
	Version     int            `json:"version"`
	GeneratedAt string         `json:"generated_at"`
	Targets     []BundleTarget `json:"targets"`
}

func LoadBundle(path string) (Bundle, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Bundle{}, fmt.Errorf("read credential bundle: %w", err)
	}
	var bundle Bundle
	if err := json.Unmarshal(b, &bundle); err != nil {
		return Bundle{}, fmt.Errorf("parse credential bundle: %w", err)
	}
	if bundle.Version != 1 {
		return Bundle{}, fmt.Errorf("unsupported credential bundle version %d", bundle.Version)
	}
	return bundle, nil
}

func Load(path string) (map[string]Credential, error) {
	v, err := LoadBundle(path)
	if err != nil {
		return nil, err
	}
	out := map[string]Credential{}
	for _, t := range v.Targets {
		out[t.ID] = t.Credential
	}
	return out, nil
}

func SaveBundle(path string, b Bundle) error {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
