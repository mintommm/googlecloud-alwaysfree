package main

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"cloud.google.com/go/logging/logadmin"
	"github.com/bwmarrin/discordgo"
	"github.com/cloudflare/cloudflare-go"
	"google.golang.org/api/iterator"
	"gopkg.in/yaml.v3"
)

var (
	Token                = os.Getenv("DISCORD_TOKEN")
	GuildID              = os.Getenv("DISCORD_GUILD_ID")
	Zone                 = os.Getenv("MC_ZONE")
	InstanceName         = os.Getenv("MC_INSTANCE_NAME")
	NotificationChannel  = os.Getenv("DISCORD_NOTIFICATION_CHANNEL_ID")
	CFAPIToken           = os.Getenv("CF_API_TOKEN")
	CFZoneID             = os.Getenv("CF_ZONE_ID")
	GCPProjectID         = os.Getenv("GCP_PROJECT_ID")
	WebhookSecret        = os.Getenv("WEBHOOK_SECRET")
	globalDiscordSession *discordgo.Session
)

var (
	CurrentPlayers = 0
	PlayersMutex   sync.Mutex
	isTimerActive  = false
	emptyStartTime time.Time
	streamCancel   context.CancelFunc
	streamMu       sync.Mutex
	tickerCancel   context.CancelFunc
	tickerMu       sync.Mutex
	gitOpsMu       sync.Mutex
	jstLocation    *time.Location
)

var (
	regexPlayerJoin   = regexp.MustCompile(`Player connected:\s+([^,]+)`)
	regexPlayerLeft   = regexp.MustCompile(`Player disconnected:\s+([^,]+)`)
	regexListCount    = regexp.MustCompile(`There are (\d+)/\d+ players online`)
	regexDockerPrefix = regexp.MustCompile(`^(?:[0-9T:.\-+]+Z?\s+)?(?:stdout|stderr):\s*(.*)$`)
	regexValidWorldID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	// Bedrock level-seed is a signed 64-bit integer; an empty value means "random".
	regexValidWorldSeed = regexp.MustCompile(`^-?[0-9]*$`)
)

const (
	ConfigRepoDir = "/opt/minecraft-controller/config-repo"
)

type WorldEntry struct {
	ID       string                 `yaml:"id"`
	Name     string                 `yaml:"name"`
	Emoji    string                 `yaml:"emoji"`
	Settings map[string]interface{} `yaml:"settings"`
}

type WorldConfig struct {
	ActiveWorld string `yaml:"active_world"`
	Defaults    struct {
		Settings map[string]interface{} `yaml:"settings"`
	} `yaml:"defaults"`
	Worlds []WorldEntry `yaml:"worlds"`
}

type ResolvedWorldConfig struct {
	ID       string
	Name     string
	Emoji    string
	Seed     string
	Commands []string
}

func (w *ResolvedWorldConfig) DisplayLabel() string {
	if w == nil {
		return "kiseki"
	}
	var parts []string
	if strings.TrimSpace(w.Emoji) != "" {
		parts = append(parts, strings.TrimSpace(w.Emoji))
	}
	if strings.TrimSpace(w.Name) != "" {
		parts = append(parts, strings.TrimSpace(w.Name))
	}
	if len(parts) == 0 {
		return w.ID
	}
	return fmt.Sprintf("%s (`%s`)", strings.Join(parts, " "), w.ID)
}

func parseWorldConfig(yamlBytes []byte) (*WorldConfig, error) {
	var config WorldConfig
	if err := yaml.Unmarshal(yamlBytes, &config); err != nil {
		return nil, fmt.Errorf("worlds.yaml パースエラー: %w", err)
	}

	if len(config.Worlds) == 0 {
		return nil, fmt.Errorf("worlds.yaml にワールド定義が存在しません")
	}

	if config.Defaults.Settings == nil {
		return nil, fmt.Errorf("defaults.settings が定義されていません")
	}

	for _, w := range config.Worlds {
		if !regexValidWorldID.MatchString(w.ID) {
			return nil, fmt.Errorf("不正なワールドID形式です: %q", w.ID)
		}
	}

	config.ActiveWorld = strings.TrimSpace(config.ActiveWorld)
	if config.ActiveWorld == "" {
		return nil, fmt.Errorf("active_world が定義されていません")
	}
	if config.findWorld(config.ActiveWorld) == nil {
		return nil, fmt.Errorf("active_world %q が worlds に定義されていません", config.ActiveWorld)
	}

	return &config, nil
}

func (c *WorldConfig) findWorld(id string) *WorldEntry {
	for i := range c.Worlds {
		if c.Worlds[i].ID == id {
			return &c.Worlds[i]
		}
	}
	return nil
}

func resolveWorldConfig(yamlBytes []byte) (*ResolvedWorldConfig, error) {
	config, err := parseWorldConfig(yamlBytes)
	if err != nil {
		return nil, err
	}
	targetWorld := config.findWorld(config.ActiveWorld)

	// Require every world setting key (except seed) to have a baseline in defaults.settings
	// so removed keys revert to defaults instead of drifting in level.dat.
	var missingKeys []string
	for key := range targetWorld.Settings {
		if key == "seed" {
			continue
		}
		if _, ok := config.Defaults.Settings[key]; !ok {
			missingKeys = append(missingKeys, key)
		}
	}

	if len(missingKeys) > 0 {
		sort.Strings(missingKeys)
		return nil, fmt.Errorf("defaults.settings に未定義のキーが存在します: %s", strings.Join(missingKeys, ", "))
	}

	mergedSettings := make(map[string]interface{})
	for k, v := range config.Defaults.Settings {
		mergedSettings[k] = v
	}
	var seedStr string
	for k, v := range targetWorld.Settings {
		if k == "seed" {
			if v != nil {
				seedStr = strings.TrimSpace(fmt.Sprintf("%v", v))
			}
			continue
		}
		mergedSettings[k] = v
	}

	var cmds []string
	var keys []string
	for k := range mergedSettings {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		v := mergedSettings[k]
		if k == "difficulty" {
			cmds = append(cmds, fmt.Sprintf("difficulty %v", v))
		} else {
			cmds = append(cmds, fmt.Sprintf("gamerule %s %v", k, v))
		}
	}

	return &ResolvedWorldConfig{
		ID:       targetWorld.ID,
		Name:     targetWorld.Name,
		Emoji:    targetWorld.Emoji,
		Seed:     seedStr,
		Commands: cmds,
	}, nil
}

