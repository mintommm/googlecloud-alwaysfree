# Always Free Google Cloud Infrastructure & Automation

Google Cloud Platform (GCP) の Always Free（無料枠）インスタンス上で常駐稼働する Discord コントローラー Bot (`apps/minecraft-controller`)、マネーフォワードME 家計簿明細の自動蓄積パイプライン (`apps/moneyforwardme-to-bigquery`)、およびそれらを支えるインフラ定義（Terraform）を管理するモノレポリポジトリ。

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
        BOT -->|Webhook リッスン :8080| GH_CONFIG[設定リポジトリ Webhook]
        BOT -->|1時間サイレントバックアップ| GCS[GCS バケット (5世代バージョニング)]
        BOT -->|IAP / SSH 制御| GCE_MC[minecraft01 (Bedrock Server: e2-highcpu-2)]
    end

    subgraph MONEYFORWARD_SYS ["マネーフォワードME 自動蓄積パイプライン"]
        TRIGGER -->|POST / (OIDC IDトークン認証)| RUN["Cloud Run (apps/moneyforwardme-to-bigquery)<br/>Playwright / Python 3.13"]
        RUN -->|セッション取得・自動延長保存| SM["Secret Manager<br/>(mf-session-cookie)"]
        RUN -->|CSVダウンロード (当月・先月・前々月)| MF[マネーフォワードME]
        RUN -->|row_hash による MERGE アペンド| BQ["BigQuery (moneyforward dataset)<br/>raw_transactions / v_transactions_latest"]
        RUN -->|実行結果通知| DISCORD[Discord Webhook]
    end
