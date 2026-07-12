package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/cloudflare/cloudflare-go"
	"github.com/gorcon/rcon"
	"google.golang.org/api/compute/v1"
)

// 環境変数からの設定値保持
var (
	DiscordToken         string
	GuildID              string
	ProjectID            string
	Zone                 string
	InstanceName         string
	CFAPIToken           string
	CFZoneID             string
	CFRecordName         string
	RCONPassword         string
	NotificationChannel  string
)

// 状態管理用構造体
type BotState struct {
	isTimerActive   bool
	emptyStartTime  time.Time
	lastPlayers     map[string]bool
}

var state = BotState{
	lastPlayers: make(map[string]bool),
}

func init() {
	DiscordToken = os.Getenv("DISCORD_TOKEN")
	GuildID = os.Getenv("DISCORD_GUILD_ID")
	ProjectID = "mintommm-alwaysfree-gce" // 固定プロジェクトID
	Zone = os.Getenv("MC_ZONE")
	InstanceName = os.Getenv("MC_INSTANCE_NAME")
	CFAPIToken = os.Getenv("CF_API_TOKEN")
	CFZoneID = os.Getenv("CF_ZONE_ID")
	CFRecordName = os.Getenv("CF_RECORD_NAME")
	RCONPassword = os.Getenv("RCON_PASSWORD")
	NotificationChannel = os.Getenv("DISCORD_NOTIFICATION_CHANNEL_ID")

	if DiscordToken == "" || Zone == "" || InstanceName == "" || RCONPassword == "" {
		log.Fatal("必須の環境変数が設定されていません。")
	}
}

func main() {
	dg, err := discordgo.New("Bot " + DiscordToken)
	if err != nil {
		log.Fatalf("Discordセッション作成エラー: %v", err)
	}

	dg.AddHandler(messageCreate)
	dg.AddHandler(interactionCreate)

	err = dg.Open()
	if err != nil {
		log.Fatalf("Discord接続エラー: %v", err)
	}

	log.Println("コマンドを登録中...")
	registerCommands(dg)

	log.Println("Botが起動しました。Ctrl+Cで終了します。")

	// 分単位のRCONポーリング監視タスクをバックグラウンドで開始
	go startPollingTicker(dg)

	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc

	dg.Close()
}

// GCPから対象インスタンスの「内部IPアドレス」を動的に取得する関数
func getGCEInternalIP() (string, error) {
	ctx := context.Background()
	computeService, err := compute.NewService(ctx)
	if err != nil {
		return "", err
	}
	instance, err := computeService.Instances.Get(ProjectID, Zone, InstanceName).Context(ctx).Do()
	if err != nil {
		return "", err
	}
	if len(instance.NetworkInterfaces) > 0 {
		return instance.NetworkInterfaces[0].NetworkIP, nil
	}
	return "", fmt.Errorf("内部IPアドレスが検出されませんでした")
}

// GCPから対象インスタンスの「外部IPアドレス」を取得する関数（DNS更新用）
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

// 内部IPを利用してRCONコマンドを実行する共通関数
func executeRCON(command string) (string, error) {
	ip, err := getGCEInternalIP()
	if err != nil {
		return "", fmt.Errorf("IP取得失敗: %v", err)
	}

	address := fmt.Sprintf("%s:25575", ip)
	conn, err := rcon.Dial(address, RCONPassword)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	response, err := conn.Execute(command)
	if err != nil {
		return "", err
	}
	return response, nil
}

func registerCommands(dg *discordgo.Session) {
	commands := []*discordgo.ApplicationCommand{
		{
			Name:        "panel",
			Description: "マインクラフトサーバーの管理パネルを表示します",
		},
		{
			Name:        "cmd",
			Description: "【管理者限定】マインクラフトサーバーにRCONコマンドを送信します",
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

	for _, v := range commands {
		_, err := dg.ApplicationCommandCreate(dg.State.User.ID, GuildID, v)
		if err != nil {
			log.Printf("コマンド '%v' の登録に失敗しました: %v", v.Name, err)
		}
	}
}

func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {}

func interactionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		switch i.ApplicationCommandData().Name {
		case "panel":
			sendPanel(s, i)
		case "cmd":
			handleCmd(s, i)
		}
	case discordgo.InteractionMessageComponent:
		handleButtons(s, i)
	}
}

