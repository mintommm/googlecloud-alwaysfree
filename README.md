# Always Free Google Cloud Infrastructure & Automation

Google Cloud Platform (GCP)のAlways Free（無料枠）インスタンス上で常駐稼働するDiscordコントローラーBot (`apps/minecraft-controller`)、マネーフォワードME家計簿明細の自動蓄積パイプライン (`apps/moneyforwardme-to-bigquery`)、およびそれらを支えるインフラ定義（Terraform）を管理するモノレポリポジトリ。

---

## システムアーキテクチャとインフラ設計根拠 (Why)

```mermaid
graph TD
    subgraph GCE_ALWAYS_FREE ["常駐 GCE インスタンス (always_free: e2-micro / Always Free $0.00)"]
        BOT["apps/minecraft-controller<br/>(Discord Bot / Go 1.26)"]
        TIMER["systemd timer<br/>(moneyforward-sync.timer: 毎朝04:00 JST)"]
        TRIGGER["/usr/local/bin/trigger-moneyforwardme-to-bigquery.sh<br/>(MemoryMax=256M 保護)"]
        TIMER -->|起動| TRIGGER
    end

    subgraph MINECRAFT_SYS ["Minecraft システム"]
        USER_MC[プレイヤー] -->|Discord コマンド| BOT
        BOT -->|DDNS 自動更新| CF[Cloudflare API]
        BOT -->|Webhook リッスン :80| GH_CONFIG[設定リポジトリ Webhook]
        BOT -->|1時間サイレントバックアップ| GCS[GCS バケット (5世代バージョニング)]
        BOT -->|Compute Engine REST API / VPC内部SSH| GCE_MC[minecraft01 (Bedrock Server: e2-highcpu-2)]
    end

    subgraph MONEYFORWARD_SYS ["マネーフォワードME 自動蓄積パイプライン"]
        TRIGGER -->|POST / (OIDC IDトークン認証)| RUN["Cloud Run (apps/moneyforwardme-to-bigquery)<br/>Playwright / Python 3.13"]
        RUN -->|セッション取得・自動延長保存| SM["Secret Manager<br/>(mf-session-cookie)"]
        RUN -->|CSVダウンロード (当月・先月・前々月)| MF[マネーフォワードME]
        RUN -->|row_hash による MERGE アペンド| BQ["BigQuery (moneyforward dataset)<br/>raw_transactions / v_transactions_latest"]
        RUN -->|実行結果通知| DISCORD[Discord Webhook]
    end
```

### 2つのGCEインスタンスの役割分離とコスト最適化
- **常駐Botインスタンス (`always_free`)**:
  - `e2-micro` / `us-central1-a` / Debian 12 / GCP Always Free（無料枠）。
  - 24時間常駐し、Discordコマンド受信、Cloudflare DDNS更新、外部設定リポジトリからのWebhook受信を担当。
- **Minecraftサーバーインスタンス (`minecraft01`)**:
  - `e2-highcpu-2` / `asia-northeast1-a` / Container-Optimized OS (`cos-stable`) / オンデマンド運用（プレイ中のみ起動）。
  - `e2-micro`で動かさない理由: `e2-micro`（1GB RAM）ではBedrockサーバーがメモリ不足でクラッシュするため、必要なリソースを持つインスタンスをプレイ時のみ起動してコストを抑える。
  - サービスアカウント構成: `always_free`と`minecraft01`の双方にCompute Engineデフォルトサービスアカウント（`cloud-platform`スコープ）を付与し、起動時のGCS自動リストア・バックアップ退避・Cloud Logging出力を行う。

