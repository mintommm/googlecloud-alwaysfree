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
	CurrentPlayers  = 0
	PlayersMutex    sync.Mutex
	isTimerActive   = false
	emptyStartTime  time.Time
	streamCancel    context.CancelFunc
	streamMu        sync.Mutex
	tickerCancel    context.CancelFunc
	tickerMu        sync.Mutex
	jstLocation     *time.Location
)

var (
	regexPlayerJoin   = regexp.MustCompile(`Player connected:\s+([^,]+)`)
	regexPlayerLeft   = regexp.MustCompile(`Player disconnected:\s+([^,]+)`)
	regexListCount    = regexp.MustCompile(`There are (\d+)/\d+ players online`)
	regexDockerPrefix = regexp.MustCompile(`^(?:[0-9T:.\-+]+Z?\s+)?(?:stdout|stderr):\s*(.*)$`)
)

type WorldConfig struct {
	Defaults struct {
		Settings map[string]interface{} `yaml:"settings"`
	} `yaml:"defaults"`
	Worlds []struct {
		ID       string                 `yaml:"id"`
		Name     string                 `yaml:"name"`
		Emoji    string                 `yaml:"emoji"`
		Settings map[string]interface{} `yaml:"settings"`
	} `yaml:"worlds"`
}

// reconcileWorldConfig は defaults.settings を基準にアクティブワールドの設定を事前検証・マージし、
// Minecraft コマンドリストを生成します。
func reconcileWorldConfig(yamlBytes []byte, activeWorldID string) ([]string, error) {
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

	var targetWorld *struct {
		ID       string                 `yaml:"id"`
		Name     string                 `yaml:"name"`
		Emoji    string                 `yaml:"emoji"`
		Settings map[string]interface{} `yaml:"settings"`
	}

	for i := range config.Worlds {
		if config.Worlds[i].ID == activeWorldID {
			targetWorld = &config.Worlds[i]
			break
		}
	}

	if targetWorld == nil {
		targetWorld = &config.Worlds[0]
	}

	// 1. targetWorld の settings にあるキーが defaults.settings に存在するか検証
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

	// 2. defaults.settings をベースに targetWorld.Settings で上書きマージ
	mergedSettings := make(map[string]interface{})
	for k, v := range config.Defaults.Settings {
		mergedSettings[k] = v
	}
	for k, v := range targetWorld.Settings {
		if k != "seed" {
			mergedSettings[k] = v
		}
	}

	// 3. Minecraft コマンドの生成 (安定した実行順序のためソート)
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

	return cmds, nil
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

	// 1. 起動時自己修復 DDNS (alwaysfree.krmtn.org Proxied: true)
	go updateSelfDDNS()

	// 2. Webhook サーバー起動 (:8080)
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

	registeredCommands, err := dg.ApplicationCommandBulkOverwrite(dg.State.User.ID, GuildID, commands)
	if err != nil {
		log.Fatalf("Could not register application commands: %v", err)
	}

	log.Println("Bot is ready. Managing Minecraft server and lifecycles...")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc

	stopBackupTicker()
	stopLogStream()
	for _, cmd := range registeredCommands {
		_ = dg.ApplicationCommandDelete(dg.State.User.ID, GuildID, cmd.ID)
	}
}

// updateSelfDDNS は Always Free インスタンスの自己修復 DDNS を実行します (alwaysfree.krmtn.org Proxied: true)
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

// updateMinecraftDNS は minecraft01 の起動時に minecraft.krmtn.org (Proxied: false) を更新します
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

// waitForDNSPropagation は公開 DNS (1.1.1.1 / 8.8.8.8) で新 IP が解決できるまで待機します
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

