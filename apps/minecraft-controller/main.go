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

// 共有ログバッファの構造体（直近のログ行をスライディング保持）
type LogLine struct {
	Timestamp time.Time
	Message   string
}

type SafeLogBuffer struct {
	mu    sync.Mutex
	lines []LogLine
}

// グローバルステート管理
var (
	GlobalLogBuffer SafeLogBuffer
	CurrentPlayers  = 0
	PlayersMutex    sync.Mutex
	isTimerActive   = false
	emptyStartTime  time.Time
	streamCancel    context.CancelFunc
	streamMu        sync.Mutex
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
		// 起動時にGCEがRUNNINGであれば、自動的にログストリーミングを開始
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

// 共有バッファへのログ行の追記と、30秒が経過した古い行の切り捨て処理
func (b *SafeLogBuffer) Append(msg string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.lines = append(b.lines, LogLine{Timestamp: now, Message: msg})

	// 30秒以上前のログをスライディングクリア
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

// 指定したタイムスタンプ以降にバッファに書き込まれた生ログを一括抽出
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

// 常時ストリーミングの開始・リトライライフサイクル管理
func manageStreamLifecycle(dg *discordgo.Session) {
	streamMu.Lock()
	if streamCancel != nil {
		streamMu.Unlock()
		return // すでに稼働中の場合は重複起動を防止
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
				// 再接続時の論理要件：単発コマンドで人数を再同期
				syncOnlinePlayersDirect()

				log.Println("【ストリーム開始】IAPキープアライブ付きでstdout常時接続を確立します。")
				err := startLogStreamProcess(ctx, dg)
				if err != nil {
					log.Printf("ストリームプロセスが切断されました: %v。5秒後に再接続を試みます。", err)
				}
			}
			time.Sleep(5 * time.Second) // インスタンス停止中または切断時のバックオフ待機
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

// SSHキープアライブオプションを強制インジェクションした永続ストリーミング実行処理
func startLogStreamProcess(ctx context.Context, dg *discordgo.Session) error {
	cmd := exec.CommandContext(ctx, "gcloud", "compute", "ssh", InstanceName,
		"--zone="+Zone,
		"--tunnel-through-iap",
		"--quiet",
		"--ssh-flag=-o ServerAliveInterval=15",
		"--ssh-flag=-o ServerAliveCountMax=3",
		"--command=docker logs -f --tail=0 minecraft-bedrock",
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	// 警告ノイズが混入する標準エラー出力は、ログ回収を汚さないよう完全に破棄または分離
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return err
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		GlobalLogBuffer.Append(line) // 共有メモリバッファへ格納
		handleLogLineEvents(dg, line)
	}

	return cmd.Wait()
}

// ストリーム行のテキストパースとイベントフック
func handleLogLineEvents(dg *discordgo.Session, line string) {
	if NotificationChannel == "" {
		return
	}

	if strings.Contains(line, "Server started.") {
		_, _ = dg.ChannelMessageSend(NotificationChannel, "【完了】マインクラフトサーバープログラムの完全起動を確認しました。ログイン可能です。")
		return
	}

	// 1. 参加イベントのフック
	if matches := regexPlayerJoin.FindStringSubmatch(line); len(matches) > 1 {
		player := matches[1]
		PlayersMutex.Lock()
		CurrentPlayers++
		isTimerActive = false // 人数が増えたためタイマーは強制解除
		PlayersMutex.Unlock()
		_, _ = dg.ChannelMessageSend(NotificationChannel, fmt.Sprintf("📥 プレイヤー **%s** が参加しました。", player))
		return
	}

	// 2. 退出イベントのフック
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

			// 非同期で1時間後の自動停止監視タスクを実行
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

// 単発のSSHコマンドを安全に実行し、標準出力（stdout）のみを分離して取得する関数
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
	cmd.Stderr = &stderrBuf // 警告ノイズ（標準エラー）をオブジェクトレベルで完全分離

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

// 再接続時や起動時に単発で list コマンドを発行し、インメモリ人数を実態に合わせるロジック
func syncOnlinePlayersDirect() {
	out, err := executeRemoteCommandGetStdout("docker exec minecraft-bedrock send-command list")
	if err != nil {
		return
	}
	// send-command 自体は空を返すため、直後に docker logs から最終行付近を直接回収
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
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}

	switch i.ApplicationCommandData().Name {
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
		minecraftCmd := i.ApplicationCommandData().Options[0].StringValue()
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: fmt.Sprintf("コマンド `%s` を標準入力ストリームへインジェクション中...", minecraftCmd)},
		})
		go func() {
			startTime := time.Now()

			// 1. 標準入力へコマンドをインジェクション（戻り値のStdout自体は空）
			remoteCommand := fmt.Sprintf("docker exec minecraft-bedrock send-command \"%s\"", minecraftCmd)
			_, err := executeRemoteCommandGetStdout(remoteCommand)
			if err != nil {
				_, _ = s.ChannelMessageSend(i.ChannelID, fmt.Sprintf("コマンドのインジェクションに失敗しました: %v", err))
				return
			}

			// 2. 確定要件：コマンド処理、ログ出力、およびストリーム経由のキャッシュ到達のための「2秒ディレイ」
			time.Sleep(2 * time.Second)

			// 3. 共有タイムバッファから、実行時刻以降に追記された生ログ行を全抽出して返却
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
