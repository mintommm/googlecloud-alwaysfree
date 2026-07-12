package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/google/uuid"
)

var (
	Token               = os.Getenv("DISCORD_TOKEN")
	GuildID             = os.Getenv("DISCORD_GUILD_ID")
	Zone                = os.Getenv("MC_ZONE")
	InstanceName        = os.Getenv("MC_INSTANCE_NAME")
	NotificationChannel = os.Getenv("DISCORD_NOTIFICATION_CHANNEL_ID")
)

type LogLine struct {
	Timestamp time.Time
	Message   string
}

// グローバルステート管理
var (
	CurrentPlayers  = 0
	PlayersMutex    sync.Mutex
	isTimerActive   = false
	emptyStartTime  time.Time
	streamCancel    context.CancelFunc
	streamMu        sync.Mutex
	cmdListeners    sync.Map // key: string(uuid), value: chan string
)

// ログ解析用の正規表現
var (
	regexPlayerJoin = regexp.MustCompile(`Player connected:\s+([^,]+)`)
	regexPlayerLeft = regexp.MustCompile(`Player disconnected:\s+([^,]+)`)
	regexListCount  = regexp.MustCompile(`There are (\d+)/\d+ players online`)
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

	dg.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		log.Printf("Bot logged in as: %v#%v", s.State.User.Username, s.State.User.Discriminator)
		go manageStreamLifecycle(dg)
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
					Description: "実行するマインクラフトコマンド",
					Required:    true,
				},
			},
		},
	}

	registeredCommands, err := dg.ApplicationCommandBulkOverwrite(dg.State.User.ID, GuildID, commands)
	if err != nil {
		log.Fatalf("Could not register application commands: %v", err)
	}

	log.Println("Bot system context is active. Managing logs and terminal lifecycles...")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc

	stopLogStream()
	for _, cmd := range registeredCommands {
		_ = dg.ApplicationCommandDelete(dg.State.User.ID, GuildID, cmd.ID)
	}
}

// 常時ストリーミングの開始・リトライライフサイクル管理
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

				log.Println("【ストリーム開始】IAPキープアライブ付きでstdout常時接続を確立します。")
				err := startLogStreamProcess(ctx, dg)
				if err != nil {
					log.Printf("ストリームプロセスが切断されました: %v。5秒後に再接続を試みます。", err)
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
		log.Println("【ストリーム停止】常時ログストリーミングを明示的に終了しました。")
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
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return err
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()

		// アクティブなコマンド待機ゴルーチン（チャネル群）へログ行をブロードキャスト（ファンアウト）
		cmdListeners.Range(func(key, value interface{}) bool {
			if ch, ok := value.(chan string); ok {
				select {
				case ch <- line:
				default: // チャネルが詰まっている場合はスキップしてデッドロックを防止
				}
			}
			return true
		})

		handleLogLineEvents(dg, line)
	}

	return cmd.Wait()
}

func handleLogLineEvents(dg *discordgo.Session, line string) {
	if NotificationChannel == "" {
		return
	}

	if strings.Contains(line, "Server started.") {
		_, _ = dg.ChannelMessageSend(NotificationChannel, "【完了】マインクラフトサーバープログラムの完全起動を確認しました。ログイン可能です。")
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
			_, _ = dg.ChannelMessageSend(NotificationChannel, "プレイヤー数が0人になりました。1時間後に自動停止します。")

			go func(startTime time.Time) {
				time.Sleep(1 * time.Hour)
				PlayersMutex.Lock()
				if isTimerActive && emptyStartTime.Equal(startTime) {
					_, _ = dg.ChannelMessageSend(NotificationChannel, "プレイヤー0人の状態が1時間継続したため、自動シャットダウンを実行します。")
					_ = exec.Command("gcloud", "compute", "instances", "stop", InstanceName, "--zone="+Zone, "--quiet").Run()
					isTimerActive = false
					stopLogStream()
				}
				PlayersMutex.Unlock()
			}(emptyStartTime)
		}
		PlayersMutex.Unlock()
		_, _ = dg.ChannelMessageSend(NotificationChannel, fmt.Sprintf("📤 プレイヤー **%s** が退出しました。", player))
		return
	}
}

func executeRemoteCommandGetStdout(commandLine string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
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
			log.Printf("【同期完了】インメモリオンラインプレイヤー数を実態（%d人）に補正しました。", count)
			return
		}
	}
}

func interactionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand && i.Type != discordgo.InteractionMessageComponent {
		return
	}

	var actionName string
	if i.Type == discordgo.InteractionApplicationCommand {
		actionName = i.ApplicationCommandData().Name
	} else if i.Type == discordgo.InteractionMessageComponent {
		actionName = i.MessageComponentData().CustomID
	}

	switch actionName {
	case "panel":
		// コントロールパネルの初期表示要求に対する同期返信（3秒ルール充足）
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
			Data: &discordgo.InteractionResponseData{Content: "サーバー起動要求をGCPへ送信しました..."},
		})
		go func() {
			cmdStart := exec.Command("gcloud", "compute", "instances", "start", InstanceName, "--zone="+Zone, "--quiet")
			if err := cmdStart.Run(); err != nil {
				_, _ = s.ChannelMessageSend(i.ChannelID, fmt.Sprintf("GCE起動失敗: %v", err))
				return
			}
			_, _ = s.ChannelMessageSend(i.ChannelID, "GCEインスタンス起動成功。【処理中】ストリームパイプラインを結合し、プログラムの完全起動を待機しています...")
			go manageStreamLifecycle(s)
		}()

	case "stop":
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: "サーバーのシャットダウン処理を実行します..."},
		})
		go func() {
			stopLogStream()
			_ = exec.Command("gcloud", "compute", "instances", "stop", InstanceName, "--zone="+Zone, "--quiet").Run()
			_, _ = s.ChannelMessageSend(i.ChannelID, "マインクラフトサーバーは正常に停止し、インスタンスは TERMINATED 状態になりました。")
		}()

	case "status":
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: "現在のインフラおよびメモリステートを確認中..."},
		})
		go func() {
			if !isGCEInstanceRunning() {
				_, _ = s.ChannelMessageSend(i.ChannelID, "GCEインスタンス状態: TERMINATED (サーバープログラムは現在停止しています)")
				return
			}
			PlayersMutex.Lock()
			count := CurrentPlayers
			PlayersMutex.Unlock()
			_, _ = s.ChannelMessageSend(i.ChannelID, fmt.Sprintf("GCEインスタンス状態: RUNNING\nオンラインプレイヤー数 (メモリ同期): %d人", count))
		}()

	case "cmd":
		var minecraftCmd string
		if i.Type == discordgo.InteractionApplicationCommand {
			options := i.ApplicationCommandData().Options
			if len(options) > 0 {
				minecraftCmd = options[0].StringValue()
			}
		}

		if minecraftCmd == "" {
			_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{Content: "エラー: コマンド引数が指定されていません。"},
			})
			return
		}

		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: fmt.Sprintf("コマンド `%s` を標準入力ストリームへインジェクション中...", minecraftCmd)},
		})

		go func() {
			// 【最適化】動的無出力インターバル（Idle Timeout）監視ロジック
			listenerID := uuid.New().String()
			ch := make(chan string, 100)
			cmdListeners.Store(listenerID, ch)
			defer cmdListeners.Delete(listenerID)

			remoteCommand := fmt.Sprintf("docker exec minecraft-bedrock send-command \"%s\"", minecraftCmd)
			_, err := executeRemoteCommandGetStdout(remoteCommand)
			if err != nil {
				_, _ = s.ChannelMessageSend(i.ChannelID, fmt.Sprintf("コマンドのインジェクションに失敗しました: %v", err))
				return
			}

			var capturedLogs []string
			idleTimer := time.NewTimer(500 * time.Millisecond) // 500msの無通信タイムアウトタイマー
			globalTimeout := time.After(5 * time.Second)       // コマンド処理の最大実行猶予（5秒）

			loop := true
			for loop {
				select {
				case line := <-ch:
					capturedLogs = append(capturedLogs, line)
					// 新しいログが流れてきたため、無通信タイムアウトタイマーを停止・再初期化
					if !idleTimer.Stop() {
						select {
						case <-idleTimer.C:
						default:
						}
					}
					idleTimer.Reset(500 * time.Millisecond)

				case <-idleTimer.C:
					// 500msの間、新しいログ行が一切流れなくなったため、応答出力完了とみなしループ脱出
					loop = false

				case <-globalTimeout:
					// サーバー高負荷等に備えた安全装置（最大猶予到達による強制ブレイク）
					loop = false
				}
			}

			if len(capturedLogs) == 0 {
				_, _ = s.ChannelMessageSend(i.ChannelID, "【実行完了】コマンドは送信されましたが、直後のログ出力は空でした。")
				return
			}

			logBlock := strings.Join(capturedLogs, "\n")
			if len(logBlock) > 1900 {
				logBlock = logBlock[:1900] + "\n...(出力が大きいため省略されました)"
			}
			_, _ = s.ChannelMessageSend(i.ChannelID, fmt.Sprintf("【実行直後のコンテナ出力】\n```\n%s\n```", logBlock))
		}()
	}
}