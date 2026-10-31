package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSanitizeMinecraftCommand(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"/gamerule keepinventory true", "gamerule keepinventory true"},
		{"gamerule keepinventory true", "gamerule keepinventory true"},
		{"  /list  ", "list"},
		{"/time set day", "time set day"},
		{"", ""},
	}

	for _, tt := range tests {
		got := sanitizeMinecraftCommand(tt.input)
		if got != tt.expected {
			t.Errorf("sanitizeMinecraftCommand(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestExtractCleanLogMessage(t *testing.T) {
	tests := []struct {
		name     string
		payload  interface{}
		expected string
	}{
		{
			name:     "GCE textPayload (Pure string)",
			payload:  "[Server] Server started.",
			expected: "[Server] Server started.",
		},
		{
			name:     "Docker stdout prefix string",
			payload:  "2026-08-17T06:30:15Z stdout: [Server] Server started.",
			expected: "[Server] Server started.",
		},
		{
			name: "Fluentd jsonPayload with message key",
			payload: map[string]interface{}{
				"message": "Player connected: PlayerOne",
				"stream":  "stdout",
			},
			expected: "Player connected: PlayerOne",
		},
		{
			name: "Docker jsonPayload with log key",
			payload: map[string]interface{}{
				"log": "Game rule keepinventory updated to true",
			},
			expected: "Game rule keepinventory updated to true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractCleanLogMessage(tt.payload)
			if got != tt.expected {
				t.Errorf("extractCleanLogMessage() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestVerifyHMACSignature(t *testing.T) {
	secret := "test_webhook_secret_key_12345"
	payload := []byte(`{"ref":"refs/heads/main","commits":[{"message":"update worlds.yaml"}]}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	validSignature := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !verifyHMACSignature(payload, validSignature, secret) {
		t.Errorf("verifyHMACSignature() failed for valid signature")
	}

	invalidSignature := "sha256=0000000000000000000000000000000000000000000000000000000000000000"
	if verifyHMACSignature(payload, invalidSignature, secret) {
		t.Errorf("verifyHMACSignature() passed for invalid signature")
	}
}

func TestGCPMetadataExternalIPMock(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flavor := r.Header.Get("Metadata-Flavor")
		if flavor != "Google" {
			http.Error(w, "Forbidden: Missing Metadata-Flavor header", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("34.120.10.5\n"))
	}))
	defer mockServer.Close()

	req, err := http.NewRequest("GET", mockServer.URL, nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}
	req.Header.Set("Metadata-Flavor", "Google")

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Failed to execute request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read body: %v", err)
	}
	if strings.TrimSpace(string(body)) != "34.120.10.5" {
		t.Errorf("Expected IP 34.120.10.5, got %s", strings.TrimSpace(string(body)))
	}
}

func TestCloudflareDNSEnvelopeMock(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader != "Bearer test_cf_api_token" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		response := map[string]interface{}{
			"success":  true,
			"errors":   []interface{}{},
			"messages": []interface{}{},
			"result": []map[string]interface{}{
				{
					"id":      "record_id_12345",
					"type":    "A",
					"name":    "alwaysfree.krmtn.org",
					"content": "34.120.10.5",
					"proxied": true,
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer mockServer.Close()

	req, _ := http.NewRequest("GET", mockServer.URL, nil)
	req.Header.Set("Authorization", "Bearer test_cf_api_token")

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Failed to execute request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}
}

func TestPlayerEventRegex(t *testing.T) {
	joinLog := "[15:30:12] Player connected: PlayerOne, xuid: 12345678"
	if matches := regexPlayerJoin.FindStringSubmatch(joinLog); len(matches) > 1 {
		if matches[1] != "PlayerOne" {
			t.Errorf("Expected player PlayerOne, got %s", matches[1])
		}
	} else {
		t.Errorf("Failed to match player join log")
	}

	leftLog := "[15:40:00] Player disconnected: PlayerTwo, xuid: 87654321"
	if matches := regexPlayerLeft.FindStringSubmatch(leftLog); len(matches) > 1 {
		if matches[1] != "PlayerTwo" {
			t.Errorf("Expected player PlayerTwo, got %s", matches[1])
		}
	} else {
		t.Errorf("Failed to match player leave log")
	}
}

func TestResolveWorldConfig(t *testing.T) {
	baseYAML := `
active_world: "%s"
defaults:
  settings:
    difficulty: "easy"
    pvp: true
    keepinventory: false
    showcoordinates: false
    dofiretick: true

worlds:
  - id: "kiseki"
    name: "Main World"
    emoji: "🌍"
    settings:
      seed: "12345"
      difficulty: "peaceful"
      pvp: false
      keepinventory: true
      showcoordinates: true
      dofiretick: false
  - id: "lobby"
    name: "Lobby"
    emoji: "🏛️"
    settings:
      difficulty: "normal"
      pvp: true
`
	yamlFor := func(active string) []byte {
		return []byte(strings.ReplaceAll(baseYAML, "%s", active))
	}

	assertCommands := func(t *testing.T, got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %d commands %v, want %d %v", len(got), got, len(want), want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("cmd[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	}

	t.Run("Success_ActiveWorldKiseki", func(t *testing.T) {
		resolved, err := resolveWorldConfig(yamlFor("kiseki"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resolved.ID != "kiseki" || resolved.Seed != "12345" {
			t.Errorf("resolved = %+v, want ID=kiseki Seed=12345", resolved)
		}
		assertCommands(t, resolved.Commands, []string{
			"difficulty peaceful",
			"gamerule dofiretick false",
			"gamerule keepinventory true",
			"gamerule pvp false",
			"gamerule showcoordinates true",
		})
	})

	t.Run("Success_FallbackToDefaultsForOmittedSettings", func(t *testing.T) {
		resolved, err := resolveWorldConfig(yamlFor("lobby"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resolved.Seed != "" {
			t.Errorf("Seed = %q, want empty", resolved.Seed)
		}
		assertCommands(t, resolved.Commands, []string{
			"difficulty normal",
			"gamerule dofiretick true",
			"gamerule keepinventory false",
			"gamerule pvp true",
			"gamerule showcoordinates false",
		})
	})

	t.Run("Error_MissingActiveWorld", func(t *testing.T) {
		_, err := resolveWorldConfig(yamlFor(""))
		if err == nil {
			t.Fatalf("expected error for missing active_world, got nil")
		}
		if !strings.Contains(err.Error(), "active_world") {
			t.Errorf("error %q should mention active_world", err.Error())
		}
	})

	t.Run("Error_UndefinedActiveWorld", func(t *testing.T) {
		_, err := resolveWorldConfig(yamlFor("nonexistent_world"))
		if err == nil {
			t.Fatalf("expected error for undefined active_world, got nil")
		}
		if !strings.Contains(err.Error(), "nonexistent_world") {
			t.Errorf("error %q should mention nonexistent_world", err.Error())
		}
	})

	t.Run("Error_MissingDefaultsKey", func(t *testing.T) {
		invalidYAML := []byte(`
active_world: "kiseki"
defaults:
  settings:
    difficulty: "easy"
    pvp: true

worlds:
  - id: "kiseki"
    settings:
      difficulty: "peaceful"
      pvp: false
      unknown_rule: true
`)
		_, err := resolveWorldConfig(invalidYAML)
		if err == nil {
			t.Fatalf("expected error for missing defaults key, got nil")
		}
		if !strings.Contains(err.Error(), "unknown_rule") {
			t.Errorf("error %q should mention unknown_rule", err.Error())
		}
	})

	t.Run("Error_NoDefaultsSection", func(t *testing.T) {
		noDefaultsYAML := []byte(`
active_world: "kiseki"
worlds:
  - id: "kiseki"
    settings:
      difficulty: "peaceful"
`)
		_, err := resolveWorldConfig(noDefaultsYAML)
		if err == nil {
			t.Fatalf("expected error for missing defaults section, got nil")
		}
	})
}

func TestFetchGCEInstanceStatusAndIPMock(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/computeMetadata/v1/instance/service-accounts/default/token":
			if r.Header.Get("Metadata-Flavor") != "Google" {
				http.Error(w, "missing Metadata-Flavor", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"mock-oauth2-token","token_type":"Bearer"}`))
		case "/compute/v1/projects/test-proj/zones/asia-northeast1-a/instances/minecraft01":
			if r.Header.Get("Authorization") != "Bearer mock-oauth2-token" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"status": "RUNNING",
				"networkInterfaces": [
					{
						"accessConfigs": [
							{"natIP": "34.85.100.200"}
						]
					}
				]
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	token, err := getGCEMetadataAccessToken(ctx, mockServer.URL)
	if err != nil {
		t.Fatalf("getGCEMetadataAccessToken() error: %v", err)
	}
	if token != "mock-oauth2-token" {
		t.Errorf("token = %q, want %q", token, "mock-oauth2-token")
	}

	status, ip, err := fetchGCEInstanceStatusAndIP(ctx, mockServer.URL, token, "test-proj", "asia-northeast1-a", "minecraft01")
	if err != nil {
		t.Fatalf("fetchGCEInstanceStatusAndIP() error: %v", err)
	}
	if status != "RUNNING" {
		t.Errorf("status = %q, want %q", status, "RUNNING")
	}
	if ip != "34.85.100.200" {
		t.Errorf("ip = %q, want %q", ip, "34.85.100.200")
	}
}

func TestTriggerEmptyServerTimer(t *testing.T) {
	PlayersMutex.Lock()
	CurrentPlayers = 0
	isTimerActive = false
	triggerEmptyServerTimerLocked(nil)
	activeAfterZeroSync := isTimerActive
	// Reset timer state so background sleep goroutine becomes a no-op.
	isTimerActive = false
	PlayersMutex.Unlock()

	if !activeAfterZeroSync {
		t.Errorf("expected isTimerActive to be true when CurrentPlayers == 0")
	}
}

func TestParseServerPropertiesLevelName(t *testing.T) {
	props := "#Minecraft server properties\r\n#level-name=commented_out\r\ngamemode=survival\r\nlevel-name=kiseki\r\nlevel-seed=12345\r\n"
	if got := parseServerPropertiesLevelName(props); got != "kiseki" {
		t.Errorf("parseServerPropertiesLevelName() = %q, want %q", got, "kiseki")
	}

	invalidProps := "level-name=../invalid/path\n"
	if got := parseServerPropertiesLevelName(invalidProps); got != "" {
		t.Errorf("parseServerPropertiesLevelName(invalid) = %q, want empty string", got)
	}
}

func TestBuildWorldSwitchScript(t *testing.T) {
	t.Run("Success_RewritesLevelNameAndSeed", func(t *testing.T) {
		script, err := buildWorldSwitchScript(&ResolvedWorldConfig{ID: "lobby", Seed: "-12345"})
		if err != nil {
			t.Fatalf("buildWorldSwitchScript() unexpected error: %v", err)
		}
		for _, want := range []string{
			"set -e",
			`s/^level-name=.*/level-name=lobby/`,
			`s/^level-seed=.*/level-seed=-12345/`,
			"/data/server.properties",
			"sudo docker rm -f minecraft-bedrock",
			"sudo google_metadata_script_runner startup",
		} {
			if !strings.Contains(script, want) {
				t.Errorf("script missing %q:\n%s", want, script)
			}
		}
	})

	t.Run("Success_EmptySeedMeansRandom", func(t *testing.T) {
		script, err := buildWorldSwitchScript(&ResolvedWorldConfig{ID: "kiseki", Seed: ""})
		if err != nil {
			t.Fatalf("buildWorldSwitchScript() unexpected error: %v", err)
		}
		if !strings.Contains(script, `s/^level-seed=.*/level-seed=/`) {
			t.Errorf("script should clear level-seed:\n%s", script)
		}
	})

	t.Run("Error_RejectsUnsafeValues", func(t *testing.T) {
		cases := []*ResolvedWorldConfig{
			nil,
			{ID: "../etc", Seed: ""},
			{ID: "lobby/x", Seed: ""},
			{ID: "lobby", Seed: "12/34"},
			{ID: "lobby", Seed: "abc"},
			{ID: "lobby", Seed: `"; rm -rf /`},
		}
		for _, c := range cases {
			if _, err := buildWorldSwitchScript(c); err == nil {
				t.Errorf("expected error for %+v, got nil", c)
			}
		}
	})
}

func TestBuildBatchCommandAndBackupScripts(t *testing.T) {
	cmds := []string{"difficulty peaceful", "gamerule keepinventory true"}
	batchScript := buildBatchSendCommandScript(cmds)
	if !strings.HasPrefix(batchScript, "set -e\n") {
		t.Errorf("batchScript should start with 'set -e', got: %s", batchScript)
	}
	if !strings.Contains(batchScript, "sleep 0.3") {
		t.Errorf("batchScript should contain 'sleep 0.3', got: %s", batchScript)
	}
	if !strings.Contains(batchScript, `sudo docker exec minecraft-bedrock send-command "difficulty peaceful"`) {
		t.Errorf("batchScript missing first command: %s", batchScript)
	}

	backupScript, err := buildWorldBackupScript("test-proj", "kiseki")
	if err != nil {
		t.Fatalf("buildWorldBackupScript() unexpected error: %v", err)
	}
	if !strings.Contains(backupScript, `gs://test-proj-minecraft-backup/world-data-${WORLD_ID}.tar.gz`) {
		t.Errorf("backupScript missing expected GCS path: %s", backupScript)
	}

	if _, err := buildWorldBackupScript("test-proj", "../bad"); err == nil {
		t.Errorf("expected error for invalid worldID in buildWorldBackupScript, got nil")
	}
}

func TestWaitForBedrockServerReadyWithRunner(t *testing.T) {
	t.Run("Success_ReadyAfterPolling", func(t *testing.T) {
		outputs := []string{
			"WAITING:startup_state=activating,mono=0",
			"WAITING:world_loading",
			"READY",
		}
		idx := 0
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		err := waitForBedrockServerReadyWithRunner(ctx, 10*time.Millisecond, "kiseki", func(cmd string) (string, error) {
			out := outputs[idx]
			if idx < len(outputs)-1 {
				idx++
			}
			return out, nil
		})
		if err != nil {
			t.Fatalf("waitForBedrockServerReadyWithRunner() unexpected error: %v", err)
		}
		if idx != 2 {
			t.Errorf("expected 2 state transitions before READY, got %d", idx)
		}
	})

	t.Run("Error_TimeoutIncludesLastStatus", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()

		err := waitForBedrockServerReadyWithRunner(ctx, 10*time.Millisecond, "kiseki", func(cmd string) (string, error) {
			return "WAITING:world_loading", nil
		})
		if err == nil {
			t.Fatalf("expected timeout error, got nil")
		}
		if !strings.Contains(err.Error(), "WAITING:world_loading") {
			t.Errorf("error %q should include last status 'WAITING:world_loading'", err.Error())
		}
	})
}
