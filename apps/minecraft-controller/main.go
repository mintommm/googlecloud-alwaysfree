package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorcon/rcon"
	"google.golang.org/api/compute/v1"
)

var (
	Token                 = os.Getenv("DISCORD_TOKEN")
	GuildID               = os.Getenv("DISCORD_GUILD_ID")
	ProjectID             = "mintommm-alwaysfree-gce"
	Zone                  = os.Getenv("MC_ZONE")
	InstanceName          = os.Getenv("MC_INSTANCE_NAME")
	CloudflareToken       = os.Getenv("CF_API_TOKEN")
	CloudflareZoneID      = os.Getenv("CF_ZONE_ID")
	CloudflareRecord      = os.Getenv("CF_RECORD_NAME")
	RconPassword          = os.Getenv("RCON_PASSWORD")
	NotificationChannelID = os.Getenv("DISCORD_NOTIFICATION_CHANNEL_ID")

	// オンラインプレイヤーのキャッシュ管理用
	currentPlayers = make(map[string]bool)
	playerMutex    sync.Mutex
)

type DNSRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

type CloudflareResponse struct {
	Success bool        `json:"success"`
	Result  []DNSRecord `json:"result"`
}

func main() {
	if Token == "" || Zone == "" || InstanceName == "" || RconPassword == "" {
		log.Fatal("必須の環境変数が設定されていません。")
	}

	dg, err := discordgo.New("Bot " + Token)
	if err != nil {
		log.Fatalf("Discordセッションの作成に失敗しました: %v", err)
	}

	dg.AddHandler(interactionHandler)

	err = dg.Open()
	if err != nil {
		log.Fatalf("Discordへの接続に失敗しました: %v", err)
	}
	defer dg.Close()

	// スラッシュコマンドの定義
	commands := []*discordgo.ApplicationCommand{
		{
			Name:        "panel",
			Description: "マインクラフトサーバーの管理パネルを表示します",
		},
		{
			Name:                     "cmd",
			Description:              "マインクラフトサーバーにコンソールコマンドを送信します（管理者限定）",
			DefaultMemberPermissions: int64Ptr(discordgo.PermissionAdministrator),
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "command",
					Description: "実行するコマンド文字列",
					Required:    true,
				},
			},
		},
	}

	log.Println("コマンドを登録中...")
	_, err = dg.ApplicationCommandBulkOverwrite(dg.State.User.ID, GuildID, commands)
	if err != nil {
		log.Fatalf("コマンドの登録に失敗しました: %v", err)
	}

	// バックグラウンドでプレイヤー監視ゴルーチンを起動
	go monitorPlayersLoop(dg)

	log.Println("Botが起動しました。Ctrl+Cで終了します。")
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
}

func int64Ptr(v int64) *int64 {
	return &v
}

func interactionHandler(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		handleSlashCommand(s, i)
	case discordgo.InteractionMessageComponent:
		handleButtonClick(s, i)
	}
}

func handleSlashCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	cmdData := i.ApplicationCommandData()
	switch cmdData.Name {
	case "panel":
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content:    "🟢 **マインクラフトサーバー管理パネル**\n下のボタンをタップして操作してください。",
				Components: createPanelComponents(),
			},
		})
	case "cmd":
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		})
		cmdText := cmdData.Options[0].StringValue()

		ip, err := getGCEExternalIP()
		if err != nil {
			sendFollowupMessage(s, i.Interaction, fmt.Sprintf("❌ サーバーの外部IP取得に失敗しました（サーバーが停止している可能性があります）: %v", err))
			return
		}

		resp, err := executeRCONCommand(ip, cmdText)
		if err != nil {
			sendFollowupMessage(s, i.Interaction, fmt.Sprintf("❌ RCONコマンドの実行に失敗しました: %v", err))
			return
		}

		sendFollowupMessage(s, i.Interaction, fmt.Sprintf("💻 **コンソール出力結果:**\n```text\n%s\n```", resp))
	}
}

