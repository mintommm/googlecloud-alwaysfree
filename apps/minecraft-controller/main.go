package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/bwmarrin/discordgo"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
)

var (
	Token        = os.Getenv("DISCORD_TOKEN")
	ProjectID    = "mintommm-alwaysfree-gce" // 固定値
	Zone         = os.Getenv("MC_ZONE")
	InstanceName = os.Getenv("MC_INSTANCE_NAME")
)

func main() {
	if Token == "" || Zone == "" || InstanceName == "" {
		log.Fatal("必須の環境変数が設定されていません。")
	}

	dg, err := discordgo.New("Bot " + Token)
	if err != nil {
		log.Fatalf("Discordセッションの作成に失敗しました: %v", err)
	}

	// インタラクション（スラッシュコマンドやボタン押下）のハンドラ登録
	dg.AddHandler(interactionHandler)

	// ボットの識別インテントを設定
	dg.Identify.Intents = discordgo.IntentsGuildMessages

	err = dg.Open()
	if err != nil {
		log.Fatalf("Discordへの接続に失敗しました: %v", err)
	}
	defer dg.Close()

	// スラッシュコマンド（/panel）の登録
	commands := []*discordgo.ApplicationCommand{
		{
			Name:        "panel",
			Description: "マインクラフトサーバーの管理パネルを表示します",
		},
	}

	log.Println("コマンドを登録中...")
	_, err = dg.ApplicationCommandBulkOverwrite(dg.State.User.ID, "", commands)
	if err != nil {
		log.Fatalf("コマンドの登録に失敗しました: %v", err)
	}

	log.Println("Botが起動しました。Ctrl+Cで終了します。")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc
}

func interactionHandler(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		// スラッシュコマンド /panel が実行された場合
		if i.ApplicationCommandData().Name == "panel" {
			respondWithPanel(s, i)
		}

	case discordgo.InteractionMessageComponent:
		// ボタンが押された場合
		customID := i.MessageComponentData().CustomID
		handleButtonClick(s, i, customID)
	}
}

// 管理パネル（ボタン付きメッセージ）を送信する関数
func respondWithPanel(s *discordgo.Session, i *discordgo.InteractionCreate) {
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: "🟢 **マインクラフトサーバー管理パネル**\n下のボタンをタップして操作してください。",
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.Button{
							Label:    "サーバー起動",
							Style:    discordgo.SuccessButton,
							CustomID: "btn_start",
							Emoji: &discordgo.ComponentEmoji{
								Name: "🚀",
							},
						},
						discordgo.Button{
							Label:    "サーバー停止",
							Style:    discordgo.DangerButton,
							CustomID: "btn_stop",
							Emoji: &discordgo.ComponentEmoji{
								Name: "🛑",
							},
						},
					},
				},
			},
		},
	})
	if err != nil {
		log.Printf("パネルの応答に失敗しました: %v", err)
	}
}

// ボタン押下時の処理ロジック
func handleButtonClick(s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	// 即座に「処理中...」と応答を返す（Discordの3秒タイムアウト対策）
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})
	if err != nil {
		log.Printf("Deferred応答に失敗しました: %v", err)
		return
	}

	var message string
	ctx := context.Background()

	// GCE APIクライアントの初期化（インスタンスの認証情報を使用）
	computeService, err := compute.NewService(ctx, option.WithoutAuthentication())
	if err != nil {
		sendFollowup(s, i, "APIクライアントの初期化に失敗しました。")
		return
	}

	switch customID {
	case "btn_start":
		log.Println("GCEインスタンスの起動リクエストを受信")
		_, err = computeService.Instances.Start(ProjectID, Zone, InstanceName).Context(ctx).Do()
		if err != nil {
			message = fmt.Sprintf("❌ インスタンスの起動に失敗しました: %v", err)
		} else {
			message = "🚀 マインクラフトサーバー（GCE）の起動命令を送信しました。起動まで1〜2分かかります。"
		}

	case "btn_stop":
		log.Println("GCEインスタンスの停止リクエストを受信")
		_, err = computeService.Instances.Stop(ProjectID, Zone, InstanceName).Context(ctx).Do()
		if err != nil {
			message = fmt.Sprintf("❌ インスタンスの停止に失敗しました: %v", err)
		} else {
			message = "🛑 マインクラフトサーバー（GCE）の停止命令を送信しました。"
		}
	}

	sendFollowup(s, i, message)
}

func sendFollowup(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	_, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: content,
	})
	if err != nil {
		log.Printf("フォローアップメッセージの送信に失敗しました: %v", err)
	}
}