### 動的IP運用とDNS伝播確認
- **静的IPを使わない理由**: Always Freeでは停止中のインスタンスに紐づく未使用静的IPに課金が発生するため、エフェメラル動的外部IPを採用。
- **自己修復DDNS**: 起動時にGCEメタデータサーバー（`Metadata-Flavor: Google`）から外部IPを取得し、Cloudflare APIで`alwaysfree.krmtn.org`（`Proxied: true`）を自動更新。
- **DNS反映確認付き起動通知**:
  `minecraft01`起動時に`minecraft.krmtn.org`（`Proxied: false`）を更新後、公開DNS（`1.1.1.1`）で名前解決の伝播を確認してから起動操作者へ`@メンション`通知を送信し、DNSキャッシュ遅延による接続失敗を防ぐ。

---

## Discord Bot (`apps/minecraft-controller/`) の設計判断

- **実装言語**: Go 1.26。
- **GCE REST API直接呼び出しによる省CPUポーリング**:
  - `e2-micro`上で`gcloud compute instances describe`（Pythonプロセス）を高頻度で起動するとCPU負荷が高騰するため、GCEメタデータサーバーからOAuth2トークンを取得してCompute Engine v1 REST APIを直接呼び出し、インスタンス稼働状態（`status`）と外部IP（`natIP`）を照会する。
  - `minecraft01`が停止中（`TERMINATED`）の間はポーリング間隔を30秒に緩和し、稼働中のストリーム再接続時は5秒間隔で試行する。
- **Discord Gateway WebSocket死活監視（Watchdog）**:
  - 長期稼働中にDiscord GatewayのWebSocket接続が切断されたままプロセスが残存する事象を防ぐため、1分周期で`startDiscordWatchdog`が接続状態とAPI応答（`dg.User("@me")`）を確認する。
  - 切断検知時は再接続を試行し、3回連続で失敗した場合はプロセスを終了して`systemd`（`Restart=always`）による自動再起動へ委譲する。また、シャットダウン時のスラッシュコマンド削除（`ApplicationCommandDelete`）は行わず、登録済みコマンド定義を維持する。
- **操作パネルの呼び出し設計**:
  チャンネルへの常時連投による画面埋め尽くしを防ぐため、`/panel`スラッシュコマンドによる明示的呼び出し時のみ操作パネル（ボタン）を表示。
- **透過的ログ閲覧 (`/logs [lines] [query]`)**:
  - GCEへのSSHを行わずCloud Logging APIを直接照会（フィルタ: `resource.type="gce_instance" AND resource.labels.instance_id="minecraft01"`）。月50GBの無料枠内で運用し、インスタンス停止後も過去ログを閲覧可能。
  - `extractCleanLogMessage`型スイッチ: GCE Syslog（文字列 `textPayload`）とDocker/Fluentd（JSON `jsonPayload`の`message` / `log`キー）の両形式に対応し、ログ本文を型安全に抽出。タイムスタンプは`[YYYY-MM-DD HH:MM:SS]`（JST）で整形し、青色Embedで返信。
- **Ack応答コマンド送信 (`/cmd <command>`)**:
  - Bedrockサーバーのstdoutは他プレイヤーのチャットログと混線しパースが不安定なため、実行結果のパースを行わず送信完了のAckを応答する。
- **10分無人自動シャットダウン ＆ 起動時オンライン人数同期**:
  - ログストリーム監視により、プレイヤー退出で0人になった時点から10分で自動停止タイマー（`triggerEmptyServerTimerLocked`）を起動する。
  - Bot起動時およびストリーム接続確立時に`syncOnlinePlayersDirect`（`send-command "list"`）を実行して実オンライン人数を照合する。プレイヤーが存在する場合はタイマーを解除して再デプロイ時の誤停止を防ぎ、0人であった場合はその時点から10分自動停止タイマーを起動して無人稼働の放置を防ぐ。
- **ストリーミングバックアップ基盤 ＆ GCSバージョニング**:
  - 1時間ごとのサイレント定期実行＋手動`/backup`＋停止時実行。
  - `google/cloud-sdk:alpine`コンテナ標準搭載の`gzip`（`tar -czf -`）による標準出力パイプラインで`gs://${project_id}-minecraft-backup/world-data-kiseki.tar.gz`へ直接ストリーミング保存する。
  - GCSバケットのバージョニングを最新5世代（`num_newer_versions = 5, with_state = "ARCHIVED"`）に制限し、容量超過課金を防止。