func getMinecraftInstanceIP() (string, error) {
	cmd := exec.Command("gcloud", "compute", "instances", "describe", InstanceName,
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
				syncOnlinePlayersDirect()
				log.Println("【ストリーム開始】ログ監視ストリームを確立します")
				err := startLogStreamProcess(ctx, dg)
				if err != nil {
					log.Printf("ストリーム切断: %v。5秒後に再接続します", err)
				}
			}
			time.Sleep(5 * time.Second)
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

func startLogStreamProcess(ctx context.Context, dg *discordgo.Session) error {
	cmd := exec.CommandContext(ctx, "gcloud", "compute", "ssh", InstanceName,
		"--zone="+Zone,
		"--tunnel-through-iap",
		"--quiet",
		"--ssh-flag=-o ServerAliveInterval=15",
		"--ssh-flag=-o ServerAliveCountMax=3",
		"--command=docker logs -f --tail=20 minecraft-bedrock",
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		handleLogLineEvents(dg, line)
	}
	return cmd.Wait()
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

		if CurrentPlayers == 0 && !isTimerActive {
			isTimerActive = true
			emptyStartTime = time.Now()
			_, _ = dg.ChannelMessageSend(NotificationChannel, "プレイヤー数が0人になりました。10分後に自動停止します。")

			go func(startTime time.Time) {
				time.Sleep(10 * time.Minute)
				PlayersMutex.Lock()
				if isTimerActive && emptyStartTime.Equal(startTime) {
					_, _ = dg.ChannelMessageSend(NotificationChannel, "プレイヤー0人の状態が10分継続したため、自動シャットダウンを実行します。")
					executeOfflineBackupSequence(dg)
					isTimerActive = false
				}
				PlayersMutex.Unlock()
			}(emptyStartTime)
		}
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
				if err := executeOnlineBackupFlow(); err != nil {
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

func executeOnlineBackupFlow() error {
	// 1. save hold
	_, err := executeRemoteCommandWithTimeout("docker exec minecraft-bedrock send-command \"save hold\"", 60*time.Second)
	if err != nil {
		return fmt.Errorf("failed to send save hold: %w", err)
	}

	time.Sleep(3 * time.Second)

	// 2. GCS バックアップスクリプト実行 (XZ 高速並列圧縮 + GCS アップロード)
	backupScript := fmt.Sprintf(`docker run --rm -i \
		-v minecraft-data:/data:ro \
		google/cloud-sdk:alpine sh -s << 'EOF'
set -e
cd /data/worlds
TARGET_DIR="kiseki"
if [ ! -d "kiseki" ] && [ -d "Kiseki" ]; then
    TARGET_DIR="Kiseki"
fi
tar -cf - "$TARGET_DIR" | xz -1 -T0 -c > /tmp/world-data-kiseki.tar.xz
gcloud storage cp /tmp/world-data-kiseki.tar.xz gs://%s-minecraft-backup/world-data-kiseki.tar.xz --quiet
rm -f /tmp/world-data-kiseki.tar.xz
EOF`, GCPProjectID)

	_, err = executeRemoteCommandWithTimeout(backupScript, 5*time.Minute)

	// 3. save resume
	_, _ = executeRemoteCommandWithTimeout("docker exec minecraft-bedrock send-command \"save resume\"", 60*time.Second)

	if err != nil {
		return fmt.Errorf("GCS バックアップ失敗: %w", err)
	}

	log.Println("【バックアップ完了】gs://" + GCPProjectID + "-minecraft-backup/world-data-kiseki.tar.xz へ安全に退避しました")
	return nil
}

func executeOfflineBackupSequence(dg *discordgo.Session) {
	_, _ = executeRemoteCommandWithTimeout("docker stop -t 10 minecraft-bedrock", 60*time.Second)
	stopLogStream()

	// 停止時最終バックアップ (XZ 高速並列圧縮)
	backupScript := fmt.Sprintf(`docker run --rm -i \
		-v minecraft-data:/data:ro \
		google/cloud-sdk:alpine sh -s << 'EOF'
set -e
cd /data/worlds
TARGET_DIR="kiseki"
if [ ! -d "kiseki" ] && [ -d "Kiseki" ]; then
    TARGET_DIR="Kiseki"
fi
tar -cf - "$TARGET_DIR" | xz -1 -T0 -c > /tmp/world-data-kiseki.tar.xz
gcloud storage cp /tmp/world-data-kiseki.tar.xz gs://%s-minecraft-backup/world-data-kiseki.tar.xz --quiet
rm -f /tmp/world-data-kiseki.tar.xz
EOF`, GCPProjectID)
	_, _ = executeRemoteCommandWithTimeout(backupScript, 5*time.Minute)

	_ = exec.Command("gcloud", "compute", "instances", "stop", InstanceName, "--zone="+Zone, "--quiet").Run()
	if NotificationChannel != "" {
		_, _ = dg.ChannelMessageSend(NotificationChannel, "マインクラフトサーバーは正常に停止し、インスタンスは停止状態になりました。")
	}
}

func executeRemoteCommandGetStdout(commandLine string) (string, error) {
	return executeRemoteCommandWithTimeout(commandLine, 60*time.Second)
}

func executeRemoteCommandWithTimeout(commandLine string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gcloud", "compute", "ssh", InstanceName,
		"--zone="+Zone,
		"--tunnel-through-iap",
		"--quiet",
		"--command="+commandLine,
	)

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
	cmd := exec.Command("gcloud", "compute", "instances", "describe", InstanceName,
		"--zone="+Zone, "--format=get(status)")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "RUNNING"
}

func syncOnlinePlayersDirect() {
	_, err := executeRemoteCommandGetStdout("docker exec minecraft-bedrock send-command list")
	if err != nil {
		return
	}
	time.Sleep(500 * time.Millisecond)
	logOut, err := executeRemoteCommandGetStdout("docker logs --tail=5 minecraft-bedrock")
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
			}
			PlayersMutex.Unlock()
			log.Printf("【同期完了】インメモリオンラインプレイヤー数を実態（%d人）に補正しました", count)
			return
		}
	}
}