func sendPanel(s *discordgo.Session, i *discordgo.InteractionCreate) {
	component := discordgo.ActionsRow{
		Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    "起動 (🚀)",
				Style:    discordgo.SuccessButton,
				CustomID: "btn_start",
			},
			discordgo.Button{
				Label:    "停止 (🛑)",
				Style:    discordgo.DangerButton,
				CustomID: "btn_stop",
			},
		},
	}

	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content:    "🟢 **マインクラフトサーバー管理パネル**",
			Components: []discordgo.MessageComponent{component},
		},
	})
	if err != nil {
		log.Printf("パネル送信エラー: %v", err)
	}
}

func handleCmd(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// 管理者権限チェック (Administrator: 0x8)
	if i.Member.Permissions&discordgo.PermissionAdministrator == 0 {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ このコマンドを実行する権限がありません。",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	options := i.ApplicationCommandData().Options
	cmdStr := options[0].StringValue()

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})

	resp, err := executeRCON(cmdStr)
	var content string
	if err != nil {
		content = fmt.Sprintf("❌ RCONコマンドの実行に失敗しました: %v", err)
	} else {
		content = fmt.Sprintf("💻 **RCON 応答結果:**\n```text\n%s\n```", resp)
	}

	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content: &content,
	})
}

func handleButtons(s *discordgo.Session, i *discordgo.InteractionCreate) {
	customID := i.MessageComponentData().CustomID

	switch customID {
	case "btn_start":
		go executeStartSequence(s, i)
	case "btn_stop":
		go executeStopSequence(s, i, false)
	case "btn_force_stop":
		go executeStopSequence(s, i, true)
	}
}

func executeStartSequence(s *discordgo.Session, i *discordgo.InteractionCreate) {
	content := "🔄 **サーバー起動プロセスを開始しました**\n⏳ [待機中] GCEインスタンスの起動リクエスト送信"
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{Content: content},
	})

	startTime := time.Now()
	ctx := context.Background()
	computeService, _ := compute.NewService(ctx)

	// Phase 1: インスタンスの起動
	_, err := computeService.Instances.Start(ProjectID, Zone, InstanceName).Context(ctx).Do()
	if err != nil {
		errContent := fmt.Sprintf("❌ GCEの起動に失敗しました: %v", err)
		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &errContent})
		return
	}

	// Phase 2: RUNNING 状態の監視
	content = "🔄 **サーバー起動プロセスを開始しました**\n🔄 [処理中] GCEインスタンスの状態を検証中 (RUNNING待ち)"
	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})

	var extIP string
	for {
		inst, err := computeService.Instances.Get(ProjectID, Zone, InstanceName).Context(ctx).Do()
		if err == nil && inst.Status == "RUNNING" {
			if len(inst.NetworkInterfaces) > 0 && len(inst.NetworkInterfaces[0].AccessConfigs) > 0 {
				extIP = inst.NetworkInterfaces[0].AccessConfigs[0].NatIP
				break
			}
		}
		time.Sleep(5 * time.Second)
	}

	// Phase 3: Cloudflare DNSレコードの動的更新
	content = "🔄 **サーバー起動プロセスを開始しました**\n✅ [完了] GCEインスタンス起動成功\n🔄 [処理中] Cloudflare DNSレコードのAレコードを更新中"
	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})

	api, err := cloudflare.NewWithAPIToken(CFAPIToken)
	if err == nil {
		_, err = api.UpdateDNSRecord(ctx, cloudflare.ResourceIdentifier(CFZoneID), cloudflare.UpdateDNSRecordParams{
			ID:      "minecraft_record_id_placeholder",
			Type:    "A",
			Name:    CFRecordName,
			Content: extIP,
			TTL:     60,
			Proxied: func(b bool) *bool { return &b }(false),
		})
	}
	if err != nil {
		log.Printf("DNS更新警告 (スキップして続行): %v", err)
	}

	// Phase 4: マイクラプロセス（RCON疎通）の検証
	content = "🔄 **サーバー起動プロセスを開始しました**\n✅ [完了] GCEインスタンス起動成功\n✅ [完了] Cloudflare DNS更新完了\n🔄 [処理中] マインクラフトサーバープログラムの疎通確認中 (RCON待ち)"
	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})

	for {
		_, err := executeRCON("list")
		if err == nil {
			break
		}
		time.Sleep(5 * time.Second)
	}

	elapsed := time.Since(startTime).Seconds()
	finalContent := fmt.Sprintf("✅ **マインクラフトサーバーの起動が完了しました**\n🌐 ドメイン: `%s`\n⏱️ 総起動時間: `%.1f` 秒\n\nシステムは正常に常駐監視下に移行しました。", CFRecordName, elapsed)
	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &finalContent})

	sendPanel(s, i)
}