func init() {
	var err error
	jstLocation, err = time.LoadLocation("Asia/Tokyo")
	if err != nil {
		jstLocation = time.FixedZone("JST", 9*60*60)
	}
}

func main() {
	if Token == "" || GuildID == "" {
		log.Fatal("Required environment variables (DISCORD_TOKEN, DISCORD_GUILD_ID) must be set")
	}

	if Zone == "" {
		Zone = "asia-northeast1-a"
	}
	if InstanceName == "" {
		InstanceName = "minecraft01"
	}
	if GCPProjectID == "" {
		GCPProjectID = "mintommm-alwaysfree-gce"
	}

	go updateSelfDDNS()
	go startWebhookServer()

	dg, err := discordgo.New("Bot " + Token)
	if err != nil {
		log.Fatalf("Error creating Discord session: %v", err)
	}
	globalDiscordSession = dg

	dg.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		log.Printf("Bot logged in as: %v#%v", s.State.User.Username, s.State.User.Discriminator)
		go manageStreamLifecycle(dg)
		go startBackupTicker(dg)
	})
	dg.AddHandler(interactionCreate)

	if err := dg.Open(); err != nil {
		log.Fatalf("Error opening Discord connection: %v", err)
	}
	defer dg.Close()

	commands := []*discordgo.ApplicationCommand{
		{Name: "start", Description: "マインクラフトサーバーを起動します"},
		{Name: "stop", Description: "マインクラフトサーバーを停止します"},
		{Name: "status", Description: "サーバーのステータスとオンライン人数を確認します"},
		{Name: "panel", Description: "サーバー制御用のボタンパネルを表示します"},
		{
			Name:        "cmd",
			Description: "サーバー内で直接コマンドを実行します",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "command",
					Description: "実行するマインクラフトコマンド (先頭スラッシュは不要)",
					Required:    true,
				},
			},
		},
		{
			Name:        "logs",
			Description: "マインクラフトサーバーの直近ログを Cloud Logging から安全に取得します",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionInteger,
					Name:        "lines",
					Description: "取得する行数 (デフォルト: 15, 範囲: 5〜30)",
					Required:    false,
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "query",
					Description: "絞り込みキーワード (例: Steve, INFO, ERROR)",
					Required:    false,
				},
			},
		},
		{Name: "backup", Description: "手動で即時バックアップを実行し GCS へ退避します"},
	}

	if _, err := dg.ApplicationCommandBulkOverwrite(dg.State.User.ID, GuildID, commands); err != nil {
		log.Fatalf("Could not register application commands: %v", err)
	}

	go startDiscordWatchdog(dg)

	log.Println("Bot is ready. Managing Minecraft server and lifecycles...")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc

	stopBackupTicker()
	stopLogStream()
}

// Always Free GCE uses an ephemeral external IP to avoid static IP charges; Proxied=true terminates HTTPS at Cloudflare for GitHub Webhooks.
func updateSelfDDNS() {
	if CFAPIToken == "" || CFZoneID == "" {
		log.Println("Cloudflare 設定が未指定のため自己修復 DDNS をスキップします")
		return
	}

	ip, err := getGCEExternalIP()
	if err != nil {
		log.Printf("GCE 外部 IP 取得失敗: %v", err)
		return
	}

	api, err := cloudflare.NewWithAPIToken(CFAPIToken)
	if err != nil {
		log.Printf("Cloudflare クライアント初期化失敗: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	records, _, err := api.ListDNSRecords(ctx, cloudflare.ZoneIdentifier(CFZoneID), cloudflare.ListDNSRecordsParams{
		Name: "alwaysfree.krmtn.org",
		Type: "A",
	})
	if err != nil {
		log.Printf("DNS レコード取得失敗: %v", err)
		return
	}

	proxied := true
	if len(records) > 0 {
		if records[0].Content == ip {
			log.Printf("alwaysfree.krmtn.org は最新 IP (%s) です", ip)
			return
		}
		_, err = api.UpdateDNSRecord(ctx, cloudflare.ZoneIdentifier(CFZoneID), cloudflare.UpdateDNSRecordParams{
			ID:      records[0].ID,
			Type:    "A",
			Name:    "alwaysfree.krmtn.org",
			Content: ip,
			Proxied: &proxied,
		})
		if err != nil {
			log.Printf("alwaysfree.krmtn.org 更新失敗: %v", err)
			return
		}
	} else {
		_, err = api.CreateDNSRecord(ctx, cloudflare.ZoneIdentifier(CFZoneID), cloudflare.CreateDNSRecordParams{
			Type:    "A",
			Name:    "alwaysfree.krmtn.org",
			Content: ip,
			Proxied: &proxied,
		})
		if err != nil {
			log.Printf("alwaysfree.krmtn.org 作成失敗: %v", err)
			return
		}
	}
	log.Printf("【自己修復 DDNS 完了】alwaysfree.krmtn.org -> %s (Proxied: true)", ip)
}

// Bedrock uses UDP 19132, which Cloudflare proxy does not forward on standard plans; Proxied=false routes traffic directly to the ephemeral IP.
func updateMinecraftDNS(ip string) error {
	if CFAPIToken == "" || CFZoneID == "" {
		return nil
	}

	api, err := cloudflare.NewWithAPIToken(CFAPIToken)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	records, _, err := api.ListDNSRecords(ctx, cloudflare.ZoneIdentifier(CFZoneID), cloudflare.ListDNSRecordsParams{
		Name: "minecraft.krmtn.org",
		Type: "A",
	})
	if err != nil {
		return err
	}

	proxied := false
	if len(records) > 0 {
		if records[0].Content == ip {
			return nil
		}
		_, err = api.UpdateDNSRecord(ctx, cloudflare.ZoneIdentifier(CFZoneID), cloudflare.UpdateDNSRecordParams{
			ID:      records[0].ID,
			Type:    "A",
			Name:    "minecraft.krmtn.org",
			Content: ip,
			Proxied: &proxied,
		})
		return err
	}

	_, err = api.CreateDNSRecord(ctx, cloudflare.ZoneIdentifier(CFZoneID), cloudflare.CreateDNSRecordParams{
		Type:    "A",
		Name:    "minecraft.krmtn.org",
		Content: ip,
		Proxied: &proxied,
	})
	return err
}