// extractCleanLogMessage は Payload から純粋なログメッセージを安全に抽出します
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

// handleCloudLogsCommand は Cloud Logging から透過的にログを取得して返信します
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

		// 新しい順から古い順に反転
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
					Color:       0x3498db, // 青色
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
			Data: &discordgo.InteractionResponseData{Content: "🚀 サーバー起動要求を送信しました。DNS の更新と反映確認を行っています..."},
		})
		go func() {
			cmdStart := exec.Command("gcloud", "compute", "instances", "start", InstanceName, "--zone="+Zone, "--quiet")
			if err := cmdStart.Run(); err != nil {
				_, _ = s.ChannelMessageSend(i.ChannelID, fmt.Sprintf("❌ GCE 起動失敗: %v", err))
				return
			}

			ip, err := getMinecraftInstanceIP()
			if err != nil {
				_, _ = s.ChannelMessageSend(i.ChannelID, fmt.Sprintf("⚠️ インスタンス IP 取得失敗: %v", err))
				return
			}

			// Cloudflare DNS 更新
			if err := updateMinecraftDNS(ip); err != nil {
				log.Printf("minecraft.krmtn.org 更新エラー: %v", err)
			}

			// 公開 DNS での反映確認
			propagated := waitForDNSPropagation(ip, 30*time.Second)
			statusNote := "DNS 反映完了 ✅"
			if !propagated {
				statusNote = "DNS 伝播中 (数分かかる場合があります) ⏳"
			}

			msg := fmt.Sprintf("%s 🟢 **マインクラフトサーバーの起動が完了しました！**\nサーバーアドレス: `minecraft.krmtn.org`\nIPアドレス: `%s` (%s)", callerMention, ip, statusNote)
			_, _ = s.ChannelMessageSend(i.ChannelID, msg)
			go applyGitOpsChanges()
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
			if !isGCEInstanceRunning() {
				_, _ = s.ChannelMessageSend(i.ChannelID, "🔴 GCEインスタンス状態: **TERMINATED** (停止中)")
				return
			}
			PlayersMutex.Lock()
			count := CurrentPlayers
			PlayersMutex.Unlock()
			_, _ = s.ChannelMessageSend(i.ChannelID, fmt.Sprintf("🟢 GCEインスタンス状態: **RUNNING** (稼働中)\n👥 オンラインプレイヤー数: **%d人**", count))
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
			remoteCommand := fmt.Sprintf("docker exec minecraft-bedrock send-command \"%s\"", cleanCmd)
			_, err := executeRemoteCommandGetStdout(remoteCommand)
			if err != nil {
				_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Embeds: []*discordgo.MessageEmbed{
							{
								Title:       "💻 コマンド送信失敗",
								Description: fmt.Sprintf("実行者: %s\nコマンド: `%s`\nステータス: ❌ 送信失敗 (%v)", callerMention, cleanCmd, err),
								Color:       0xe74c3c, // 赤色
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
							Color:       0x3498db, // 青色
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
			Data: &discordgo.InteractionResponseData{Content: "💾 即時バックアップを開始します..."},
		})
		go func() {
			if !isGCEInstanceRunning() {
				_, _ = s.ChannelMessageSend(i.ChannelID, "❌ サーバーが停止しているためバックアップを実行できません。")
				return
			}
			err := executeOnlineBackupFlow()
			if err != nil {
				_, _ = s.ChannelMessageSend(i.ChannelID, fmt.Sprintf("❌ バックアップ失敗: %v", err))
				return
			}
			_, _ = s.ChannelMessageSend(i.ChannelID, fmt.Sprintf("✅ **手動バックアップ完了**\n最新ワールドデータ (`world-data-kiseki.tar.xz`) を GCS (`gs://%s-minecraft-backup`) へ保存しました。", GCPProjectID))
		}()
	}
}