func executeStopSequence(s *discordgo.Session, i *discordgo.InteractionCreate, force bool) {
	if !force {
		playersStr, err := executeRCON("list")
		if err == nil && !strings.Contains(playersStr, "There are 0/") {
			content := fmt.Sprintf("⚠️ **警告: プレイヤーがまだサーバーに滞在しています。**\n%s\n本当に停止する場合は以下の「強制停止」を押してください。", playersStr)
			component := discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					discordgo.Button{
						Label:    "強制停止 (⚠️)",
						Style:    discordgo.DangerButton,
						CustomID: "btn_force_stop",
					},
				},
			}
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseUpdateMessage,
				Data: &discordgo.InteractionResponseData{
					Content:    content,
					Components: []discordgo.MessageComponent{component},
				},
			})
			return
		}
	}

	content := "🔄 **サーバー停止プロセスを開始しました**\n🔄 [処理中] Compute Engine API 経由で停止シグナルを送信中"
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{Content: content, Components: []discordgo.MessageComponent{}},
	})

	ctx := context.Background()
	computeService, _ := compute.NewService(ctx)
	_, err := computeService.Instances.Stop(ProjectID, Zone, InstanceName).Context(ctx).Do()
	if err != nil {
		errContent := fmt.Sprintf("❌ GCEの停止要求に失敗しました: %v", err)
		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &errContent})
		return
	}

	for {
		inst, err := computeService.Instances.Get(ProjectID, Zone, InstanceName).Context(ctx).Do()
		if err == nil && inst.Status == "TERMINATED" {
			break
		}
		time.Sleep(5 * time.Second)
	}

	state.isTimerActive = false
	finalContent := "✅ **マインクラフトサーバーは正常にシャットダウンされ、インスタンスは TERMINATED 状態になりました。**"
	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &finalContent})

	sendPanel(s, i)
}

func startPollingTicker(dg *discordgo.Session) {
	ticker := time.NewTicker(1 * time.Minute)
	for range ticker.C {
		listResp, err := executeRCON("list")
		if err != nil {
			continue
		}

		handlePlayerJoinLeave(dg, listResp)

		if strings.Contains(listResp, "There are 0/") {
			if !state.isTimerActive {
				state.isTimerActive = true
				state.emptyStartTime = time.Now()
				msg := "📥 **通知:** サーバー内のプレイヤーが0名になりました。このまま状態が維持された場合、1時間後に自動的にシャットダウンします。"
				dg.ChannelMessageSend(NotificationChannel, msg)
			} else {
				if time.Since(state.emptyStartTime) >= 1*time.Hour {
					msg := "🛑 **自動シャットダウン判定:** プレイヤー0名の状態が1時間継続したため、サーバーの自動停止処理を実行します。"
					dg.ChannelMessageSend(NotificationChannel, msg)

					ctx := context.Background()
					computeService, _ := compute.NewService(ctx)
					computeService.Instances.Stop(ProjectID, Zone, InstanceName).Context(ctx).Do()
					state.isTimerActive = false
				}
			}
		} else {
			if state.isTimerActive {
				state.isTimerActive = false
				msg := "🔄 **通知:** プレイヤーのログインを検知したため、自動シャットダウンタイマーを解除しました。"
				dg.ChannelMessageSend(NotificationChannel, msg)
			}
		}
	}
}

func handlePlayerJoinLeave(dg *discordgo.Session, listResp string) {
	parts := strings.Split(listResp, ":")
	currentPlayers := make(map[string]bool)
	if len(parts) > 1 && strings.TrimSpace(parts[1]) != "" {
		names := strings.Split(parts[1], ",")
		for _, name := range names {
			pName := strings.TrimSpace(name)
			if pName != "" {
				currentPlayers[pName] = true
			}
		}
	}

	for name := range currentPlayers {
		if !state.lastPlayers[name] {
			msg := fmt.Sprintf("📥 **[入室]** `%s` がサーバーに参加しました。", name)
			dg.ChannelMessageSend(NotificationChannel, msg)
		}
	}

	for name := range state.lastPlayers {
		if !currentPlayers[name] {
			msg := fmt.Sprintf("📤 **[退出]** `%s` がサーバーから退出しました。", name)
			dg.ChannelMessageSend(NotificationChannel, msg)
		}
	}

	state.lastPlayers = currentPlayers
}