```

### 2 つの GCE インスタンスの役割分離とコスト最適化
- **常駐 Bot インスタンス (`always_free`)**:
  - `e2-micro` / `us-central1-a` / Debian 12 / GCP Always Free（完全無料 $0.00）。
  - 24時間365日常駐し、Discord コマンド受信、Cloudflare DDNS 更新、外部設定リポジトリからの Webhook 受信を担当。
- **Minecraft サーバーインスタンス (`minecraft01`)**:
  - `e2-highcpu-2` / `asia-northeast1-a` / Debian 12 / オンデマンド運用（プレイ中のみ起動）。
  - なぜ e2-micro で動かさないのか: e2-micro（1GB RAM）では Bedrock サーバーがメモリ不足でクラッシュするため、十分なリソースを持つインスタンスを必要な時だけ起動しコストを最小化する。

### 動的 IP 運用と DNS 自動伝播確認
- **静的 IP を使わない理由**: Always Free では停止中のインスタンスに紐づく未使用静的 IP に課金が発生するため、動的外部 IP を採用。
- **自己修復 DDNS**: 起動時に GCE メタデータサーバー（`Metadata-Flavor: Google`）から外部 IP を取得し、Cloudflare API で `alwaysfree.krmtn.org` (Proxied: true) を自動更新。
- **DNS 反映確認付き起動通知**:
  `minecraft01` 起動時に `minecraft.krmtn.org` (Proxied: false) を更新後、直ちに通知せず公開 DNS（1.1.1.1 / 8.8.8.8）で名前解決の伝播を確認してから起動操作者へ `@メンション` 通知を送信。DNS キャッシュ遅延による接続失敗を完全防止。

---

## Discord Bot (`apps/minecraft-controller/`) の非自明な設計判断

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

## マネーフォワードME to BigQuery パイプライン (`apps/moneyforwardme-to-bigquery/`) の非自明な設計判断

- **GCE と Cloud Run のハイブリッド構成（コスト ＆ メモリ最適化）**:
  - なぜ GCE 上で直接ブラウザを動かさないのか: 常駐インスタンス `always_free` はメモリ 1GB（e2-micro）であり、Headless Chromium を起動すると Discord Bot を巻き込んで OOM（メモリ枯渇）クラッシュするため。
  - Cloud Run の Always Free 枠（月 180,000 vCPU 秒 / 360,000 GiB 秒 / 200 万リクエスト）を活用し、ブラウザ処理を完全に外部委譲。追加費用 $0.00 を死守。
- **スライディングセッション自動延長ループ（半永久自律稼働）**:
  - マネーフォワードME（有料プラン）の本体セッション（`_moneybook_session`）は 1 年間の有効期限を持つ。
  - Cloud Run 上の `sync.py` は、CSV 取得完了ごとに最新のブラウザストレージ状態（`context.storage_state()`）を取得し、Secret Manager（`mf-session-cookie`）に新しいバージョンとして自動上書き保存。アクセスごとに有効期限が延長され、定期的な手動再認証が不要。
- **行ハッシュ (`row_hash`) ＋ `MERGE` による重複除外アペンド**:
  - クレジットカードの確定遅延や過去明細修正を漏れなく追従するため、日次定期実行では当月・先月・前々月の「計 3 ヶ月分」を常にエクスポート。
  - 各明細の全カラム値を結合した SHA256 ハッシュ（`row_hash`）を生成し、BigQuery の `MERGE` 文（`WHEN NOT MATCHED THEN INSERT`）により未変更行を完全にスキップ。データ重複やテーブル肥大化を永久防止。
- **GCE `systemd timer` による定期実行とメモリ保護**:
  - 毎朝 04:00 JST（`OnCalendar=*-*-* 04:00:00 Asia/Tokyo`）に `systemd timer` で自律起動。
  - `MemoryMax=256M` の cgroups メモリ上限を設定し、同居する Discord Bot を万が一の暴走から完全保護。
  - 実行成否や次回予定時刻は `systemctl list-timers`、詳細ログは `journalctl -u moneyforward-sync.service` で一元管理。
- **手元 PC 用 初回ログインスクリプト (`manual_refresh_session.py`)**:
  - PEP 723（インラインスクリプトメタデータ）準拠。手元 PC でブラウザを立ち上げて 2 段階認証（MFA）を人間が突破し、取得された Cookie を Secret Manager へ一括同期。

---

## インフラ・テスト・CI/CD の非自明な設計判断

- **Terraform ファイル分割（ブラスト半径の最小化）**:
  - 稼働中かつリプレイス厳禁の Minecraft インフラ（`main.tf`）と、新設のマネーフォワード連携（`moneyforwardme-to-bigquery.tf`）を分離。
  - `always_free` の `startup-script` 定義も locals 経由で `moneyforwardme-to-bigquery.tf` に集約し、`main.tf` への変更を最小限（1 行参照）に抑制。
- **ファイアウォール設定 (`firewall.tf`)**:
  ポート `8080/tcp`（GitHub Webhook 受信用）およびポート `19132/udp`（Minecraft Bedrock ゲーム通信用）の INGRESS 通信を許可。
- **起動時自動ディザスタリカバリ (`minecraft-startup.sh`)**:
  コンテナ起動時にボリューム内にワールドデータが存在しない場合、GCS バケットから最新のバックアップアーカイブ（`world-data-kiseki.tar.xz`）を自動ダウンロード・解凍して復旧。
- **コンテナログ容量制限**: `max-size=30m, max-file=3` によりディスク容量枯渇クラッシュを防止。
- **Native / Rootless Podman テストランナー (`test-terraform.sh`)**:
  - ネイティブの `terraform` CLI、またはコンテナ環境（Podman）のどちらでも同一のテストを実行可能。
  - 読み取り専用マウント（`:ro`）、`-backend=false` によりホスト改変とクレデンシャル漏洩を完全遮断。
- **ネイティブ仕様アサーション (`*.tftest.hcl`)**:
  - `main_test.tftest.hcl`: バックアップバケット、ファイアウォール、一時バケット、SSH メタデータの検証（全 4 件）。
  - `moneyforwardme-to-bigquery_test.tftest.hcl`: BigQuery データセット・テーブル・重複排除ビュー、Secret Manager、Cloud Run サービス、IAM 最小権限、GCE systemd timer 設定の検証（全 5 件）。

---

## ローカル開発 ＆ テスト手順 (`Makefile`)

テストターゲットはモジュールごとに完全に分離・独立しています：

```bash
# 全モジュールのテストを一括実行
make test

# モジュール別: Minecraft Controller (Go Bot)
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

## マネーフォワードME パイプラインの運用手順

### 初回セッション Cookie 登録（人間作業）
手元 PC（ブラウザ操作可能な環境）で以下を実行し、マネーフォワードMEにログインします：
```bash
cd apps/moneyforwardme-to-bigquery
uv run manual_refresh_session.py
```
ブラウザが起動したらログイン（2段階認証含む）を完了させます。家計簿ホーム画面が表示されると自動検知され、最新の Cookie JSON が Google Cloud Secret Manager（`mf-session-cookie`）に保存されます。

### GCE 上での手動バックフィル（過去月一括取得）
過去の任意期間を遡って BigQuery に蓄積したい場合は、GCE `always_free` 上で引数を指定してトリガースクリプトを実行します：
```bash
# 単月のみバックフィル (例: 2024年5月)
/usr/local/bin/trigger-moneyforwardme-to-bigquery.sh 2024-05

# 期間指定バックフィル (例: 2024年1月 〜 2024年12月)
/usr/local/bin/trigger-moneyforwardme-to-bigquery.sh 2024-01 2024-12
```
既存の明細と重複する行は `row_hash` により自動スキップされ、未取得の明細のみがクリーンにアペンドされます。