- **GitOps Webhook Hot Reloadエンジン**:
  - Cloudflare Proxy（`https://alwaysfree.krmtn.org/webhook`）経由でポート`80/webhook`（環境変数`WEBHOOK_PORT`で変更可）にてHTTP POSTを受信。`X-Hub-Signature-256`によるHMAC-SHA256署名検証（`http.MaxBytesReader` 1MB制限）。
  - GCE上の`/opt/minecraft-controller/config-repo`で`git pull origin main`を実行し、`worlds.yaml`をパース。
  - `defaults.settings`に基づく事前検証を行い、未定義キーや構文エラー検知時はDiscordへエラー通知を送信して中断。
  - 検証通過時はアクティブワールドの設定を`send-command`で稼働中コンテナへ反映（Hot Reload）。サーバー起動時（`/start`完了時）にも自動同期を実行。

---

## マネーフォワードME to BigQuery パイプライン (`apps/moneyforwardme-to-bigquery/`) の設計判断

- **GCEとCloud Runのハイブリッド構成（コスト ＆ メモリ最適化）**:
  - GCE上で直接ブラウザを動かさない理由: 常駐インスタンス`always_free`はメモリ1GB（`e2-micro`）であり、Headless Chromiumを起動するとDiscord Botを巻き込んでOOM（メモリ枯渇）クラッシュするため。
  - Cloud RunのAlways Free枠（月180,000 vCPU秒 / 360,000 GiB秒 / 200万リクエスト）を活用し、ブラウザ処理を外部委譲。
- **スライディングセッション自動延長ループ**:
  - マネーフォワードME（有料プラン）の本体セッション（`_moneybook_session`）は1年間の有効期限を持つ。
  - Cloud Run上の`sync.py`は、CSV取得完了ごとに最新のブラウザストレージ状態（`context.storage_state()`）を取得し、Secret Manager（`mf-session-cookie`）に新しいバージョンとして自動上書き保存。アクセスごとに有効期限が延長され、定期的な手動再認証が不要。
- **行ハッシュ (`row_hash`) ＋ `MERGE` による重複除外アペンド**:
  - クレジットカードの確定遅延や過去明細修正を追従するため、日次定期実行では当月・先月・前々月の計3ヶ月分をエクスポート。
  - 各明細の全カラム値を結合したSHA256ハッシュ（`row_hash`）を生成し、BigQueryの`MERGE`文（`WHEN NOT MATCHED THEN INSERT`）により未変更行をスキップしてデータ重複を防止。
- **GCE `systemd timer` による定期実行とメモリ保護**:
  - 毎朝04:00 JST（`OnCalendar=*-*-* 04:00:00 Asia/Tokyo`）に`systemd timer`で自律起動。
  - `MemoryMax=256M`のcgroupsメモリ上限を設定し、同居するDiscord Botをメモリ圧迫から保護。
  - 実行成否や次回予定時刻は`systemctl list-timers`、詳細ログは`journalctl -u moneyforward-sync.service`で管理。
- **手元PC用 初回ログインスクリプト (`manual_refresh_session.py`)**:
  - PEP 723（インラインスクリプトメタデータ）準拠。手元PCでブラウザを立ち上げて2段階認証（MFA）を人間が突破し、取得されたCookieをSecret Managerへ同期。

---

## インフラ・テスト・CI/CDの設計判断

- **Terraformファイル分割（影響範囲の局所化）**:
  - リプレイス禁止のMinecraftインフラ（`main.tf`）と、マネーフォワード連携（`moneyforwardme-to-bigquery.tf`）を分離。
  - `always_free`の`startup-script`定義もlocals経由で`moneyforwardme-to-bigquery.tf`に集約し、`main.tf`への変更を最小限に抑制。
