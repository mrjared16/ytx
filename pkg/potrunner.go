package ytx

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	_ "embed"
)

//go:embed po_minter.mjs
var poMinterScript string

type poMintRequest struct {
	VideoID     string          `json:"videoId"`
	VisitorData string          `json:"visitorData,omitempty"`
	Challenge   json.RawMessage `json:"challenge,omitempty"`
}

type poMintResponse struct {
	PlayerToken string `json:"playerToken"`
	URLToken    string `json:"urlToken"`
	TTLSeconds  int    `json:"ttlSeconds"`
	Error       string `json:"error,omitempty"`
}

func mintPOTokenWithBun(ctx context.Context, challenge *POTokenChallenge) (string, string, time.Duration, error) {
	bunPath, err := exec.LookPath("bun")
	if err != nil {
		return "", "", 0, fmt.Errorf("bun not found in PATH")
	}

	tmpPath := filepath.Join(os.TempDir(), "ytx-po-minter.mjs")
	if cacheHome, err := os.UserCacheDir(); err == nil && cacheHome != "" {
		cacheDir := filepath.Join(cacheHome, "ytx")
		if mkErr := os.MkdirAll(cacheDir, 0o755); mkErr == nil {
			tmpPath = filepath.Join(cacheDir, "po-minter.mjs")
		}
	}
	if err := ensurePOMinterScript(tmpPath); err != nil {
		return "", "", 0, fmt.Errorf("failed to write po minter script: %w", err)
	}

	cmd := exec.CommandContext(ctx, bunPath, tmpPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", "", 0, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", "", 0, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return "", "", 0, err
	}

	if err := json.NewEncoder(stdin).Encode(poMintRequest{VideoID: challenge.VideoID, VisitorData: challenge.VisitorData, Challenge: challenge.BGChallenge}); err != nil {
		_ = stdin.Close()
		_ = cmd.Wait()
		return "", "", 0, err
	}
	_ = stdin.Close()

	var resp poMintResponse
	if err := json.NewDecoder(stdout).Decode(&resp); err != nil {
		_ = cmd.Wait()
		return "", "", 0, err
	}
	if err := cmd.Wait(); err != nil {
		return "", "", 0, err
	}
	if resp.Error != "" {
		return "", "", 0, fmt.Errorf("%s", resp.Error)
	}
	return resp.PlayerToken, resp.URLToken, time.Duration(resp.TTLSeconds) * time.Second, nil
}

func ensurePOMinterScript(path string) error {
	existing, err := os.ReadFile(path)
	if err == nil && string(existing) == poMinterScript {
		return nil
	}
	return os.WriteFile(path, []byte(poMinterScript), 0644)
}
