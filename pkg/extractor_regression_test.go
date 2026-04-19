package ytx

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestExtractorRegression is the main regression test
func TestExtractorRegression(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping regression test in short mode")
	}

	tmpDir := t.TempDir()

	results := make([]TestResult, 0)

	for _, videoID := range testVideoIDs {
		videoCacheDir := filepath.Join(tmpDir, "cache_video_"+videoID)
		if err := os.MkdirAll(videoCacheDir, 0755); err != nil {
			t.Fatalf("create video cache dir: %v", err)
		}
		videoRuntime := NewRuntime()
		defer videoRuntime.CloseCachedEngine()

		for _, cacheState := range []string{"cold", "warm"} {
			cacheState := cacheState
			t.Run(fmt.Sprintf("Video_%s_%s", cacheState, videoID), func(t *testing.T) {
				result := testVideoMode(t, videoCacheDir, videoID, videoRuntime, cacheState)
				results = append(results, result)
				logResult(t, result)
			})
		}

		musicCacheDir := filepath.Join(tmpDir, "cache_music_"+videoID)
		if err := os.MkdirAll(musicCacheDir, 0755); err != nil {
			t.Fatalf("create music cache dir: %v", err)
		}
		musicRuntime := NewRuntime()
		defer musicRuntime.CloseCachedEngine()

		for _, cacheState := range []string{"cold", "warm"} {
			cacheState := cacheState
			t.Run(fmt.Sprintf("Music_%s_%s", cacheState, videoID), func(t *testing.T) {
				home, _ := os.UserHomeDir()
				cookiePath := filepath.Join(home, ".config", "ytx", "cookies.txt")
				if _, err := os.Stat(cookiePath); os.IsNotExist(err) {
					t.Skip("Cookie file not found, skipping music mode test")
				}
				result := testMusicMode(t, musicCacheDir, videoID, cookiePath, musicRuntime, cacheState, false)
				results = append(results, result)
				logResult(t, result)
			})
		}

		musicPOCacheDir := filepath.Join(tmpDir, "cache_music_po_"+videoID)
		if err := os.MkdirAll(musicPOCacheDir, 0755); err != nil {
			t.Fatalf("create music po cache dir: %v", err)
		}
		musicPORuntime := NewRuntime()
		defer musicPORuntime.CloseCachedEngine()

		for _, cacheState := range []string{"cold", "warm"} {
			cacheState := cacheState
			t.Run(fmt.Sprintf("MusicPO_%s_%s", cacheState, videoID), func(t *testing.T) {
				home, _ := os.UserHomeDir()
				cookiePath := filepath.Join(home, ".config", "ytx", "cookies.txt")
				if _, err := os.Stat(cookiePath); os.IsNotExist(err) {
					t.Skip("Cookie file not found, skipping music+po mode test")
				}
				result := testMusicMode(t, musicPOCacheDir, videoID, cookiePath, musicPORuntime, cacheState, true)
				results = append(results, result)
				logResult(t, result)
			})
		}
	}

	updateGolden := os.Getenv("UPDATE_GOLDEN") == "1"

	existingGolden, err := loadGoldenImage()
	if !updateGolden && err == nil && len(existingGolden.Results) > 0 {
		t.Log("Comparing against existing golden image...")
		compareResults(t, results, existingGolden.Results)
	} else if updateGolden {
		t.Log("UPDATE_GOLDEN=1 set, skipping golden comparison")
	} else {
		t.Log("No existing golden image found, creating new baseline")
	}

	golden := GoldenImage{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Version:     4,
		Results:     results,
	}

	goldenJSON, _ := json.MarshalIndent(golden, "", "  ")
	t.Logf("\n=== Current Results ===\n%s\n", goldenJSON)

	if updateGolden || err != nil {
		if saveErr := saveGoldenImage(&golden); saveErr != nil {
			t.Logf("Failed to save golden image: %v", saveErr)
		} else {
			t.Logf("Golden image saved to: %s", goldenImagePath())
		}
	} else {
		t.Log("Set UPDATE_GOLDEN=1 to update the golden image")
	}
}

func TestBulkMusicProbeRegression(t *testing.T) {
	runBulkMusicProbeRegression(t, false)
}

func TestBulkMusicProbeRegressionPO(t *testing.T) {
	runBulkMusicProbeRegression(t, true)
}