// startWebhookServer は GitHub からの Webhook をリッスンし、設定変更を即時反映します (:8080/webhook)
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
			applyGitOpsChanges()
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

func applyGitOpsChanges() {
	// 1. ローカル設定リポジトリで git pull
	repoDir := "/opt/minecraft-controller/config-repo"
	_ = exec.Command("git", "-C", repoDir, "pull", "origin", "main").Run()

	// 2. worlds.yaml のパースと事前検証
	yamlBytes, err := os.ReadFile(repoDir + "/worlds.yaml")
	if err != nil {
		log.Printf("worlds.yaml 読み取り失敗: %v", err)
		notifyGitOpsError(fmt.Sprintf("`worlds.yaml` の読み取りに失敗しました: %v", err))
		return
	}

	activeWorld := os.Getenv("MC_LEVEL_NAME")
	if activeWorld == "" {
		activeWorld = "kiseki"
	}

	cmds, err := reconcileWorldConfig(yamlBytes, activeWorld)
	if err != nil {
		log.Printf("【GitOps 検証エラー】%v", err)
		notifyGitOpsError(fmt.Sprintf("`worlds.yaml` の事前検証に失敗したため、設定の反映をスキップしました。\n**エラー内容**: %v", err))
		return
	}

	if !isGCEInstanceRunning() {
		log.Println("GCE が停止中のため、ゲーム内ルールのインジェクションをスキップしました")
		return
	}

	// 3. 稼働中サーバーへのルール適用 (Hot Reload)
	for _, cmdStr := range cmds {
		_, _ = executeRemoteCommandGetStdout(fmt.Sprintf("docker exec minecraft-bedrock send-command \"%s\"", cmdStr))
	}
	log.Println("【Hot Reload 完了】Gamerules を稼働中のマインクラフトサーバーへ即時適用しました")
	notifyGitOpsSuccess()
}

func notifyGitOpsError(reason string) {
	if globalDiscordSession != nil && NotificationChannel != "" {
		embed := &discordgo.MessageEmbed{
			Title:       "⚠️ GitOps 設定検証エラー",
			Description: reason,
			Color:       0xE74C3C, // 赤
			Timestamp:   time.Now().Format(time.RFC3339),
		}
		_, _ = globalDiscordSession.ChannelMessageSendEmbed(NotificationChannel, embed)
	}
}

func notifyGitOpsSuccess() {
	if globalDiscordSession != nil && NotificationChannel != "" {
		embed := &discordgo.MessageEmbed{
			Title:       "✅ GitOps 設定適用完了",
			Description: "`worlds.yaml` の更新を検知し、ゲーム内設定を稼働中サーバーへ即時適用しました。",
			Color:       0x2ECC71, // 緑
			Timestamp:   time.Now().Format(time.RFC3339),
		}
		_, _ = globalDiscordSession.ChannelMessageSendEmbed(NotificationChannel, embed)
	}
}
