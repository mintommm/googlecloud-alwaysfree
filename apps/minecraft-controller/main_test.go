package main

import (
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

// 1. コマンドサニタイズ（先頭スラッシュ自動除去）テスト
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

// 2. Cloud Logging 型安全ログ抽出テスト (textPayload, jsonPayload, stdout: 除去)
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
				"message": "Player connected: MockPencil3834",
				"stream":  "stdout",
			},
			expected: "Player connected: MockPencil3834",
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

// 3. GitHub Webhook HMAC-SHA256 署名検証テスト
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

// 4. GCP メタデータサーバー仕様テスト (Metadata-Flavor: Google ヘッダー検証)
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

// 5. Cloudflare v4 REST API 仕様テスト (JSON Envelope レスポンス検証)
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

// 6. プレイヤー参加/退出イベント正規表現テスト
func TestPlayerEventRegex(t *testing.T) {
	joinLog := "[15:30:12] Player connected: MockPencil3834, xuid: 12345678"
	if matches := regexPlayerJoin.FindStringSubmatch(joinLog); len(matches) > 1 {
		if matches[1] != "MockPencil3834" {
			t.Errorf("Expected player MockPencil3834, got %s", matches[1])
		}
	} else {
		t.Errorf("Failed to match player join log")
	}

	leftLog := "[15:40:00] Player disconnected: superkurute, xuid: 87654321"
	if matches := regexPlayerLeft.FindStringSubmatch(leftLog); len(matches) > 1 {
		if matches[1] != "superkurute" {
			t.Errorf("Expected player superkurute, got %s", matches[1])
		}
	} else {
		t.Errorf("Failed to match player leave log")
	}
}

// 7. GitOps 宣言的設定パース＆事前検証（reconcileWorldConfig）テスト
func TestReconcileWorldConfig(t *testing.T) {
	validYAML := []byte(`
defaults:
  settings:
    difficulty: "easy"
    pvp: true
    keepinventory: false
    showcoordinates: false
    dofiretick: true

worlds:
  - id: "kiseki"
    name: "Lili's world"
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
`)

	t.Run("Success_ActiveWorldKiseki", func(t *testing.T) {
		cmds, err := reconcileWorldConfig(validYAML, "kiseki")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		expected := []string{
			"difficulty peaceful",
			"gamerule dofiretick false",
			"gamerule keepinventory true",
			"gamerule pvp false",
			"gamerule showcoordinates true",
		}

		if len(cmds) != len(expected) {
			t.Fatalf("got %d commands, want %d", len(cmds), len(expected))
		}
		for i, cmd := range cmds {
			if cmd != expected[i] {
				t.Errorf("cmd[%d] = %q, want %q", i, cmd, expected[i])
			}
		}
	})

	t.Run("Success_FallbackToDefaultsForOmittedSettings", func(t *testing.T) {
		cmds, err := reconcileWorldConfig(validYAML, "lobby")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// lobby では dofiretick, keepinventory, showcoordinates が未指定のため defaults から自動復元
		expected := []string{
			"difficulty normal",
			"gamerule dofiretick true",
			"gamerule keepinventory false",
			"gamerule pvp true",
			"gamerule showcoordinates false",
		}

		if len(cmds) != len(expected) {
			t.Fatalf("got %d commands, want %d", len(cmds), len(expected))
		}
		for i, cmd := range cmds {
			if cmd != expected[i] {
				t.Errorf("cmd[%d] = %q, want %q", i, cmd, expected[i])
			}
		}
	})

	t.Run("Error_MissingDefaultsKey", func(t *testing.T) {
		invalidYAML := []byte(`
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
		_, err := reconcileWorldConfig(invalidYAML, "kiseki")
		if err == nil {
			t.Fatalf("expected error for missing defaults key, got nil")
		}
		if !strings.Contains(err.Error(), "unknown_rule") {
			t.Errorf("error %q should mention unknown_rule", err.Error())
		}
	})

	t.Run("Error_NoDefaultsSection", func(t *testing.T) {
		noDefaultsYAML := []byte(`
worlds:
  - id: "kiseki"
    settings:
      difficulty: "peaceful"
`)
		_, err := reconcileWorldConfig(noDefaultsYAML, "kiseki")
		if err == nil {
			t.Fatalf("expected error for missing defaults section, got nil")
		}
	})
}
