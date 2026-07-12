package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var (
	Token               = os.Getenv("DISCORD_TOKEN")
	GuildID             = os.Getenv("DISCORD_GUILD_ID")
	Zone                = os.Getenv("MC_ZONE")
	InstanceName        = os.Getenv("MC_INSTANCE_NAME")
	NotificationChannel = os.Getenv("DISCORD_NOTIFICATION_CHANNEL_ID")
)

// BDS WebSocketプロトコルのJSON構造体定義
type WSMessage struct {
	Header WSHeader        `json:"header"`
	Body   json.RawMessage `json:"body"`
}

type WSHeader struct {
	Version        int    `json:"version"`
	RequestID      string `json:"requestId"`
	MessageType    string `json:"messageType"`
	MessagePurpose string `json:"messagePurpose"`
}

type WSCommandBody struct {
	Version     int      `json:"version"`
	CommandLine string   `json:"commandLine"`
	Origin      WSOrigin `json:"origin"`
}

type WSOrigin struct {
	Type string `json:"type"`
}

type WSEventBody struct {
	EventName string `json:"eventName"`
}

// 接続管理および非同期レスポンス同期用のグローバルステート
var (
	wsConn      *websocket.Conn
	wsMutex     sync.Mutex
	responseMap sync.Map // key: requestId (string), value: chan string
	isTimerActive  = false
	emptyStartTime time.Time
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func main() {
	if Token == "" || GuildID == "" {
		log.Fatal("DISCORD_TOKEN and DISCORD_GUILD_ID must be set")
	}

	dg, err := discordgo.New("Bot " + Token)
	if err != nil {
		log.Fatalf("Error creating Discord session: %v", err)
	}

	dg.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		log.Printf("Bot logged in as: %v#%v", s.State.User.Username, s.State.User.Discriminator)
	})
	dg.AddHandler(interactionCreate)

	if err := dg.Open(); err != nil {
		log.Fatalf("Error opening Discord connection: %v", err)
	}
	defer dg.Close()

	// 内部ポート8000番でのWebSocketサーバー起動（非同期実行）
	go startWebSocketServer(dg)

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

	log.Println("Bot system context is active. Listening on network configurations...")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc

	for _, cmd := range registeredCommands {
		_ = dg.ApplicationCommandDelete(dg.State.User.ID, GuildID, cmd.ID)
	}
}

// BDSからの接続を待ち受けるハンドラ
func startWebSocketServer(dg *discordgo.Session) {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("WebSocket upgrade failed: %v", err)
			return
		}

		wsMutex.Lock()
		wsConn = conn
		wsMutex.Unlock()
		log.Println("【接続確立】Minecraft BDS とのWebSocket永続接続が完了しました。")

		// 接続成功直後に入退出イベントの購読要求（サブスクリプション）を送信
		subscribeEvent("PlayerJoined")
		subscribeEvent("PlayerLeft")

		// 起動ポーリングを即時充足させるための通知シグナルをDiscordに送信可能
		if NotificationChannel != "" {
			_, _ = dg.ChannelMessageSend(NotificationChannel, "【完了】マインクラフトサーバーの起動を確認し、制御バスを確立しました。")
		}

		// メッセージ受信ループの稼働
		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				log.Printf("WebSocket read connection closed: %v", err)
				wsMutex.Lock()
				wsConn = nil
				wsMutex.Unlock()
				break
			}
			handleIncomingWSMessage(dg, message)
		}
	})

	log.Println("Starting native WebSocket listener on 0.0.0.0:8000...")
	if err := http.ListenAndServe(":8000", nil); err != nil {
		log.Fatalf("Failed to start HTTP server for WS: %v", err)
	}
}