// Wait for public DNS resolvers to observe the new IP before notifying players, preventing connection failures caused by negative caching.
func waitForDNSPropagation(expectedIP string, maxWait time.Duration) bool {
	deadline := time.Now().Add(maxWait)
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 2 * time.Second}
			return d.DialContext(ctx, "udp", "1.1.1.1:53")
		},
	}

	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		ips, err := r.LookupHost(ctx, "minecraft.krmtn.org")
		cancel()

		if err == nil {
			for _, ip := range ips {
				if ip == expectedIP {
					return true
				}
			}
		}
		time.Sleep(3 * time.Second)
	}
	return false
}

func getGCEExternalIP() (string, error) {
	req, err := http.NewRequest("GET", "http://metadata.google.internal/computeMetadata/v1/instance/network-interfaces/0/access-configs/0/external-ip", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

// discordgo's internal reconnect can transiently drop DataReady; wait for 3 consecutive failures before forcing Close/Open or exiting for systemd recovery.
func startDiscordWatchdog(dg *discordgo.Session) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	consecutiveFailures := 0
	for range ticker.C {
		healthy := dg != nil && dg.DataReady
		if healthy {
			if _, err := dg.User("@me"); err != nil {
				healthy = false
			}
		}

		if healthy {
			consecutiveFailures = 0
			continue
		}

		consecutiveFailures++
		if consecutiveFailures < 3 {
			log.Printf("Discord Gateway check failed (%d/3): waiting for native auto-reconnect...", consecutiveFailures)
			continue
		}

		log.Printf("Discord Gateway check failed (%d/3): attempting explicit reconnect...", consecutiveFailures)
		_ = dg.Close()
		if err := dg.Open(); err != nil {
			log.Printf("Discord Gateway reconnect error (%d/3): %v", consecutiveFailures, err)
			log.Fatalf("Discord Gateway remained disconnected for 3 consecutive checks; exiting for systemd restart")
		} else {
			log.Println("Discord Gateway reconnected")
			consecutiveFailures = 0
		}
	}
}

func getGCEMetadataAccessToken(ctx context.Context, metadataBaseURL string) (string, error) {
	url := strings.TrimRight(metadataBaseURL, "/") + "/computeMetadata/v1/instance/service-accounts/default/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata token status %d", resp.StatusCode)
	}

	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if payload.AccessToken == "" {
		return "", fmt.Errorf("empty access_token in metadata response")
	}
	return payload.AccessToken, nil
}

