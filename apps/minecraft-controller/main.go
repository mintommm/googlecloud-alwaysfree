package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"google.golang.org/api/compute/v1"
)

var (
	Token            = os.Getenv("DISCORD_TOKEN")
	GuildID          = os.Getenv("DISCORD_GUILD_ID")
	ProjectID        = "mintommm-alwaysfree-gce"
	Zone             = os.Getenv("MC_ZONE")
	InstanceName     = os.Getenv("MC_INSTANCE_NAME")
	CloudflareToken  = os.Getenv("CF_API_TOKEN")
	CloudflareZoneID = os.Getenv("CF_ZONE_ID")
	CloudflareRecord = os.Getenv("CF_RECORD_NAME")
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
	if Token == "" {
		log.Fatal("DISCORD_TOKEN is not set")
	}

	dg, err := discordgo.New("Bot " + Token)
	if err != nil {
		log.Fatalf("Error creating Discord session: %v", err)
	}

	dg.AddHandler(interactionHandler)

	err = dg.Open()
	if err != nil {
		log.Fatalf("Error opening connection: %v", err)
	}
	defer dg.Close()

	commands := []*discordgo.ApplicationCommand{
		{
			Name:        "panel",
			Description: "マインクラフトサーバーの管理パネルを表示します",
		},
	}

	log.Println("コマンドを登録中...")
	_, err = dg.ApplicationCommandBulkOverwrite(dg.State.User.ID, GuildID, commands)
	if err != nil {
		log.Fatalf("Error registering commands: %v", err)
	}

	log.Println("Botが起動しました。Ctrl+Cで終了します。")
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
}

func interactionHandler(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		if i.ApplicationCommandData().Name == "panel" {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "マインクラフトサーバー管理パネル",
					Components: []discordgo.MessageComponent{
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
					},
				},
			})
		}

	case discordgo.InteractionMessageComponent:
		customID := i.MessageComponentData().CustomID
		if customID == "btn_start" {
			// 3秒以上の処理を行うため、事前に応答を保留（Defer）する
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
			})

			// 1. GCEインスタンスの起動
			err := startGCEInstance()
			if err != nil {
				sendFollowupMessage(s, i.Interaction, fmt.Sprintf("サーバーの起動に失敗しました: %v", err))
				return
			}

			// 2. RUNNING状態へ遷移するまでポーリング（最大2分半待機）
			err = waitForInstanceRunning()
			if err != nil {
				sendFollowupMessage(s, i.Interaction, fmt.Sprintf("サーバーの起動確認に失敗しました: %v", err))
				return
			}

			// 3. 新しく割り当てられた外部IPアドレスの取得
			ip, err := getGCEExternalIP()
			if err != nil {
				sendFollowupMessage(s, i.Interaction, fmt.Sprintf("外部IPの取得に失敗しました: %v", err))
				return
			}

			// 4. Cloudflare DNSレコードの書き換え（DDNS）
			err = updateCloudflareDNS(ip)
			if err != nil {
				sendFollowupMessage(s, i.Interaction, fmt.Sprintf("DNSの更新に失敗しました: %v", err))
				return
			}

			// 5. 完了通知
			sendFollowupMessage(s, i.Interaction, fmt.Sprintf("🚀 サーバーを起動しました。\nドメイン: `%s` (IP: %s) で接続可能です。", CloudflareRecord, ip))

		} else if customID == "btn_stop" {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
			})

			// GCEインスタンスの停止
			err := stopGCEInstance()
			if err != nil {
				sendFollowupMessage(s, i.Interaction, fmt.Sprintf("サーバーの停止に失敗しました: %v", err))
				return
			}

			sendFollowupMessage(s, i.Interaction, "🛑 サーバーの停止リクエストを送信しました。")
		}
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

func waitForInstanceRunning() error {
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
		if instance.Status == "RUNNING" {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("timeout waiting for instance to be RUNNING")
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
	return "", fmt.Errorf("external IP not found")
}

func updateCloudflareDNS(ip string) error {
	client := &http.Client{Timeout: 10 * time.Second}

	// ① 対象レコードの内部IDを名称から検索して取得
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
		return fmt.Errorf("dns record not found on cloudflare")
	}
	recordID := cfResp.Result[0].ID

	// ② 特定した内部IDに対してPATCHリクエストを送信し、Aレコードを更新
	updateURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", CloudflareZoneID, recordID)
	payload, err := json.Marshal(DNSRecord{
		Type:    "A",
		Name:    CloudflareRecord,
		Content: ip,
		TTL:     1,     // Auto
		Proxied: false, // DNS Only (UDP透過に必須)
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
		return fmt.Errorf("failed to update cloudflare dns record")
	}

	return nil
}

func sendFollowupMessage(s *discordgo.Session, i *discordgo.Interaction, content string) {
	s.FollowupMessageCreate(i, false, &discordgo.WebhookParams{ // discordgo. を付与
		Content: content,
	})
}
