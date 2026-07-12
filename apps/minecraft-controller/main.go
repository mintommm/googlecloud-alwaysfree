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

type SafeLogBuffer struct {
	mu    sync.Mutex
	lines []LogLine
}

var (
	GlobalLogBuffer SafeLogBuffer
	CurrentPlayers  = 0
	PlayersMutex    sync.Mutex
	isTimerActive   = false
	emptyStartTime  time.Time
	streamCancel    context.CancelFunc
	streamMu        sync.Mutex
)

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

func (b *SafeLogBuffer) Append(msg string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.lines = append(b.lines, LogLine{Timestamp: now, Message: msg})

	cutoff := now.Add(-30 * time.Second)
	idx := 0
	for i, line := range b.lines {
		if line.Timestamp.After(cutoff) {
			idx = i
			break
		}
	}
	if idx > 0 {
		b.lines = b.lines[idx:]
	}
}

func (b *SafeLogBuffer) ExtractSince(since time.Time) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var result []string
	for _, line := range b.lines {
		if line.Timestamp.After(since) || line.Timestamp.Equal(since) {
			result = append(result, line.Message)
		}
	}
	return result
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
	// 【不具合①対策】 --tail=20 を指定し、フフライング接続時でも直近のServer started.を見落とさない構造へ変更
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
		GlobalLogBuffer.Append(line)
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
	// 【不具合②対策】 スラッシュコマンドに加え、メッセージコンポーネント(ボタン)の判定空間を解放
	if i.Type != discordgo.InteractionApplicationCommand && i.Type != discordgo.InteractionMessageComponent {
		return
	}

	// 呼び出し元のコンテキストに応じてアクション名を透過的にマッピング
	var actionName string
	if i.Type == discordgo.InteractionApplicationCommand {
		actionName = i.ApplicationCommandData().Name
	} else if i.Type == discordgo.InteractionMessageComponent {
		actionName = i.MessageComponentData().CustomID
	}

	switch actionName {
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
		// スラッシュコマンドからの呼び出し時のみ、配列から引数を抽出
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
			startTime := time.Now()

			remoteCommand := fmt.Sprintf("docker exec minecraft-bedrock send-command \"%s\"", minecraftCmd)
			_, err := executeRemoteCommandGetStdout(remoteCommand)
			if err != nil {
				_, _ = s.ChannelMessageSend(i.ChannelID, fmt.Sprintf("コマンドのインジェクションに失敗しました: %v", err))
				return
			}

			time.Sleep(2 * time.Second)

			capturedLogs := GlobalLogBuffer.ExtractSince(startTime)
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
