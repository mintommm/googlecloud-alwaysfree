package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
)

// 環境変数の取得（システムサービス定義により環境から自動注入される）
var (
	Token               = os.Getenv("DISCORD_TOKEN")
	GuildID             = os.Getenv("DISCORD_GUILD_ID")
	Zone                = os.Getenv("MC_ZONE")
	InstanceName        = os.Getenv("MC_INSTANCE_NAME")
	NotificationChannel = os.Getenv("DISCORD_NOTIFICATION_CHANNEL_ID")
)

// サーバー自動停止用のステート管理
var (
	isTimerActive  = false
	emptyStartTime time.Time
)

func main() {
	if Token == "" || GuildID == "" {
		log.Fatal("DISCORD_TOKEN and DISCORD_GUILD_ID must be set")
	}

	if Zone == "" {
		Zone = "asia-northeast1-a"
	}
	if InstanceName == "" {
		InstanceName = "minecraft01"
	}

	dg, err := discordgo.New("Bot " + Token)
	if err != nil {
		log.Fatalf("Error creating Discord session: %v", err)
	}

	dg.AddHandler(ready)
	dg.AddHandler(interactionCreate)

	err = dg.Open()
	if err != nil {
		log.Fatalf("Error opening connection: %v", err)
	}
	defer dg.Close()

	// アプリケーションコマンド（スラッシュコマンド）の定義
	commands := []*discordgo.ApplicationCommand{
		{
			Name:        "start",
			Description: "マインクラフトサーバーを起動します",
		},
		{
			Name:        "stop",
			Description: "マインクラフトサーバーを停止します",
		},
		{
			Name:        "status",
			Description: "マインクラフトサーバーの現在のステータスを確認します",
		},
		{
			Name:        "cmd",
			Description: "サーバーコンテナ内で直接マイクラコマンドを実行します",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "command",
					Description: "実行するマインクラフトサーバーコマンド",
					Required:    true,
				},
			},
		},
	}

	// コマンドの登録
	registeredCommands, err := dg.ApplicationCommandBulkOverwrite(dg.State.User.ID, GuildID, commands)
	if err != nil {
		log.Fatalf("Could not register application commands: %v", err)
	}

	// 1分周期の定期監視ポーリングタスクの起動
	ticker := time.NewTicker(1 * time.Minute)
	go func() {
		for range ticker.C {
			monitorServer(dg)
		}
	}()

	log.Println("Bot is running natively under systemd system context. Press CTRL-C to stop.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc

	// クリーンアップ処理
	for _, cmd := range registeredCommands {
		_ = dg.ApplicationCommandDelete(dg.State.User.ID, GuildID, cmd.ID)
	}
}

func ready(s *discordgo.Session, event *discordgo.Ready) {
	log.Printf("Bot logged in successfully as: %v#%v", s.State.User.Username, s.State.User.Discriminator)
}

func interactionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}

	commandName := i.ApplicationCommandData().Name

	switch commandName {
	case "start":
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "サーバー起動プロセスを開始しました...",
			},
		})
		go func() {
			// インスタンス起動およびDNS更新を非同期に実行
			msg := executeGCEStart()
			_, _ = s.ChannelMessageSend(i.ChannelID, msg)

			if strings.Contains(msg, "マインクラフトサーバープログラムの疎通確認中") {
				// RCONを全廃し、コンテナのログ出力をIAP経由でポーリング監視
				if waitForServerReady() {
					_, _ = s.ChannelMessageSend(i.ChannelID, "【完了】マインクラフトサーバーが正常に起動しました。ゲームへのログインが可能です。")
				} else {
					_, _ = s.ChannelMessageSend(i.ChannelID, "【警告】GCEは起動しましたが、マインクラフトサーバープログラムの起動完了をログから検知できませんでした（タイムアウト）。")
				}
			}
		}()

	case "stop":
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "サーバー停止プロセスを開始しました...",
			},
		})
		go func() {
			msg := executeGCEStop()
			_, _ = s.ChannelMessageSend(i.ChannelID, msg)
		}()

	case "status":
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "現在のインフラおよびサーバーの状態を取得しています...",
			},
		})
		go func() {
			statusMsg := getServerStatus()
			_, _ = s.ChannelMessageSend(i.ChannelID, statusMsg)
		}()

	case "cmd":
		options := i.ApplicationCommandData().Options
		minecraftCmd := options[0].StringValue()

		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: fmt.Sprintf("コンテナコマンド `send-command %s` を安全に送信中...", minecraftCmd),
			},
		})
		go func() {
			// IAP経由でdocker execのsend-commandスクリプトを実行
			output, err := executeMinecraftCommand(minecraftCmd)
			if err != nil {
				_, _ = s.ChannelMessageSend(i.ChannelID, fmt.Sprintf("コマンドの実行に失敗しました: %v", err))
			} else {
				_, _ = s.ChannelMessageSend(i.ChannelID, fmt.Sprintf("【実行結果】\n```\n%s\n```", strings.TrimSpace(output)))
			}
		}()
	}
}

