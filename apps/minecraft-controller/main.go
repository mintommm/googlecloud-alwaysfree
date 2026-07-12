package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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
	RepoName            = os.Getenv("CF_RECORD_NAME")
	GithubOwner         = os.Getenv("GITHUB_OWNER")
)

var (
	CurrentPlayers  = 0
	PlayersMutex    sync.Mutex
	isTimerActive   = false
	emptyStartTime  time.Time
	streamCancel    context.CancelFunc
	streamMu        sync.Mutex
	cmdListeners    sync.Map // key: string(uuid), value: chan string
	tickerCancel    context.CancelFunc
	tickerMu        sync.Mutex
)

var (
	regexPlayerJoin = regexp.MustCompile(`Player connected:\s+([^,]+)`)
	regexPlayerLeft = regexp.MustCompile(`Player disconnected:\s+([^,]+)`)
	regexListCount  = regexp.MustCompile(`There are (\d+)/\d+ players online`)
	regexSaveFile   = regexp.MustCompile(`([^\s,:]+):(\d+)`)
)

type BackupFile struct {
	Path string
	Size string
}

func main() {
	if Token == "" || GuildID == "" || GithubOwner == "" || RepoName == "" {
		log.Fatal("Required environment variables (DISCORD_TOKEN, DISCORD_GUILD_ID, GITHUB_OWNER, CF_RECORD_NAME) must be set")
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

	stopBackupTicker()
	stopLogStream()
	for _, cmd := range registeredCommands {
		_ = dg.ApplicationCommandDelete(dg.State.User.ID, GuildID, cmd.ID)
	}
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
		cmdListeners.Range(func(key, value interface{}) bool {
			if ch, ok := value.(chan string); ok {
				select {
				case ch <- line:
				default:
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

	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if isGCEInstanceRunning() {
				log.Println("【定期バックアップ開始】オンライン差分バックアップシーケンスを実行します。")
				if err := executeOnlineBackupFlow(); err != nil {
					log.Printf("定期バックアップ失敗: %v", err)
					if NotificationChannel != "" {
						_, _ = dg.ChannelMessageSend(NotificationChannel, fmt.Sprintf("⚠️ 【警告】30分毎の定期バックアップ処理に失敗しました。詳細: %v", err))
					}
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
	_, err := executeRemoteCommandGetStdout("docker exec minecraft-bedrock send-command \"save hold\"")
	if err != nil {
		return fmt.Errorf("failed to send save hold: %v", err)
	}

	var queryOutput string
	listenerID := uuid.New().String()
	ch := make(chan string, 100)
	cmdListeners.Store(listenerID, ch)
	defer cmdListeners.Delete(listenerID)

	success := false
	for attempt := 0; attempt < 5; attempt++ {
		time.Sleep(2 * time.Second)
		queryOutput, err = executeRemoteCommandGetStdout("docker exec minecraft-bedrock send-command \"save query\"")
		if err != nil {
			continue
		}

		timeout := time.After(1 * time.Second)
	readLoop:
		for {
			select {
			case line := <-ch:
				if strings.Contains(line, "Data saved. Files are:") {
					queryOutput = line
					success = true
					break readLoop
				}
			case <-timeout:
				break readLoop
			}
		}
		if success || strings.Contains(queryOutput, "Data saved. Files are:") {
			success = true
			break
		}
	}

	if !success {
		_, _ = executeRemoteCommandGetStdout("docker exec minecraft-bedrock send-command \"save resume\"")
		return fmt.Errorf("save query response timeout or invalid")
	}

	matches := regexSaveFile.FindAllStringSubmatch(queryOutput, -1)
	if len(matches) == 0 {
		_, _ = executeRemoteCommandGetStdout("docker exec minecraft-bedrock send-command \"save resume\"")
		return fmt.Errorf("no files parsed from save query")
	}

	var backupFiles []BackupFile
	for _, match := range matches {
		backupFiles = append(backupFiles, BackupFile{Path: match[1], Size: match[2]})
	}

	var scriptBuilder strings.Builder
	scriptBuilder.WriteString("set -e\n")
	for _, bf := range backupFiles {
		escapedPath := strings.ReplaceAll(bf.Path, `"`, `\"`)
		dir := filepath.Dir(escapedPath)
		if dir != "." {
			scriptBuilder.WriteString(fmt.Sprintf("mkdir -p \"/workspace/%s\"\n", dir))
		}
		scriptBuilder.WriteString(fmt.Sprintf("cp \"/data/%s\" \"/workspace/%s\"\n", escapedPath, escapedPath))
		scriptBuilder.WriteString(fmt.Sprintf("truncate -s %s \"/workspace/%s\"\n", bf.Size, escapedPath))
	}

	copyCmd := fmt.Sprintf(`docker run --rm -i \
		-v minecraft-data:/data \
		-v /var/minecraft/git-repo:/workspace \
		alpine sh -s << 'EOF'
%s
EOF`, scriptBuilder.String())

	_, err = executeRemoteCommandGetStdout(copyCmd)
	if err != nil {
		_, _ = executeRemoteCommandGetStdout("docker exec minecraft-bedrock send-command \"save resume\"")
		return fmt.Errorf("failed to copy and truncate files inside container: %v", err)
	}

	_, err = executeRemoteCommandGetStdout("docker exec minecraft-bedrock send-command \"save resume\"")
	if err != nil {
		return fmt.Errorf("failed to send save resume: %v", err)
	}

	go func() {
		keyBytes, err := os.ReadFile("/opt/minecraft-controller/.ssh/github_id")
		if err != nil {
			log.Printf("Failed to read deploy key: %v", err)
			return
		}

		gitCmd := fmt.Sprintf(`docker run --rm -i \
			-v /var/minecraft/git-repo:/workspace \
			alpine/git sh -s << 'EOF'
mkdir -p /root/.ssh
cat << 'KEY' > /root/.ssh/id_ed25519
%s
KEY
chmod 600 /root/.ssh/id_ed25519
echo "github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl" > /root/.ssh/known_hosts

cd /workspace
if [ ! -d .git ]; then
	git init
	git remote add origin git@github.com:%s/%s.git
	git branch -M main
fi
git add .
if git commit -m "Automatic periodic backup: %s"; then
	git push -u origin main
fi
EOF`, string(keyBytes), GithubOwner, RepoName, time.Now().Format("2006-01-02 15:04:05"))

		_, _ = executeRemoteCommandGetStdout(gitCmd)
	}()

	return nil
}

func executeOfflineBackupSequence(dg *discordgo.Session) {
	_, err := executeRemoteCommandGetStdout("docker stop -t 10 minecraft-bedrock")
	if err != nil {
		log.Printf("Failed to stop container container: %v", err)
	}

	stopLogStream()

	syncCmd := `docker run --rm -i \
		-v minecraft-data:/data \
		-v /var/minecraft/git-repo:/workspace \
		alpine sh -s << 'EOF'
set -e
cp -r /data/* /workspace/
EOF`
	_, _ = executeRemoteCommandGetStdout(syncCmd)

	keyBytes, err := os.ReadFile("/opt/minecraft-controller/.ssh/github_id")
	if err == nil {
		gitCmd := fmt.Sprintf(`docker run --rm -i \
			-v /var/minecraft/git-repo:/workspace \
			alpine/git sh -s << 'EOF'
mkdir -p /root/.ssh
cat << 'KEY' > /root/.ssh/id_ed25519
%s
KEY
chmod 600 /root/.ssh/id_ed25519
echo "github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl" > /root/.ssh/known_hosts

cd /workspace
if [ ! -d .git ]; then
	git init
	git remote add origin git@github.com:%s/%s.git
	git branch -M main
fi
git add .
if git commit -m "Final backup execution before instance stop"; then
	git push -u origin main
fi
EOF`, string(keyBytes), GithubOwner, RepoName)
		_, _ = executeRemoteCommandGetStdout(gitCmd)
	}

	_ = exec.Command("gcloud", "compute", "instances", "stop", InstanceName, "--zone="+Zone, "--quiet").Run()
	if NotificationChannel != "" {
		_, _ = dg.ChannelMessageSend(NotificationChannel, "マインクラフトサーバーは正常に停止し、インスタンスは停止状態になりました。")
	}
}

func executeRemoteCommandGetStdout(commandLine string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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
			_, _ = s.ChannelMessageSend(i.ChannelID, "GCEインスタンス起動成功。ストリームパイプラインを結合し、プログラムの完全起動を待記しています...")
			go manageStreamLifecycle(s)
		}()

	case "stop":
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: "サーバーのシャットダウン処理を実行します..."},
		})
		go func() {
			executeOfflineBackupSequence(s)
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
			idleTimer := time.NewTimer(500 * time.Millisecond)
			globalTimeout := time.After(5 * time.Second)

			loop := true
			for loop {
				select {
				case line := <-ch:
					capturedLogs = append(capturedLogs, line)
					if !idleTimer.Stop() {
						select {
						case <-idleTimer.C:
						default:
						}
					}
					idleTimer.Reset(500 * time.Millisecond)
				case <-idleTimer.C:
					loop = false
				case <-globalTimeout:
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
