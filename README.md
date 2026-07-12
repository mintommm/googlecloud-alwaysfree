Secret Managerの導入に伴い、認証情報のローカルディスクへの平文保存を完全に排除し、安全にローカル検証（`terraform plan`/`apply`）を行うための運用手順を統合した最新の `README.md` である。

プロジェクトのルート直下の `README.md` を以下の内容で更新する。

---

### `README.md`（最新版）

```markdown
---
project: discord-integrated-minecraft-server
architecture: gcp-terraform-github-actions-golang
ssot: github-repository
credentials_management: gcp-secret-manager
last_updated: 2026-07-12
---

# Discord連携マイクラサーバーGCE構築プロジェクト

## 概要
本プロジェクトは、Google Cloud Platform（GCP）上に構築されたマインクラフト（Bedrock版）ゲームサーバー、およびそれを制御するDiscord Bot環境を、Terraformによるインフラのコード化（IaC）とGitHub ActionsによるCI/CDパイプラインを用いて自動管理するマルチモジュール（モノレポ）リポジトリである。

## 単一の正（SSOT）の定義
本環境におけるすべての「あるべき姿」は、本GitHubリポジトリにコミットされたコードを正（Single Source of Truth）とする。
- GCP上の物理状態（State）はGCSバケット（`tf-mintommm-alwaysfree-gce`）で排他ロック管理されるが、構成定義の決定権はリポジトリのコードに帰属する。
- 実行環境（Cloud Shell等）への個別カスタマイズは排除し、ツール類は都度インストールを行う。
- リモートリポジトリへの秘密情報の流出を完全に防ぐため、ローカル環境での認証情報の平文保存（`terraform.tfvars` 等への記述）は禁止とする。

## ディレクトリ構造
```text
.
├── .github/
│   └── workflows/
│       ├── deploy-infra.yml          # インフラ自動適用（Terraform）
│       └── deploy-minecraft.yml      # 制御Bot自動適用（Go）
├── .gitignore                        # 各種除外定義（*.tfvars, Goバイナリ等）
├── README.md                         # 本ファイル
├── go.work                           # Go Workspace（モノレポ統括設定）
├── infrastructure/                   # インフラ構成定義（Terraform）
│   ├── backend.tf
│   ├── firewall.tf
│   ├── main.tf
│   ├── provider.tf
│   ├── variables.tf
│   └── scripts/
│       └── minecraft-startup.sh      # GCEホスト側スタートアップスクリプト
├── shared/                           # 共通モジュール領域
│   └── go.mod
└── apps/                             # アプリケーションコード
    └── minecraft-controller/         # マイクラ制御Bot（Goモジュール）
        ├── go.mod
        ├── go.sum
        └── main.go

```

## システム仕様

### 1. 制御用メインインスタンス（always-free）

* **マシンタイプ**: `e2-micro` / `us-central1-a` ゾーン / 30GB 標準永続ディスク（GCP無料枠対象）
* **認証権限**: `381098905316-compute@developer.gserviceaccount.com`（アクセススコープ: `https://www.googleapis.com/auth/cloud-platform`）を保持。インプレース更新による破壊は禁止。

### 2. ゲームサーバー用インスタンス（minecraft01）

* **マシンタイプ**: `e2-highcpu-2` / `asia-northeast1-a` ゾーン / 10GB バランス永続ディスク
* **実行環境**: Container-Optimized OS（COS）によるDockerコンテナ運用
* **データ定義**: メタデータ（`startup-script`）をインプレース更新可能な構造で保持。環境変数 `LEVEL_NAME="Kiseki"` を指定し、既存のワールドデータを永続ボリューム（`minecraft-data`）経由でロード。
* **アクセス制御**: `ALLOW_LIST=true` を適用し、指定された特定の4ユーザー（`MockPencil3834`, `DaftBurrito7340`, `superkurute`, `StaticEar839559`）のみ接続を許可。

### 3. アプリケーション（minecraft-controller）

* **開発言語**: Go言語 (Golang v1.22)
* **主要機能**:
* Discordのインタラクション（スラッシュコマンド `/panel` およびボタンUI）によるGCEインスタンスの起動・停止制御。
* インスタンス起動検知時のポーリング制御、および外部IPの動的取得。
* Cloudflare APIを介したAレコードの自動更新（DDNS機能、UDP透過のための `Proxied=false` 設定）。



---

## 開発・運用手順

### 認証情報の管理について（GCP Secret Manager）

RCONパスワードなどの機密情報は、GCPの「Secret Manager」を利用して一元管理し、Always Free（無料枠）の範囲内で安全に参照を行う。

#### 1. シークレットの初回登録・更新（Cloud Shell等での実行）

```bash
# シークレットの作成（未作成の場合のみ）
gcloud secrets create minecraft-rcon-password --replication-policy="automatic"

# パスワード値の登録・変更（バージョン追加）
echo -n "実際のRCONパスワード文字列" | gcloud secrets versions add minecraft-rcon-password --data-file=-

```

#### 2. ローカル検証（terraform plan / apply）の実行手順

ファイルにパスワードを永続化させず、コマンド実行時のみ環境変数を動的に注入して整合性を検証する。`infrastructure/` ディレクトリに移動した上で、以下のコマンドを実行する。

```bash
# 実行計画の確認（Plan）
TF_VAR_rcon_password=$(gcloud secrets versions access latest --secret="minecraft-rcon-password") terraform plan

# 実行計画の適用（Apply）
TF_VAR_rcon_password=$(gcloud secrets versions access latest --secret="minecraft-rcon-password") terraform apply -auto-approve

```