func handleButtonClick(s *discordgo.Session, i *discordgo.InteractionCreate) {
	customID := i.MessageComponentData().CustomID
	channelID := i.ChannelID

	switch customID {
	case "btn_start":
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		})

		go func() {
			startTime := time.Now()

			progressText := "🔄 **サーバー起動プロセスを開始しました**\n" +
				"⏳ [待機中] GCEインスタンスの起動リクエスト\n" +
				"⏳ [待機中] RUNNING状態の遷移確認\n" +
				"⏳ [待機中] Cloudflare DNSレコードの更新\n" +
				"⏳ [待機中] マイクラプロセスの起動確認（接続可能判定）"
			updateProgress(s, i.Interaction, progressText)

			// 1. GCEインスタンスの起動
			if err := startGCEInstance(); err != nil {
				updateProgress(s, i.Interaction, fmt.Sprintf("❌ サーバーの起動リクエストに失敗しました: %v", err))
				return
			}
			progressText = "🔄 **サーバー起動プロセスを実行中**\n" +
				"✅ [完了] GCEインスタンスの起動リクエスト\n" +
				"🔄 [処理中] RUNNING状態の遷移確認\n" +
				"⏳ [待機中] Cloudflare DNSレコードの更新\n" +
				"⏳ [待機中] マイクラプロセスの起動確認（接続可能判定）"
			updateProgress(s, i.Interaction, progressText)

			// 2. RUNNING状態へのポーリング
			if err := waitForInstanceStatus("RUNNING"); err != nil {
				updateProgress(s, i.Interaction, fmt.Sprintf("❌ サーバーの起動確認に失敗しました: %v", err))
				return
			}
			progressText = "🔄 **サーバー起動プロセスを実行中**\n" +
				"✅ [完了] GCEインスタンスの起動リクエスト\n" +
				"✅ [完了] RUNNING状態の遷移確認\n" +
				"🔄 [処理中] Cloudflare DNSレコードの更新\n" +
				"⏳ [待機中] マイクラプロセスの起動確認（接続可能判定）"
			updateProgress(s, i.Interaction, progressText)

			// 3. 外部IP取得およびCloudflare DNS更新
			ip, err := getGCEExternalIP()
			if err != nil {
				updateProgress(s, i.Interaction, fmt.Sprintf("❌ 外部IPの取得に失敗しました: %v", err))
				return
			}
			if err := updateCloudflareDNS(ip); err != nil {
				updateProgress(s, i.Interaction, fmt.Sprintf("❌ DNSの更新に失敗しました: %v", err))
				return
			}
			progressText = "🔄 **サーバー起動プロセスを実行中**\n" +
				"✅ [完了] GCEインスタンスの起動リクエスト\n" +
				"✅ [完了] RUNNING状態の遷移確認\n" +
				"✅ [完了] Cloudflare DNSレコードの更新\n" +
				"🔄 [処理中] マイクラプロセスの起動確認（接続可能判定）"
			updateProgress(s, i.Interaction, progressText)

			// 4. マイクラゲームプロセスの起動確認（RCON疎通確認）
			if err := verifyMinecraftOnline(ip); err != nil {
				updateProgress(s, i.Interaction, fmt.Sprintf("❌ マイクラプロセスの起動確認に失敗しました: %v", err))
				return
			}

			elapsed := time.Since(startTime).Seconds()
			finalProgress := fmt.Sprintf(
				"🚀 **サーバーを起動しました**\n"+
					"✅ [完了] GCEインスタンスの起動リクエスト\n"+
					"✅ [完了] RUNNING状態の遷移確認\n"+
					"✅ [完了] Cloudflare DNSレコードの更新\n"+
					"✅ [完了] マイクラプロセスの起動確認（接続可能判定）\n\n"+
					"🌐 **ドメイン:** `%s`\n⏱️ **総起動時間:** %.1f 秒",
				CloudflareRecord, elapsed,
			)
			updateProgress(s, i.Interaction, finalProgress)

			// パネルの自動再表示
			repostPanel(s, channelID)
		}()

	case "btn_stop", "btn_stop_forced":
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		})

		go func() {
			ip, err := getGCEExternalIP()
			// インスタンスが起動していない場合はチェックをスキップして停止シーケンスへ
			if err == nil && customID != "btn_stop_forced" {
				players, err := fetchOnlinePlayers(ip)
				if err == nil && len(players) > 0 {
					// 滞在プレイヤーが存在する場合の警告分岐
					playerList := strings.Join(players, ", ")
					s.FollowupMessageCreate(i.Interaction, false, &discordgo.WebhookParams{
						Content: fmt.Sprintf("⚠️ **警告: プレイヤーがまだサーバーに滞在しています。**\n現在のオンライン: `%s`\n本当に停止する場合は、下の「強制停止」ボタンを押してください。", playerList),
						Components: []discordgo.MessageComponent{
							discordgo.ActionsRow{
								Components: []discordgo.MessageComponent{
									discordgo.Button{
										Label:    "強制停止",
										Style:    discordgo.DangerButton,
										CustomID: "btn_stop_forced",
										Emoji: &discordgo.ComponentEmoji{
											Name: "⚠️",
										},
									},
								},
							},
						},
					})
					return
				}
			}

			// 通常または強制停止シーケンスの実行
			progressText := "🔄 **サーバー停止プロセスを開始しました**\n" +
				"⏳ [待機中] GCEインスタンスの停止リクエスト\n" +
				"⏳ [待機中] TERMINATED状態の遷移確認"
			updateProgress(s, i.Interaction, progressText)

			if err := stopGCEInstance(); err != nil {
				updateProgress(s, i.Interaction, fmt.Sprintf("❌ サーバーの停止リクエストに失敗しました: %v", err))
				return
			}
			progressText = "🔄 **サーバー停止プロセスを実行中**\n" +
				"✅ [完了] GCEインスタンスの停止リクエスト\n" +
				"🔄 [処理中] TERMINATED状態の遷移確認"
			updateProgress(s, i.Interaction, progressText)

			if err := waitForInstanceStatus("TERMINATED"); err != nil {
				updateProgress(s, i.Interaction, fmt.Sprintf("❌ サーバーの停止確認に失敗しました: %v", err))
				return
			}

			// キャッシュをクリーンアップ
			playerMutex.Lock()
			currentPlayers = make(map[string]bool)
			playerMutex.Unlock()

			updateProgress(s, i.Interaction, "🛑 **サーバーを正常に停止しました。**\n✅ [完了] GCEインスタンスの停止リクエスト\n✅ [完了] TERMINATED状態の遷移確認")

			// パネルの自動再表示
			repostPanel(s, channelID)
		}()
	}
}

