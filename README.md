# Always Free Google Cloud Infrastructure & Minecraft Controller

Google Cloud Platform (GCP) の Always Free（無料枠）インスタンス上で常駐稼働する Discord コントローラー Bot (`apps/minecraft-controller`)、および Minecraft Bedrock サーバーのインフラ定義（Terraform）と CI/CD パイプラインを管理するモノレポリポジトリ。

---

## 1. システムアーキテクチャとインフラ設計根拠 (Why)

```mermaid
graph TD
    USER[ユーザー / プレイヤー] -->|Discord スラッシュコマンド| BOT[apps/minecraft-controller (Always Free GCE: e2-micro)]
    BOT -->|DDNS 自動更新| CF[Cloudflare API (alwaysfree.krmtn.org / minecraft.krmtn.org)]
    BOT -->|Webhook リッスン :8080| GH_CONFIG[設定リポジトリ Webhook (minecraft-krmtn-org)]
    BOT -->|1時間サイレントバックアップ| GCS[GCS バケット (5世代バージョニング / tar.xz)]
    BOT -->|IAP / SSH 制御| GCE_MC[minecraft01 (Bedrock Server: e2-highcpu-2)]
    BOT -->|Cloud Logging 照会| GCP_LOGS[Cloud Logging API]
    GCE_MC -->|ログ転送| GCP_LOGS
```

### 1.1 2 つの GCE インスタンスの役割分離とコスト最適化
- **常駐 Bot インスタンス (`always_free`)**:
  - `e2-micro` / `us-central1-a` / Debian 12 / GCP Always Free（完全無料 $0.00）。
  - 24時間365日常駐し、Discord コマンド受信、Cloudflare DDNS 更新、外部設定リポジトリからの Webhook 受信を担当。
- **Minecraft サーバーインスタンス (`minecraft01`)**:
  - `e2-highcpu-2` / `asia-northeast1-a` / Debian 12 / オンデマンド運用（プレイ中のみ起動）。
  - なぜ e2-micro で動かさないのか: e2-micro（1GB RAM）では Bedrock サーバーがメモリ不足でクラッシュするため、十分なリソースを持つインスタンスを必要な時だけ起動しコストを最小化する。

### 1.2 動的 IP 運用と DNS 自動伝播確認
- **静的 IP を使わない理由**: Always Free では停止中のインスタンスに紐づく未使用静的 IP に課金が発生するため、動的外部 IP を採用。
- **自己修復 DDNS**: 起動時に GCE メタデータサーバー（`Metadata-Flavor: Google`）から外部 IP を取得し、Cloudflare API で `alwaysfree.krmtn.org` (Proxied: true) を自動更新。
- **DNS 反映確認付き起動通知**:
  `minecraft01` 起動時に `minecraft.krmtn.org` (Proxied: false) を更新後、直ちに通知せず公開 DNS（1.1.1.1 / 8.8.8.8）で名前解決の伝播を確認してから起動操作者へ `@メンション` 通知を送信。DNS キャッシュ遅延による接続失敗を完全防止。

---

## 2. Discord Bot (`apps/minecraft-controller/`) の非自明な設計判断

- **実装言語**: Go 1.26。
- **操作パネルの呼び出し設計**:
  チャンネルへの常時連投による画面埋め尽くしを防ぐため、`/panel` スラッシュコマンドによる明示的呼び出し時のみ操作パネル（ボタンおよび Modal 入力フォーム）を表示。
- **透過的ログ閲覧 (`/logs [lines] [query]`)**:
  - GCE への SSH を行わず Cloud Logging API を直接照会（フィルタ: `resource.type="gce_instance" AND resource.labels.instance_id="minecraft01"`）。月 50GB の無料枠内で運用し追加コスト $0.00。インスタンス停止後も過去ログを閲覧可能。
  - `extractCleanLogMessage` 型スイッチ: GCE Syslog（純粋文字列 `textPayload`）と Docker/Fluentd（JSON `jsonPayload` の `message` / `log` キー）の両形式に対応し、ログ本文を型安全に抽出。タイムスタンプは `[YYYY-MM-DD HH:MM:SS]` (JST) で整形し、青色 Embed で視認性高く返信。
  - **Discord 2000 文字制限の自動分割**: ログ出力が Discord メッセージの上限（2000 文字）を超える場合、Bot 側で安全に chunk 分割して連続送信する。
- **即時 Ack コマンド送信 (`/cmd <command>`)**:
  - Bedrock サーバーの stdout は他プレイヤーのチャットログと混線しパースが極めて不安定なため、実行結果のパースを行わず「✅ サーバーへ正常に送信されました」という Ack を即座に応答（混線・保守コスト完全排除）。
- **10 分無人自動シャットダウン ＆ 再デプロイ誤停止防止**:
  - ログストリーム常時監視により、プレイヤー 0 人から 10 分で自動停止。
  - Bot 起動時に `send-command "list"` を実行して実オンライン人数を照合。Bot 再デプロイ時にプレイヤーがいるにもかかわらず人数が 0 と誤認されて 10 分後に停止する事故を 100% 防止。