// Query Compute Engine REST API directly instead of spawning python-based gcloud subprocesses, which cause high CPU spikes on e2-micro.
func fetchGCEInstanceStatusAndIP(ctx context.Context, computeBaseURL, token, project, zone, instance string) (string, string, error) {
	url := fmt.Sprintf("%s/compute/v1/projects/%s/zones/%s/instances/%s",
		strings.TrimRight(computeBaseURL, "/"), project, zone, instance)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("compute API status %d", resp.StatusCode)
	}

	var inst struct {
		Status            string `json:"status"`
		NetworkInterfaces []struct {
			AccessConfigs []struct {
				NatIP string `json:"natIP"`
			} `json:"accessConfigs"`
		} `json:"networkInterfaces"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&inst); err != nil {
		return "", "", err
	}

	var natIP string
	if len(inst.NetworkInterfaces) > 0 && len(inst.NetworkInterfaces[0].AccessConfigs) > 0 {
		natIP = inst.NetworkInterfaces[0].AccessConfigs[0].NatIP
	}
	return inst.Status, natIP, nil
}

func getMinecraftInstanceIP() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if token, err := getGCEMetadataAccessToken(ctx, "http://metadata.google.internal"); err == nil {
		if _, ip, err := fetchGCEInstanceStatusAndIP(ctx, "https://compute.googleapis.com", token, GCPProjectID, Zone, InstanceName); err == nil && ip != "" {
			return ip, nil
		}
	}

	cmd := exec.CommandContext(ctx, "gcloud", "compute", "instances", "describe", InstanceName,
		"--zone="+Zone, "--format=get(networkInterfaces[0].accessConfigs[0].natIP)")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func manageStreamLifecycle(dg *discordgo.Session) {
	streamMu.Lock()
	if streamCancel != nil {
		streamMu.Unlock()
		return
	}
	var ctx context.Context
	ctx, streamCancel = context.WithCancel(context.Background())
	streamMu.Unlock()

	defer func() {
		streamMu.Lock()
		streamCancel = nil
		streamMu.Unlock()
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
			if isGCEInstanceRunning() {
				readyCtx, readyCancel := context.WithTimeout(ctx, 90*time.Second)
				err := waitForBedrockServerReady(readyCtx, 3*time.Second, "")
				readyCancel()
				if err != nil {
					log.Printf("ログストリーム待機中断 (コンテナ未準備): %v", err)
					select {
					case <-ctx.Done():
						return
					case <-time.After(5 * time.Second):
					}
					continue
				}

				syncOnlinePlayersDirect(dg)
				log.Println("【ストリーム開始】ログ監視ストリームを確立します")
				err = startLogStreamProcess(ctx, dg)
				if err != nil {
					log.Printf("ストリーム切断: %v。5秒後に再接続します", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			} else {
				select {
				case <-ctx.Done():
					return
				case <-time.After(30 * time.Second):
				}
			}
		}
	}
}

func stopLogStream() {
	streamMu.Lock()
	if streamCancel != nil {
		streamCancel()
		streamCancel = nil
		log.Println("【ストリーム停止】常時ログストリーミングを終了しました")
	}
	streamMu.Unlock()
}

const (
	MinecraftInternalHost = "minecraft01.asia-northeast1-a.c.mintommm-alwaysfree-gce.internal"
	BotSSHKeyPath         = "/root/.ssh/bot_id_ed25519"
)

func startLogStreamProcess(ctx context.Context, dg *discordgo.Session) error {
	var cmd *exec.Cmd
	if _, err := os.Stat(BotSSHKeyPath); err == nil {
		cmd = exec.CommandContext(ctx, "ssh",
			"-i", BotSSHKeyPath,
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "ServerAliveInterval=15",
			"-o", "ServerAliveCountMax=3",
			"-o", "ConnectTimeout=5",
			"bot@"+MinecraftInternalHost,
			"sudo docker logs -f --tail=20 minecraft-bedrock",
		)
	} else {
		cmd = exec.CommandContext(ctx, "gcloud", "compute", "ssh", InstanceName,
			"--zone="+Zone,
			"--internal-ip",
			"--quiet",
			"--ssh-flag=-o ServerAliveInterval=15",
			"--ssh-flag=-o ServerAliveCountMax=3",
			"--command=sudo docker logs -f --tail=20 minecraft-bedrock",
		)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return err
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		handleLogLineEvents(dg, line)
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%w, stderr: %s", err, strings.TrimSpace(stderrBuf.String()))
	}
	return nil
}

// Caller must hold PlayersMutex.
func triggerEmptyServerTimerLocked(dg *discordgo.Session) {
	if CurrentPlayers == 0 && !isTimerActive {
		isTimerActive = true
		emptyStartTime = time.Now()
		if dg != nil && NotificationChannel != "" {
			_, _ = dg.ChannelMessageSend(NotificationChannel, "プレイヤー数が0人になりました。10分後に自動停止します。")
		}

		go func(startTime time.Time) {
			time.Sleep(10 * time.Minute)
			PlayersMutex.Lock()
			if isTimerActive && emptyStartTime.Equal(startTime) {
				if dg != nil && NotificationChannel != "" {
					_, _ = dg.ChannelMessageSend(NotificationChannel, "プレイヤー0人の状態が10分継続したため、自動シャットダウンを実行します。")
				}
				executeOfflineBackupSequence(dg)
				isTimerActive = false
			}
			PlayersMutex.Unlock()
		}(emptyStartTime)
	}
}

func handleLogLineEvents(dg *discordgo.Session, line string) {
	if NotificationChannel == "" {
		return
	}

	if matches := regexPlayerJoin.FindStringSubmatch(line); len(matches) > 1 {
		player := matches[1]
		PlayersMutex.Lock()
		CurrentPlayers++
		isTimerActive = false
		PlayersMutex.Unlock()
		_, _ = dg.ChannelMessageSend(NotificationChannel, fmt.Sprintf("📥 プレイヤー **%s** が参加しました。", player))
		return
	}

	if matches := regexPlayerLeft.FindStringSubmatch(line); len(matches) > 1 {
		player := matches[1]
		PlayersMutex.Lock()
		CurrentPlayers--
		if CurrentPlayers < 0 {
			CurrentPlayers = 0
		}
		triggerEmptyServerTimerLocked(dg)
		PlayersMutex.Unlock()
		_, _ = dg.ChannelMessageSend(NotificationChannel, fmt.Sprintf("📤 プレイヤー **%s** が退出しました。", player))
		return
	}
}

func startBackupTicker(dg *discordgo.Session) {
	tickerMu.Lock()
	if tickerCancel != nil {
		tickerMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	tickerCancel = cancel
	tickerMu.Unlock()

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if isGCEInstanceRunning() {
				log.Println("【定期バックアップ開始】サイレントバックアップを実行します")
				if _, err := executeOnlineBackupFlow(); err != nil {
					log.Printf("定期バックアップ失敗: %v", err)
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func stopBackupTicker() {
	tickerMu.Lock()
	if tickerCancel != nil {
		tickerCancel()
		tickerCancel = nil
	}
	tickerMu.Unlock()
}

func parseServerPropertiesLevelName(content string) string {
	var levelName string
	for _, rawLine := range strings.Split(content, "\n") {
		line := strings.TrimSpace(rawLine)
		if strings.HasPrefix(line, "#") || !strings.HasPrefix(line, "level-name=") {
			continue
		}
		val := strings.TrimSpace(strings.TrimPrefix(line, "level-name="))
		if regexValidWorldID.MatchString(val) {
			levelName = val
		}
	}
	return levelName
}

// Read server.properties from the running container, or via a lightweight alpine mount if the container is currently stopped.
func getServerPropertiesWorldID() string {
	cmd := `sudo docker exec minecraft-bedrock sh -c 'cat /data/server.properties 2>/dev/null' 2>/dev/null || sudo docker run --rm -v minecraft-data:/data:ro alpine sh -c 'cat /data/server.properties 2>/dev/null' 2>/dev/null || true`
	out, err := executeRemoteCommandWithTimeout(cmd, 15*time.Second)
	if err != nil {
		return ""
	}
	return parseServerPropertiesLevelName(out)
}

func loadResolvedWorld() (*ResolvedWorldConfig, error) {
	yamlBytes, err := os.ReadFile(ConfigRepoDir + "/worlds.yaml")
	if err != nil {
		return nil, err
	}
	return resolveWorldConfig(yamlBytes)
}

// Prefer server.properties over worlds.yaml so backups target the world actually mounted on disk even before a pending world switch is applied.
func resolveBackupTargetWorldID() string {
	if propsID := getServerPropertiesWorldID(); propsID != "" {
		return propsID
	}
	if resolved, err := loadResolvedWorld(); err == nil {
		return resolved.ID
	}
	return "kiseki"
}

func buildWorldBackupScript(projectID, worldID string) (string, error) {
	trimmedWorld := strings.TrimSpace(worldID)
	if !regexValidWorldID.MatchString(trimmedWorld) {
		return "", fmt.Errorf("不正なワールドID形式です: %q", worldID)
	}
	script := fmt.Sprintf(`sudo docker run --rm -i \
		-e WORLD_ID=%q \
		-v minecraft-data:/data:ro \
		google/cloud-sdk:alpine sh -s << 'EOF'
set -e
cd /data/worlds
if [ ! -d "$WORLD_ID" ]; then
    echo "backup target directory not found: $WORLD_ID" >&2
    exit 1
fi
tar -czf - "$WORLD_ID" | gcloud storage cp - "gs://%s-minecraft-backup/world-data-${WORLD_ID}.tar.gz" --quiet
EOF`, trimmedWorld, projectID)
	return script, nil
}

// Batch commands into a single SSH execution with 0.3s pacing to avoid per-command SSH handshake overhead and console stdin pipe contention.
func buildBatchSendCommandScript(cmds []string) string {
	var lines []string
	lines = append(lines, "set -e")
	for idx, cmdStr := range cmds {
		if idx > 0 {
			lines = append(lines, "sleep 0.3")
		}
		lines = append(lines, fmt.Sprintf("sudo docker exec minecraft-bedrock send-command %q", cmdStr))
	}
	return strings.Join(lines, "\n")
}

// Wait for google-startup-scripts.service to finish first so commands are not sent to a stale --restart=always container that minecraft-startup.sh is about to stop and recreate.
func checkBedrockServerReadyScript(expectedWorldID string) string {
	worldCheck := ""
	if strings.TrimSpace(expectedWorldID) != "" {
		worldCheck = fmt.Sprintf(`
CUR_WORLD=$(sudo docker exec minecraft-bedrock sh -c 'grep "^level-name=" /data/server.properties 2>/dev/null | tail -n 1 | cut -d= -f2- | tr -d "\r[:space:]"' 2>/dev/null || true)
if [ "$CUR_WORLD" != %q ]; then
    echo "WAITING:world_mismatch=$CUR_WORLD"
    exit 0
fi`, strings.TrimSpace(expectedWorldID))
	}

	return fmt.Sprintf(`STATE=$(systemctl show -p ActiveState --value google-startup-scripts.service 2>/dev/null || echo "unknown")
MONO=$(systemctl show -p ExecMainStartTimestampMonotonic --value google-startup-scripts.service 2>/dev/null || echo "0")
if [ "$STATE" != "inactive" ] || [ "${MONO:-0}" = "0" ]; then
    echo "WAITING:startup_state=${STATE},mono=${MONO}"
    exit 0
fi
RUNNING=$(sudo docker inspect -f '{{.State.Running}}' minecraft-bedrock 2>/dev/null || echo "false")
if [ "$RUNNING" != "true" ]; then
    echo "WAITING:container_running=${RUNNING}"
    exit 0
fi%s
if sudo docker logs minecraft-bedrock 2>&1 | grep -q "Server started."; then
    echo "READY"
else
    echo "WAITING:world_loading"
fi`, worldCheck)
}

func waitForBedrockServerReadyWithRunner(ctx context.Context, pollInterval time.Duration, expectedWorldID string, runner func(string) (string, error)) error {
	script := checkBedrockServerReadyScript(expectedWorldID)
	var lastStatus string
	for {
		out, err := runner(script)
		trimmed := strings.TrimSpace(out)
		if err == nil && strings.Contains(trimmed, "READY") {
			return nil
		}
		if err != nil {
			lastStatus = err.Error()
		} else {
			lastStatus = trimmed
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("Bedrockサーバー準備待機タイムアウト (%s): %w", lastStatus, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

func waitForBedrockServerReady(ctx context.Context, pollInterval time.Duration, expectedWorldID string) error {
	return waitForBedrockServerReadyWithRunner(ctx, pollInterval, expectedWorldID, func(cmd string) (string, error) {
		return executeRemoteCommandWithTimeout(cmd, 15*time.Second)
	})
}

// Delegate container recreation and GCS restore to google_metadata_script_runner startup so world switching shares the exact same boot path as instance startup.
func buildWorldSwitchScript(world *ResolvedWorldConfig) (string, error) {
	if world == nil || !regexValidWorldID.MatchString(world.ID) {
		return "", fmt.Errorf("不正なワールドIDです")
	}
	if !regexValidWorldSeed.MatchString(world.Seed) {
		return "", fmt.Errorf("不正なシード値形式です: %q", world.Seed)
	}
	script := fmt.Sprintf(`set -e
sudo docker run --rm -v minecraft-data:/data alpine sh -c 'sed -i -e "s/^level-name=.*/level-name=%s/" -e "s/^level-seed=.*/level-seed=%s/" /data/server.properties'
sudo docker rm -f minecraft-bedrock || true
sudo google_metadata_script_runner startup`, world.ID, world.Seed)
	return script, nil
}

func switchWorldIfNeeded(world *ResolvedWorldConfig) (string, error) {
	currentWorldID := getServerPropertiesWorldID()
	if currentWorldID == world.ID {
		return "", nil
	}
	log.Printf("【ワールド切り替え】%q -> %q", currentWorldID, world.ID)

	_, _ = executeRemoteCommandWithTimeout("sudo docker stop -t 10 minecraft-bedrock || true", 60*time.Second)

	// Abort the switch if backing up the previous world fails, preventing GCS from holding a stale archive when the world is later restored.
	if currentWorldID != "" {
		backupScript, err := buildWorldBackupScript(GCPProjectID, currentWorldID)
		if err != nil {
			return currentWorldID, err
		}
		if _, err := executeRemoteCommandWithTimeout(backupScript, 5*time.Minute); err != nil {
			return currentWorldID, fmt.Errorf("切り替え前の旧ワールド (%s) バックアップに失敗したため切り替えを中断しました: %w", currentWorldID, err)
		}
		log.Printf("【旧ワールド退避完了】world-data-%s.tar.gz を GCS へ保存しました", currentWorldID)
	}

	switchScript, err := buildWorldSwitchScript(world)
	if err != nil {
		return currentWorldID, err
	}
	if _, err := executeRemoteCommandWithTimeout(switchScript, 5*time.Minute); err != nil {
		return currentWorldID, fmt.Errorf("ワールド (%s) への切り替えに失敗しました: %w", world.ID, err)
	}
	return currentWorldID, nil
}

func executeOnlineBackupFlow() (string, error) {
	worldID := resolveBackupTargetWorldID()
	backupScript, err := buildWorldBackupScript(GCPProjectID, worldID)
	if err != nil {
		return "", err
	}

	// Freeze LevelDB compaction on disk during tar streaming so the archive snapshot is consistent.
	_, err = executeRemoteCommandWithTimeout("sudo docker exec minecraft-bedrock send-command \"save hold\"", 60*time.Second)
	if err != nil {
		return worldID, fmt.Errorf("failed to send save hold: %w", err)
	}

	time.Sleep(3 * time.Second)

	_, err = executeRemoteCommandWithTimeout(backupScript, 5*time.Minute)
	_, _ = executeRemoteCommandWithTimeout("sudo docker exec minecraft-bedrock send-command \"save resume\"", 60*time.Second)

	if err != nil {
		return worldID, fmt.Errorf("GCS バックアップ失敗: %w", err)
	}

	log.Printf("【バックアップ完了】gs://%s-minecraft-backup/world-data-%s.tar.gz へ退避しました", GCPProjectID, worldID)
	return worldID, nil
}

func executeOfflineBackupSequence(dg *discordgo.Session) {
	worldID := resolveBackupTargetWorldID()

	_, _ = executeRemoteCommandWithTimeout("sudo docker stop -t 10 minecraft-bedrock", 60*time.Second)
	// Remove the container on shutdown so Docker's --restart=always does not briefly start the old container on next boot before minecraft-startup.sh runs.
	_, _ = executeRemoteCommandWithTimeout("sudo docker rm -f minecraft-bedrock || true", 30*time.Second)
	stopLogStream()

	if backupScript, err := buildWorldBackupScript(GCPProjectID, worldID); err == nil {
		_, _ = executeRemoteCommandWithTimeout(backupScript, 5*time.Minute)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer stopCancel()
	_ = exec.CommandContext(stopCtx, "gcloud", "compute", "instances", "stop", InstanceName, "--zone="+Zone, "--quiet").Run()
	if dg != nil && NotificationChannel != "" {
		_, _ = dg.ChannelMessageSend(NotificationChannel, fmt.Sprintf("マインクラフトサーバー（ワールド: `%s`）は正常に停止し、インスタンスは停止状態になりました。", worldID))
	}
}

func executeRemoteCommandGetStdout(commandLine string) (string, error) {
	return executeRemoteCommandWithTimeout(commandLine, 60*time.Second)
}

func executeRemoteCommandWithTimeout(commandLine string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var cmd *exec.Cmd
	if _, err := os.Stat(BotSSHKeyPath); err == nil {
		cmd = exec.CommandContext(ctx, "ssh",
			"-i", BotSSHKeyPath,
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "ConnectTimeout=5",
			"bot@"+MinecraftInternalHost,
			commandLine,
		)
	} else {
		cmd = exec.CommandContext(ctx, "gcloud", "compute", "ssh", InstanceName,
			"--zone="+Zone,
			"--internal-ip",
			"--quiet",
			"--command="+commandLine,
		)
	}

	var stdoutBuf, stderrBuf strings.Builder
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("error: %v, stderr: %s", err, stderrBuf.String())
	}
	return stdoutBuf.String(), nil
}

func isGCEInstanceRunning() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if token, err := getGCEMetadataAccessToken(ctx, "http://metadata.google.internal"); err == nil {
		if status, _, err := fetchGCEInstanceStatusAndIP(ctx, "https://compute.googleapis.com", token, GCPProjectID, Zone, InstanceName); err == nil {
			return status == "RUNNING"
		}
	}

	cmd := exec.CommandContext(ctx, "gcloud", "compute", "instances", "describe", InstanceName,
		"--zone="+Zone, "--format=get(status)")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "RUNNING"
}

func syncOnlinePlayersDirect(dg *discordgo.Session) {
	_, err := executeRemoteCommandGetStdout("sudo docker exec minecraft-bedrock send-command list")
	if err != nil {
		return
	}
	time.Sleep(500 * time.Millisecond)
	logOut, err := executeRemoteCommandGetStdout("sudo docker logs --tail=5 minecraft-bedrock")
	if err != nil {
		return
	}

	lines := strings.Split(logOut, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if matches := regexListCount.FindStringSubmatch(lines[i]); len(matches) > 1 {
			var count int
			_, _ = fmt.Sscanf(matches[1], "%d", &count)
			PlayersMutex.Lock()
			CurrentPlayers = count
			if count > 0 {
				isTimerActive = false
			} else {
				triggerEmptyServerTimerLocked(dg)
			}
			PlayersMutex.Unlock()
			log.Printf("【同期完了】インメモリオンラインプレイヤー数を実態（%d人）に補正しました", count)
			return
		}
	}
}

// Cloud Logging delivers GCE syslog entries as string textPayload and Docker/Fluentd entries as structured jsonPayload.
func extractCleanLogMessage(payload interface{}) string {
	var rawText string

	switch p := payload.(type) {
	case string:
		rawText = p
	case map[string]interface{}:
		if msg, ok := p["message"].(string); ok {
			rawText = msg
		} else if logStr, ok := p["log"].(string); ok {
			rawText = logStr
		} else {
			b, _ := json.Marshal(p)
			rawText = string(b)
		}
	default:
		rawText = fmt.Sprintf("%v", p)
	}

	trimmed := strings.TrimSpace(rawText)
	if matches := regexDockerPrefix.FindStringSubmatch(trimmed); len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}
	return trimmed
}

func handleCloudLogsCommand(s *discordgo.Session, i *discordgo.InteractionCreate, lines int, keyword string) {
	if lines <= 0 || lines > 30 {
		lines = 15
	}

	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		client, err := logadmin.NewClient(ctx, GCPProjectID)
		if err != nil {
			_, _ = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
				Content: fmt.Sprintf("❌ Cloud Logging クライアント初期化失敗: %v", err),
			})
			return
		}
		defer client.Close()

		filter := fmt.Sprintf(`resource.type="gce_instance" AND resource.labels.instance_id="%s"`, InstanceName)
		if keyword != "" {
			filter += fmt.Sprintf(` AND textPayload:"%s"`, keyword)
		}

		it := client.Entries(ctx, logadmin.Filter(filter), logadmin.NewestFirst())

		var logs []string
		for len(logs) < lines {
			entry, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				log.Printf("ログ反復取得エラー: %v", err)
				break
			}

			msg := extractCleanLogMessage(entry.Payload)
			if msg == "" {
				continue
			}
			timestamp := entry.Timestamp.In(jstLocation).Format("2006-01-02 15:04:05")
			logs = append(logs, fmt.Sprintf("[%s] %s", timestamp, msg))
		}

		// logadmin.NewestFirst() fetches the most recent N entries; reverse them to display in chronological order.
		for l, r := 0, len(logs)-1; l < r; l, r = l+1, r-1 {
			logs[l], logs[r] = logs[r], logs[l]
		}

		logBlock := strings.Join(logs, "\n")
		if logBlock == "" {
			logBlock = "指定された条件に一致するログは見つかりませんでした。"
		} else if len(logBlock) > 3800 {
			logBlock = logBlock[len(logBlock)-3800:] + "\n...(文字数制限のため古いログを省略しました)"
		}

		title := fmt.Sprintf("📜 マインクラフト サーバーログ (直近 %d 行)", len(logs))
		if keyword != "" {
			title = fmt.Sprintf("📜 マインクラフト サーバーログ (絞り込み: \"%s\")", keyword)
		}

		_, _ = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
			Embeds: []*discordgo.MessageEmbed{
				{
					Title:       title,
					Description: fmt.Sprintf("```log\n%s\n```", logBlock),
					Color:       0x3498db,
					Footer: &discordgo.MessageEmbedFooter{
						Text: "Cloud Logging API | 全ログは GCP コンソールで検索可能",
					},
				},
			},
		})
	}()
}

func sanitizeMinecraftCommand(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if strings.HasPrefix(cmd, "/") {
		cmd = strings.TrimPrefix(cmd, "/")
	}
	return cmd
}

func interactionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand && i.Type != discordgo.InteractionMessageComponent {
		return
	}

	var actionName string
	var callerMention string
	if i.Member != nil && i.Member.User != nil {
		callerMention = fmt.Sprintf("<@%s>", i.Member.User.ID)
	} else if i.User != nil {
		callerMention = fmt.Sprintf("<@%s>", i.User.ID)
	}

	if i.Type == discordgo.InteractionApplicationCommand {
		actionName = i.ApplicationCommandData().Name
	} else if i.Type == discordgo.InteractionMessageComponent {
		actionName = i.MessageComponentData().CustomID
	}

	switch actionName {
	case "panel":
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "🎛️ **マインクラフトサーバー 遠隔制御パネル**\n以下のボタンからインスタンスおよびプロセスの状態を操作できます。",
				Components: []discordgo.MessageComponent{
					discordgo.ActionsRow{
						Components: []discordgo.MessageComponent{
							discordgo.Button{
								Label:    "サーバー起動",
								Style:    discordgo.SuccessButton,
								CustomID: "start",
							},
							discordgo.Button{
								Label:    "サーバー停止",
								Style:    discordgo.DangerButton,
								CustomID: "stop",
							},
							discordgo.Button{
								Label:    "ステータス確認",
								Style:    discordgo.PrimaryButton,
								CustomID: "status",
							},
						},
					},
				},
			},
		})

	case "start":
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: "🚀 サーバー起動要求を送信しました。DNS更新およびワールド準備・ルール適用を行っています..."},
		})
		go func() {
			startCtx, startCancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer startCancel()
			cmdStart := exec.CommandContext(startCtx, "gcloud", "compute", "instances", "start", InstanceName, "--zone="+Zone, "--quiet")
			if err := cmdStart.Run(); err != nil {
				_, _ = s.ChannelMessageSend(i.ChannelID, fmt.Sprintf("❌ GCE 起動失敗: %v", err))
				return
			}

			ip, err := getMinecraftInstanceIP()
			if err != nil {
				_, _ = s.ChannelMessageSend(i.ChannelID, fmt.Sprintf("⚠️ インスタンス IP 取得失敗: %v", err))
				return
			}

			if err := updateMinecraftDNS(ip); err != nil {
				log.Printf("minecraft.krmtn.org 更新エラー: %v", err)
			}

			propagated := waitForDNSPropagation(ip, 30*time.Second)
			statusNote := "DNS 反映完了 ✅"
			if !propagated {
				statusNote = "DNS 伝播中 (数分かかる場合があります) ⏳"
			}

			resolvedWorld, gitOpsErr := applyGitOpsChanges()
			worldLabel := "kiseki"
			if resolvedWorld != nil {
				worldLabel = resolvedWorld.DisplayLabel()
			}
			ruleNote := "ワールドルール適用完了 ✅"
			if gitOpsErr != nil {
				ruleNote = fmt.Sprintf("ワールドルール適用エラー ⚠️ (%v)", gitOpsErr)
			}

			msg := fmt.Sprintf("%s 🟢 **マインクラフトサーバーの起動が完了しました！**\nワールド: %s (%s)\nサーバーアドレス: `minecraft.krmtn.org`\nIPアドレス: `%s` (%s)",
				callerMention, worldLabel, ruleNote, ip, statusNote)
			_, _ = s.ChannelMessageSend(i.ChannelID, msg)
			go manageStreamLifecycle(s)
		}()

	case "stop":
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: "🛑 サーバーのシャットダウン処理を実行します..."},
		})
		go func() {
			executeOfflineBackupSequence(s)
		}()

	case "status":
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: "現在のサーバー状態を確認中..."},
		})
		go func() {
			worldLabel := "kiseki"
			if resolved, err := loadResolvedWorld(); err == nil {
				worldLabel = resolved.DisplayLabel()
			}
			if !isGCEInstanceRunning() {
				_, _ = s.ChannelMessageSend(i.ChannelID, fmt.Sprintf("🔴 GCEインスタンス状態: **TERMINATED** (停止中)\n🗺️ 設定ワールド: %s", worldLabel))
				return
			}
			PlayersMutex.Lock()
			count := CurrentPlayers
			PlayersMutex.Unlock()
			_, _ = s.ChannelMessageSend(i.ChannelID, fmt.Sprintf("🟢 GCEインスタンス状態: **RUNNING** (稼働中)\n🗺️ 有効ワールド: %s\n👥 オンラインプレイヤー数: **%d人**", worldLabel, count))
		}()

	case "cmd":
		var rawCmd string
		if i.Type == discordgo.InteractionApplicationCommand {
			options := i.ApplicationCommandData().Options
			if len(options) > 0 {
				rawCmd = options[0].StringValue()
			}
		}

		cleanCmd := sanitizeMinecraftCommand(rawCmd)
		if cleanCmd == "" {
			_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{Content: "❌ コマンドが空です。"},
			})
			return
		}

		go func() {
			remoteCommand := fmt.Sprintf("sudo docker exec minecraft-bedrock send-command \"%s\"", cleanCmd)
			_, err := executeRemoteCommandGetStdout(remoteCommand)
			if err != nil {
				_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Embeds: []*discordgo.MessageEmbed{
							{
								Title:       "💻 コマンド送信失敗",
								Description: fmt.Sprintf("実行者: %s\nコマンド: `%s`\nステータス: ❌ 送信失敗 (%v)", callerMention, cleanCmd, err),
								Color:       0xe74c3c,
							},
						},
					},
				})
				return
			}

			_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Embeds: []*discordgo.MessageEmbed{
						{
							Title:       "💻 コマンド送信完了",
							Description: fmt.Sprintf("実行者: %s\nコマンド: `%s`\nステータス: ✅ サーバーへ正常に送信されました", callerMention, cleanCmd),
							Color:       0x3498db,
						},
					},
				},
			})
		}()

	case "logs":
		lines := 15
		keyword := ""
		if i.Type == discordgo.InteractionApplicationCommand {
			for _, opt := range i.ApplicationCommandData().Options {
				if opt.Name == "lines" {
					lines = int(opt.IntValue())
				} else if opt.Name == "query" {
					keyword = opt.StringValue()
				}
			}
		}
		handleCloudLogsCommand(s, i, lines, keyword)

	case "backup":
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: "💾 バックアップを開始します..."},
		})
		go func() {
			if !isGCEInstanceRunning() {
				_, _ = s.ChannelMessageSend(i.ChannelID, "❌ サーバーが停止しているためバックアップを実行できません。")
				return
			}
			worldID, err := executeOnlineBackupFlow()
			if err != nil {
				_, _ = s.ChannelMessageSend(i.ChannelID, fmt.Sprintf("❌ バックアップ失敗: %v", err))
				return
			}
			_, _ = s.ChannelMessageSend(i.ChannelID, fmt.Sprintf("✅ **手動バックアップ完了**\n最新ワールドデータ (`world-data-%s.tar.gz`) を GCS (`gs://%s-minecraft-backup`) へ保存しました。", worldID, GCPProjectID))
		}()
	}
}