func createPanelComponents() []discordgo.MessageComponent {
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					Label:    "起動",
					Style:    discordgo.SuccessButton,
					CustomID: "btn_start",
					Emoji: &discordgo.ComponentEmoji{
						Name: "🚀",
					},
				},
				discordgo.Button{
					Label:    "停止",
					Style:    discordgo.DangerButton,
					CustomID: "btn_stop",
					Emoji: &discordgo.ComponentEmoji{
						Name: "🛑",
					},
				},
			},
		},
	}
}

func updateProgress(s *discordgo.Session, i *discordgo.Interaction, content string) {
	_, err := s.InteractionResponseEdit(i, &discordgo.WebhookEdit{
		Content: &content,
	})
	if err != nil {
		log.Printf("進捗メッセージの更新に失敗しました: %v", err)
	}
}

func repostPanel(s *discordgo.Session, channelID string) {
	_, err := s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content:    "🟢 **マインクラフトサーバー管理パネル**\n下のボタンをタップして操作してください。",
		Components: createPanelComponents(),
	})
	if err != nil {
		log.Printf("管理パネルの再表示に失敗しました: %v", err)
	}
}

func startGCEInstance() error {
	ctx := context.Background()
	computeService, err := compute.NewService(ctx)
	if err != nil {
		return err
	}
	_, err = computeService.Instances.Start(ProjectID, Zone, InstanceName).Context(ctx).Do()
	return err
}

func stopGCEInstance() error {
	ctx := context.Background()
	computeService, err := compute.NewService(ctx)
	if err != nil {
		return err
	}
	_, err = computeService.Instances.Stop(ProjectID, Zone, InstanceName).Context(ctx).Do()
	return err
}