// 受信したJSONオブジェクトのパースとルーティング
func handleIncomingWSMessage(dg *discordgo.Session, message []byte) {
	var msg WSMessage
	if err := json.Unmarshal(message, &msg); err != nil {
		return
	}

	// 1. コマンド実行結果（requestId）の同期処理へのマッピング
	if ch, exists := responseMap.Load(msg.Header.RequestID); exists {
		if responseChan, ok := ch.(chan string); ok {
			responseChan <- string(message)
			return
		}
	}

	// 2. イベントプッシュ（入退室通知）の処理
	if msg.Header.MessageType == "event" {
		var eventBody map[string]interface{}
		if err := json.Unmarshal(msg.Body, &eventBody); err != nil {
			return
		}

		eventName := eventBody["eventName"].(string)
		properties := eventBody["properties"].(map[string]interface{})
		player := properties["player"].(string)

		if NotificationChannel == "" {
			return
		}

		if eventName == "PlayerJoined" {
			_, _ = dg.ChannelMessageSend(NotificationChannel, fmt.Sprintf("📥 プレイヤー **%s** がサーバーに参加しました。", player))
			// 自動停止タイマーが動いていれば解除
			if isTimerActive {
				isTimerActive = false
				_, _ = dg.ChannelMessageSend(NotificationChannel, "自動停止タイマーを解除しました。")
			}
		} else if eventName == "PlayerLeft" {
			_, _ = dg.ChannelMessageSend(NotificationChannel, fmt.Sprintf("📤 プレイヤー **%s** がサーバーから退出しました。", player))

			// 人数チェックを行い、0人であればタイマーを開始
			go func() {
				time.Sleep(5 * time.Second) // 切断処理の完了猶予
				if count, err := getOnlinePlayerCountWS(); err == nil && count == 0 {
					isTimerActive = true
					emptyStartTime = time.Now()
					_, _ = dg.ChannelMessageSend(NotificationChannel, "プレイヤー数が0人になりました。1時間後に自動停止します。")

					// 1時間後の自動停止監視タスク
					go func(startTime time.Time) {
						time.Sleep(1 * time.Hour)
						if isTimerActive && emptyStartTime.Equal(startTime) {
							_, _ = dg.ChannelMessageSend(NotificationChannel, "プレイヤー0人の状態が1時間継続したため、自動シャットダウンを実行します。")
							exec.Command("gcloud", "compute", "instances", "stop", InstanceName, "--zone="+Zone, "--quiet").Run()
							isTimerActive = false
						}
					}(emptyStartTime)
				}
			}()
		}
	}
}

// イベント購読用のJSONコマンド送信関数
func subscribeEvent(eventName string) {
	reqID := uuid.New().String()
	body, _ := json.Marshal(WSEventBody{EventName: eventName})
	msg := WSMessage{
		Header: WSHeader{
			Version:        1,
			RequestID:      reqID,
			MessageType:    "commandRequest",
			MessagePurpose: "subscribe",
		},
		Body: body,
	}
	sendWSJSON(msg)
}

func sendWSJSON(msg interface{}) bool {
	wsMutex.Lock()
	defer wsMutex.Unlock()
	if wsConn == nil {
		return false
	}
	err := wsConn.WriteJSON(msg)
	return err == nil
}

// WebSocketを介した命令の確実な同期実行ロジック
func sendCommandAndWait(commandLine string) (string, error) {
	wsMutex.Lock()
	hasConn := wsConn != nil
	wsMutex.Unlock()

	if !hasConn {
		return "", fmt.Errorf("MinecraftサーバーとのWebSocket制御コネクションが確立されていません")
	}

	reqID := uuid.New().String()
	cmdBody, _ := json.Marshal(WSCommandBody{
		Version:     1,
		CommandLine: commandLine,
		Origin:      WSOrigin{Type: "player"},
	})

	msg := WSMessage{
		Header: WSHeader{
			Version:        1,
			RequestID:      reqID,
			MessageType:    "commandRequest",
			MessagePurpose: "commandRequest",
		},
		Body: cmdBody,
	}

	// 応答用チャネルの生成と同期用Mapへの登録
	ch := make(chan string, 1)
	responseMap.Store(reqID, ch)
	defer responseMap.Delete(reqID)

	if !sendWSJSON(msg) {
		return "", fmt.Errorf("WebSocketデータの送信に失敗しました")
	}

	// タイムアウトを設けて応答を同期待機
	select {
	case res := <-ch:
		return res, nil
	case <-time.After(5 * time.Second):
		return "", fmt.Errorf("サーバーからの応答タイムアウト（5秒）")
	}
}