func startWebhookServer() {
	http.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024*1024))
		if err != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		if WebhookSecret != "" {
			signature := r.Header.Get("X-Hub-Signature-256")
			if !verifyHMACSignature(body, signature, WebhookSecret) {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}

		go func() {
			log.Println("【Webhook 受信】設定リポジトリの更新を検知しました。Hot Reload を実行します")
			_, _ = applyGitOpsChanges()
		}()

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	port := os.Getenv("WEBHOOK_PORT")
	if port == "" {
		port = "80"
	}

	log.Printf("Webhook サーバーを :%s で起動しました", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Printf("Webhook サーバーエラー: %v", err)
	}
}

func verifyHMACSignature(body []byte, signature, secret string) bool {
	if !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	expectedMAC := hmac.New(sha256.New, []byte(secret))
	expectedMAC.Write(body)
	expectedSig := "sha256=" + hex.EncodeToString(expectedMAC.Sum(nil))
	return hmac.Equal([]byte(signature), []byte(expectedSig))
}

func applyGitOpsChanges() (*ResolvedWorldConfig, error) {
	gitOpsMu.Lock()
	defer gitOpsMu.Unlock()

	repoDir := ConfigRepoDir

	if out, err := exec.Command("git", "-C", repoDir, "pull", "origin", "main").CombinedOutput(); err != nil {
		log.Printf("git pull 警告: %v (output: %s)", err, strings.TrimSpace(string(out)))
	}

	yamlBytes, err := os.ReadFile(repoDir + "/worlds.yaml")
	if err != nil {
		log.Printf("worlds.yaml 読み取り失敗: %v", err)
		notifyGitOpsError(fmt.Sprintf("`worlds.yaml` の読み取りに失敗しました: %v", err))
		return nil, err
	}

	resolvedWorld, err := resolveWorldConfig(yamlBytes)
	if err != nil {
		log.Printf("【GitOps 検証エラー】%v", err)
		notifyGitOpsError(fmt.Sprintf("`worlds.yaml` の事前検証に失敗したため、設定の反映をスキップしました。\n**エラー内容**: %v", err))
		return nil, err
	}

	if !isGCEInstanceRunning() {
		log.Printf("GCE が停止中のため、ゲーム内ルールのインジェクションをスキップしました (設定ワールド: %s)", resolvedWorld.ID)
		notifyGitOpsInfo(fmt.Sprintf("サーバー停止中のため、`worlds.yaml` の変更（有効ワールド: %s）は次回起動時に反映されます。現在のワールドと異なる場合は起動後に切り替えが行われます。", resolvedWorld.DisplayLabel()))
		return resolvedWorld, nil
	}

	previousWorldID, err := switchWorldIfNeeded(resolvedWorld)
	if err != nil {
		log.Printf("【GitOps ワールド切り替えエラー】%v", err)
		notifyGitOpsError(fmt.Sprintf("ワールド %s への切り替えに失敗しました。\n**エラー内容**: %v", resolvedWorld.DisplayLabel(), err))
		return resolvedWorld, err
	}
	if previousWorldID != "" {
		notifyGitOpsInfo(fmt.Sprintf("ワールドを `%s` から %s へ切り替えます（旧ワールドのバックアップ済み）。", previousWorldID, resolvedWorld.DisplayLabel()))
	}

	// Bedrock drops console commands sent before "Server started.", and itzg resets difficulty to "easy" in server.properties on every container recreation.
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer readyCancel()
	if err := waitForBedrockServerReady(readyCtx, 3*time.Second, resolvedWorld.ID); err != nil {
		log.Printf("【GitOps サーバー準備待機エラー】%v", err)
		notifyGitOpsError(fmt.Sprintf("ワールド %s の起動完了待機中にエラーが発生しました。\n**エラー内容**: %v", resolvedWorld.DisplayLabel(), err))
		return resolvedWorld, err
	}

	batchScript := buildBatchSendCommandScript(resolvedWorld.Commands)
	if _, err := executeRemoteCommandWithTimeout(batchScript, 60*time.Second); err != nil {
		log.Printf("【Hot Reload 失敗】コマンド一括送信エラー: %v", err)
		notifyGitOpsError(fmt.Sprintf("ワールド %s へのゲーム内ルール適用に失敗しました。\n**エラー内容**: %v", resolvedWorld.DisplayLabel(), err))
		return resolvedWorld, err
	}

	log.Printf("【Hot Reload 完了】ワールド %s (%s) へ Gamerules を適用しました", resolvedWorld.Name, resolvedWorld.ID)
	notifyGitOpsSuccess(resolvedWorld)
	return resolvedWorld, nil
}