- **ファイアウォール設定 (`firewall.tf`)**:
  ポート`80/tcp`・`8080/tcp`（Cloudflare経由のGitHub Webhook受信用）、ポート`22/tcp`（GCP IAP `35.235.240.0/20`からのSSH用）、ポート`19132/udp`（Minecraft Bedrockゲーム通信用）、およびVPC内部通信（`10.128.0.0/9`）のINGRESS通信を許可。
- **起動時自動ディザスタリカバリ (`minecraft-startup.sh`)**:
  コンテナ起動時にボリューム内にワールドデータが存在しない場合、GCSバケットから最新のバックアップアーカイブ（`world-data-kiseki.tar.gz`、フォールバックとして`world-data-kiseki.tar.xz`）を自動ダウンロード・解凍して復旧。
- **コンテナログ容量制限**: `max-size=30m, max-file=3`によりディスク容量枯渇を防止。
- **Native / Rootless Podmanテストランナー (`test-terraform.sh`)**:
  - 公式コンテナイメージ（`docker.io/hashicorp/terraform:1.11.4`）をRootless Podman上で実行、またはネイティブの`terraform` CLIで同一のテストを実行可能。
  - リポジトリルートを`/workspace`へマウントし（`fmt -check`時は`:ro`）、`-backend=false`によりホスト改変とクレデンシャル漏洩を遮断しつつ`../apps/`配下のファイルハッシュ参照（`filesha1`）および`override_during = plan`を検証。
- **ネイティブ仕様アサーション (`*.tftest.hcl`)**:
  - `main_test.tftest.hcl`: バックアップバケット、ファイアウォール、デプロイ一時バケット、`minecraft01`のSSHメタデータおよびサービスアカウントスコープの検証（全4件）。
  - `moneyforwardme-to-bigquery_test.tftest.hcl`: BigQueryデータセット・テーブル・重複排除ビュー、Secret Manager、Cloud Runサービス、IAM最小権限、GCE `systemd timer`設定の検証（全5件）。

---

## ローカル開発 ＆ テスト手順 (`Makefile`)

テストターゲットはモジュールごとに分離・独立しています：

```bash
# 全モジュールのテストを一括実行
make test

# モジュール別: Minecraft Controller (Go Bot: 9件)
make test-minecraft-controller

# モジュール別: マネーフォワードME to BigQuery (Python & Shell)
make test-moneyforwardme-to-bigquery
make test-moneyforwardme-to-bigquery-python  # pytest (34件)
make test-moneyforwardme-to-bigquery-shell   # bash (12件)

# モジュール別: インフラ (Terraform 品質ゲート)
make test-infra                             # 全インフラテスト (9件)
make test-infra-minecraft                   # Minecraft & コアインフラテスト (4件)
make test-infra-moneyforwardme-to-bigquery  # マネーフォワードMEインフラテスト (5件)
```

---

## マネーフォワードMEパイプラインの運用手順

### 初回セッションCookie登録（人間作業）
手元PC（ブラウザ操作可能な環境）で以下を実行し、マネーフォワードMEにログインします：
```bash
cd apps/moneyforwardme-to-bigquery
uv run manual_refresh_session.py
```
ブラウザが起動したらログイン（2段階認証含む）を完了させます。家計簿ホーム画面が表示されると自動検知され、最新のCookie JSONがGoogle Cloud Secret Manager（`mf-session-cookie`）に保存されます。

### GCE上での手動バックフィル（過去月一括取得）
過去の任意期間を遡ってBigQueryに蓄積したい場合は、GCE `always_free`上で引数を指定してトリガースクリプトを実行します：
```bash
# 単月のみバックフィル (例: 2024年5月)
/usr/local/bin/trigger-moneyforwardme-to-bigquery.sh 2024-05

# 期間指定バックフィル (例: 2024年1月 〜 2024年12月)
/usr/local/bin/trigger-moneyforwardme-to-bigquery.sh 2024-01 2024-12
```
既存の明細と重複する行は`row_hash`により自動スキップされ、未取得の明細のみがクリーンにアペンドされます。