- **高圧縮バックアップ基盤 ＆ GCS バージョニング**:
  - 1 時間ごとの完全サイレント定期実行（プレイを邪魔しない）＋ 手動 `/backup` ＋ 停止時実行。
  - XZ 超高圧縮（`tar.xz -9e`）を採用し、Always Free の転送量および GCS 容量を最小化。単一ワールド `kiseki` は `world-data-kiseki.tar.xz` として保存。
  - GCS バケット（`gs://${project_id}-minecraft-backup`）のバージョニングを最新 5 世代（`num_newer_versions = 5, with_state = "ARCHIVED"`）に制限し、容量超過課金を永久防止。
- **GitOps Webhook Hot Reload エンジン**:
  - ポート `8080/webhook` で HTTP POST を受信。`X-Hub-Signature-256` による HMAC-SHA256 署名検証（`http.MaxBytesReader` 1MB 制限）。
  - GCE 上の `/opt/minecraft-controller/config-repo` で `git pull origin main` を実行し、`worlds.yaml` をパース。
  - `defaults.settings` に基づく事前検証を行い、未定義キーや構文エラー検知時は Discord へ ⚠️ エラー通知を送信して安全に中断。
  - 検証通過時はアクティブワールドの設定を `send-command` で稼働中コンテナへ即時反映（Hot Reload）。サーバー起動時（`/start` 完了時）にも自動同期を実行。

---

## 3. インフラ・テスト・CI/CD の非自明な設計判断

- **Terraform 破壊厳禁（インプレース更新）制約**:
  `google_compute_instance.minecraft01` はリプレイスされると動的外部 IP が変わり、コンテナの永続データや起動スクリプトの整合性が崩れるため、メタデータ変更等はインプレース更新を徹底。
- **ファイアウォール設定 (`firewall.tf`)**:
  ポート `8080/tcp`（GitHub Webhook 受信用）およびポート `19132/udp`（Minecraft Bedrock ゲーム通信用）の INGRESS 通信を許可。
- **起動時自動ディザスタリカバリ (`minecraft-startup.sh`)**:
  コンテナ起動時にボリューム内にワールドデータが存在しない場合、GCS バケットから最新のバックアップアーカイブ（`world-data-kiseki.tar.xz`）を自動ダウンロード・解凍して復旧。
- **コンテナログ容量制限**: `max-size=30m, max-file=3` によりディスク容量枯渇クラッシュを防止。
- **Rootless Podman テストランナー (`test-terraform.sh`)**:
  - なぜ Go 単体テストでの HCL パースを却下したのか: Go の HCL2 パーサーは構文エラーしか検出できず、Terraform Provider のスキーマ検証やライフサイクルルールのアサーションが不可能なため。
  - なぜ Rootless Podman なのか: Cloudtop 上に OSS terraform がなく、`g3terraform` は Piper 専用であるため、社内ポリシー（`go/dont-install-docker`）に準拠した非特権 Rootless Podman 内で公式 `terraform:1.10.4` を実行。読み取り専用マウント（`:ro`）、`-backend=false`、`--rm` によりホスト改変とクレデンシャル漏洩を完全遮断。
- **ネイティブ単体テスト (`main_test.tftest.hcl`)**:
  1. `verify_backup_bucket_config`: GCS バックアップバケットの名称、リージョン（US-CENTRAL1）、バージョニング有効化、5 世代保持ライフサイクルルールのアサーション。
  2. `verify_webhook_firewall_rule`: ポート 8080/tcp が Webhook 受信用に正しく開放されていることのアサーション。
- **Single-shot 高速デプロイ ＆ 自動即時ロールバック (`deploy-minecraft.yml`)**:
  NumPy 等の重い依存を排除してデプロイを約 20 秒に短縮。デプロイ直後に `systemctl is-active` でヘルスチェックを行い、起動失敗時は直前の正常バイナリへ自動ロールバック。共有鍵 `WEBHOOK_SECRET` は GitHub Secrets から GCE 上の `/opt/minecraft-controller/.env`（`chmod 600`）へ安全に注入。
- **二重フェーズ運用とガードレールのトレードオフ**:
  ローカル検証（Phase 1）と本番反映（Phase 2）を厳格に分離し、デプロイ直前バックアップと手動承認ゲートを設けることで本番障害を 100% 防止する。このガードレールにより、全自動デプロイと比べて 1 往復の手動確認オーバーヘッドが発生するが、データの確実な保全を優先するトレードオフを受け入れる。
- **変更管理と検証ログの監査性**:
  変更はすべて事前レビューを経てコミットされ、自律エージェントの検証プロセスはトランスクリプトログ（`transcript.jsonl`）によって改ざんなく追跡可能とする。

---

## 4. ローカル開発 ＆ テスト手順 (`Makefile`)

- `make test`: 全テストの一括実行（最初に Go 単体テスト、次に Rootless Podman による Terraform 品質ゲートを順次実行）。
- `make test-bot`: Go Discord Bot 単体テスト（Mock 検証）のみ実行。
- `make test-infra`: Terraform 品質ゲート（fmt, init, validate, test）のみ実行。