func notifyGitOpsError(reason string) {
	if globalDiscordSession != nil && NotificationChannel != "" {
		embed := &discordgo.MessageEmbed{
			Title:       "⚠️ GitOps 設定検証・適用エラー",
			Description: reason,
			Color:       0xE74C3C,
			Timestamp:   time.Now().Format(time.RFC3339),
		}
		_, _ = globalDiscordSession.ChannelMessageSendEmbed(NotificationChannel, embed)
	}
}

func notifyGitOpsInfo(message string) {
	if globalDiscordSession != nil && NotificationChannel != "" {
		_, _ = globalDiscordSession.ChannelMessageSend(NotificationChannel, "🔄 "+message)
	}
}

func notifyGitOpsSuccess(world *ResolvedWorldConfig) {
	if globalDiscordSession != nil && NotificationChannel != "" {
		worldLabel := "kiseki"
		if world != nil {
			worldLabel = world.DisplayLabel()
		}
		embed := &discordgo.MessageEmbed{
			Title:       "✅ GitOps 設定適用完了",
			Description: fmt.Sprintf("ワールド %s のゲーム内設定を稼働中サーバーへ適用しました。", worldLabel),
			Color:       0x2ECC71,
			Timestamp:   time.Now().Format(time.RFC3339),
		}
		_, _ = globalDiscordSession.ChannelMessageSendEmbed(NotificationChannel, embed)
	}
}