func waitForInstanceStatus(targetStatus string) error {
	ctx := context.Background()
	computeService, err := compute.NewService(ctx)
	if err != nil {
		return err
	}

	for i := 0; i < 30; i++ {
		instance, err := computeService.Instances.Get(ProjectID, Zone, InstanceName).Context(ctx).Do()
		if err != nil {
			return err
		}
		if instance.Status == targetStatus {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("ステータスが %s に遷移するまでタイムアウトしました", targetStatus)
}

func getGCEExternalIP() (string, error) {
	ctx := context.Background()
	computeService, err := compute.NewService(ctx)
	if err != nil {
		return "", err
	}
	instance, err := computeService.Instances.Get(ProjectID, Zone, InstanceName).Context(ctx).Do()
	if err != nil {
		return "", err
	}
	if len(instance.NetworkInterfaces) > 0 && len(instance.NetworkInterfaces[0].AccessConfigs) > 0 {
		return instance.NetworkInterfaces[0].AccessConfigs[0].NatIP, nil
	}
	return "", fmt.Errorf("外部IPアドレスが検出されませんでした")
}

func verifyMinecraftOnline(ip string) error {
	// 最大2分間、RCONポートの疎通を確認
	for i := 0; i < 24; i++ {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, "25575"), 3*time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("マインクラフトのプロセスの応答がタイムアウトしました")
}

func updateCloudflareDNS(ip string) error {
	client := &http.Client{Timeout: 10 * time.Second}

	reqURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?name=%s", CloudflareZoneID, CloudflareRecord)
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+CloudflareToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var cfResp CloudflareResponse
	if err := json.NewDecoder(resp.Body).Decode(&cfResp); err != nil {
		return err
	}
	if !cfResp.Success || len(cfResp.Result) == 0 {
		return fmt.Errorf("cloudflare上に該当のDNSレコードがありません")
	}
	recordID := cfResp.Result[0].ID

	updateURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", CloudflareZoneID, recordID)
	payload, err := json.Marshal(DNSRecord{
		Type:    "A",
		Name:    CloudflareRecord,
		Content: ip,
		TTL:     1,
		Proxied: false,
	})
	if err != nil {
		return err
	}

	req, err = http.NewRequest("PATCH", updateURL, bytes.NewBuffer(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+CloudflareToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err = client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var cfUpdateResp struct {
		Success bool `json:"success"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfUpdateResp); err != nil {
		return err
	}
	if !cfUpdateResp.Success {
		return fmt.Errorf("cloudflareのAレコード更新に失敗しました")
	}

	return nil
}

func executeRCONCommand(ip, command string) (string, error) {
	client, err := rcon.Dial(net.JoinHostPort(ip, "25575"), RconPassword)
	if err != nil {
		return "", err
	}
	defer client.Close()

	return client.Execute(command)
}

func fetchOnlinePlayers(ip string) ([]string, error) {
	resp, err := executeRCONCommand(ip, "list")
	if err != nil {
		return nil, err
	}
	return parseBedrockPlayers(resp), nil
}

func parseBedrockPlayers(response string) []string {
	// 統合版の典型的な応答: "There are 2/20 players online:\nMockPencil3834, superkurute"
	// またはプレイヤーゼロ時: "There are 0 players online:"
	lines := strings.Split(response, "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[1]) == "" {
		return []string{}
	}

	rawPlayers := strings.Split(lines[1], ",")
	var players []string
	for _, p := range rawPlayers {
		name := strings.TrimSpace(p)
		if name != "" {
			players = append(players, name)
		}
	}
	return players
}

func monitorPlayersLoop(dg *discordgo.Session) {
	ticker := time.NewTicker(15 * time.Second)
	for range ticker.C {
		ip, err := getGCEExternalIP()
		if err != nil {
			// インスタンスが停止状態の場合はスキップ
			continue
		}

		players, err := fetchOnlinePlayers(ip)
		if err != nil {
			// RCON接続不可（コンテナ起動中など）はスキップ
			continue
		}

		targetChannel := NotificationChannelID
		if targetChannel == "" {
			continue // 通知先チャンネルIDが環境変数に無ければ通知処理自体をパス
		}

		playerMutex.Lock()
		fetchedMap := make(map[string]bool)
		for _, p := range players {
			fetchedMap[p] = true
			if !currentPlayers[p] {
				// 前回のキャッシュに存在しないプレイヤー＝入室
				dg.ChannelMessageSend(targetChannel, fmt.Sprintf("📥 **[入室]** `%s` がサーバーに参加しました。", p))
			}
		}

		for p := range currentPlayers {
			if !fetchedMap[p] {
				// 今回のフェッチに存在しないプレイヤー＝退出
				dg.ChannelMessageSend(targetChannel, fmt.Sprintf("📤 **[退出]** `%s` がサーバーから退出しました。", p))
			}
		}

		currentPlayers = fetchedMap
		playerMutex.Unlock()
	}
}

func sendFollowupMessage(s *discordgo.Session, i *discordgo.Interaction, content string) {
	_, err := s.FollowupMessageCreate(i, false, &discordgo.WebhookParams{
		Content: content,
	})
	if err != nil {
		log.Printf("フォローアップメッセージの送信に失敗しました: %v", err)
	}
}