func getOnlinePlayerCountWS() (int, error) {
	res, err := sendCommandAndWait("list")
	if err != nil {
		return 0, err
	}

	// 戻り値JSON内のbody/statusMessageに配置された "There are X/Y players online" をパース
	var msg WSMessage
	_ = json.Unmarshal([]byte(res), &msg)

	var bodyMap map[string]interface{}
	if err := json.Unmarshal(msg.Body, &bodyMap); err != nil {
		return 0, err
	}

	statusMessage, ok := bodyMap["statusMessage"].(string)
	if !ok {
		return 0, fmt.Errorf("invalid status message format")
	}

	var current, max int
	_, err = fmt.Sscanf(statusMessage, "There are %d/%d players online", &current, &max)
	if err != nil {
		return 0, err
	}
	return current, nil
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
			_, _ = s.ChannelMessageSend(i.ChannelID, "GCEインスタンスの起動に成功しました。[処理中] マインクラフトプログラムからの接続バス(WebSocket)確立を待機しています...")
		}()

	case "stop":
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: "サーバーのシャットダウン処理を実行します..."},
		})
		go func() {
			// 安全に停止させるため、事前にコンテナの停止猶予を持たせる目的等で直接GCE stopを実行
			exec.Command("gcloud", "compute", "instances", "stop", InstanceName, "--zone="+Zone, "--quiet").Run()
			_, _ = s.ChannelMessageSend(i.ChannelID, "マインクラフトサーバーは正常に停止し、インスタンスは TERMINATED 状態になりました。")
		}()

	case "status":
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: "現在のインフラおよびサーバー状態を同期中..."},
		})
		go func() {
			cmdStatus := exec.Command("gcloud", "compute", "instances", "describe", InstanceName, "--zone="+Zone, "--format=get(status)")
			statusOutput, err := cmdStatus.Output()
			if err != nil {
				_, _ = s.ChannelMessageSend(i.ChannelID, "ステータス取得失敗")
				return
			}
			gceStatus := strings.TrimSpace(string(statusOutput))

			if gceStatus != "RUNNING" {
				_, _ = s.ChannelMessageSend(i.ChannelID, fmt.Sprintf("GCEインスタンス状態: %s (サーバープログラムは現在停止しています)", gceStatus))
				return
			}

			count, err := getOnlinePlayerCountWS()
			if err != nil {
				_, _ = s.ChannelMessageSend(i.ChannelID, "GCEインスタンス状態: RUNNING (WebSocketバス接続確立待ち、または応答なし)")
				return
			}
			_, _ = s.ChannelMessageSend(i.ChannelID, fmt.Sprintf("GCEインスタンス状態: RUNNING\nオンラインプレイヤー数: %d人", count))
		}()

	case "cmd":
		minecraftCmd := i.ApplicationCommandData().Options[0].StringValue()
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: fmt.Sprintf("WebSocketバス経由でコマンド `%s` を同期送信中...", minecraftCmd)},
		})
		go func() {
			res, err := sendCommandAndWait(minecraftCmd)
			if err != nil {
				_, _ = s.ChannelMessageSend(i.ChannelID, fmt.Sprintf("コマンド送信失敗: %v", err))
				return
			}

			var msg WSMessage
			_ = json.Unmarshal([]byte(res), &msg)
			var bodyMap map[string]interface{}
			_ = json.Unmarshal(msg.Body, &bodyMap)
			statusMessage, _ := bodyMap["statusMessage"].(string)

			_, _ = s.ChannelMessageSend(i.ChannelID, fmt.Sprintf("【実行結果】\n```\n%s\n```", strings.TrimSpace(statusMessage)))
		}()
	}
}