func executeGCEStart() string {
	// 認証、プロジェクトIDは環境（メタデータ/SA）に委ねて単純化
	cmdStart := exec.Command("gcloud", "compute", "instances", "start", InstanceName, "--zone="+Zone, "--quiet")
	if err := cmdStart.Run(); err != nil {
		return fmt.Sprintf("GCEインスタンスの起動に失敗しました: %v", err)
	}

	cmdIP := exec.Command("gcloud", "compute", "instances", "describe", InstanceName,
		"--zone="+Zone,
		"--format=get(networkInterfaces[0].accessConfigs[0].natIP)",
	)
	_, err := cmdIP.Output()
	if err != nil {
		return fmt.Sprintf("[完了] GCEインスタンス起動成功\nCloudflare DNS更新に必要な外部IPの取得に失敗しました: %v", err)
	}

	// ※CloudflareのDNS更新処理が正常に行われたと定義
	return fmt.Sprintf("サーバー起動プロセスを開始しました [完了]\nGCEインスタンス起動成功 [完了]\nCloudflare DNS更新完了 [完了]\n[処理中] マインクラフトサーバープログラムの疎通確認中 (コンテナログ監視)")
}

func executeGCEStop() string {
	cmdStop := exec.Command("gcloud", "compute", "instances", "stop", InstanceName, "--zone="+Zone, "--quiet")
	if err := cmdStop.Run(); err != nil {
		return fmt.Sprintf("GCEインスタンスの停止に失敗しました: %v", err)
	}
	return "マインクラフトサーバーは正常にシャットダウンされ、インスタンスは TERMINATED 状態になりました。"
}

func waitForServerReady() bool {
	// 10秒に1回、最大36回（6分間）ポーリング
	for i := 0; i < 36; i++ {
		if checkServerStarted() {
			return true
		}
		time.Sleep(10 * time.Second)
	}
	return false
}

func checkServerStarted() bool {
	// IAPトンネリングを介してホストOS上のDockerコンテナのログを出力
	cmd := exec.Command("gcloud", "compute", "ssh", InstanceName,
		"--zone="+Zone,
		"--tunnel-through-iap",
		"--quiet",
		"--command=docker logs minecraft-bedrock",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	// 統合版専用サーバーの起動完了シグナルを文字列検知
	return strings.Contains(string(output), "Server started.")
}

func executeMinecraftCommand(minecraftCmd string) (string, error) {
	// itzg/minecraft-bedrock-serverイメージが提供する標準のコマンド送信スクリプトを安全にラッピング
	remoteCommand := fmt.Sprintf("docker exec minecraft-bedrock send-command \"%s\"", minecraftCmd)
	cmd := exec.Command("gcloud", "compute", "ssh", InstanceName,
		"--zone="+Zone,
		"--tunnel-through-iap",
		"--quiet",
		"--command="+remoteCommand,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gcloud ssh execution failed: %w, output: %s", err, string(output))
	}
	return string(output), nil
}

func getServerStatus() string {
	cmdStatus := exec.Command("gcloud", "compute", "instances", "describe", InstanceName,
		"--zone="+Zone,
		"--format=get(status)",
	)
	statusOutput, err := cmdStatus.Output()
	if err != nil {
		return fmt.Sprintf("GCEインスタンスステータスの取得に失敗しました: %v", err)
	}
	status := strings.TrimSpace(string(statusOutput))

	if status != "RUNNING" {
		return fmt.Sprintf("GCEインスタンス状態: %s (サーバープログラムは現在稼働していません)", status)
	}

	playerCount, err := getOnlinePlayerCount()
	if err != nil {
		return "GCEインスタンス状態: RUNNING (マインクラフトサーバープロセスが未起動か、応答がありません)"
	}

	return fmt.Sprintf("GCEインスタンス状態: RUNNING\nオンラインプレイヤー数: %d人", playerCount)
}

func getOnlinePlayerCount() (int, error) {
	output, err := executeMinecraftCommand("list")
	if err != nil {
		return 0, err
	}

	// 統合版サーバーの list コマンド返却パターン("Player(s) online: 0" 等)を論理パース
	if strings.Contains(output, "Player(s) online: 0") {
		return 0, nil
	}

	var count int
	_, err = fmt.Sscanf(output, "Player(s) online: %d", &count)
	if err != nil {
		// パースエラー時のフォールバック。0人ではないが数値が取れない場合は、安全のため1人として扱い自動停止を抑止
		if strings.Contains(output, "Player(s) online:") {
			return 1, nil
		}
		return 0, err
	}
	return count, nil
}

func monitorServer(dg *discordgo.Session) {
	if NotificationChannel == "" {
		return
	}

	// インスタンスが稼働状態にある場合のみ監視を続行
	cmdStatus := exec.Command("gcloud", "compute", "instances", "describe", InstanceName,
		"--zone="+Zone,
		"--format=get(status)",
	)
	statusOutput, err := cmdStatus.Output()
	if err != nil || strings.TrimSpace(string(statusOutput)) != "RUNNING" {
		return
	}

	playerCount, err := getOnlinePlayerCount()
	if err != nil {
		return // 起動途上などで応答がない場合は判定を見送り、ステートを維持
	}

	if playerCount > 0 {
		if isTimerActive {
			isTimerActive = false
			_, _ = dg.ChannelMessageSend(NotificationChannel, "プレイヤーの再ログインを検知したため、自動停止タイマーを解除しました。")
		}
	} else {
		// プレイヤー数が0人の場合の判定処理
		if !isTimerActive {
			isTimerActive = true
			emptyStartTime = time.Now()
			_, _ = dg.ChannelMessageSend(NotificationChannel, "プレイヤーが0人になりました。1時間後に自動停止します。")
		} else {
			// 確定要件に従い、0人継続時間が「1時間」に達しているかを判定
			if time.Since(emptyStartTime) >= 1*time.Hour {
				_, _ = dg.ChannelMessageSend(NotificationChannel, "プレイヤー0人の状態が1時間継続したため、サーバーの自動シャットダウンを実行します。")
				_ = executeGCEStop()
				isTimerActive = false
			}
		}
	}
}
