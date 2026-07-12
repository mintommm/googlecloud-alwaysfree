---
project: discord-integrated-minecraft-server
architecture: gcp-terraform-github-actions-golang
ssot: github-repository
last_updated: 2026-07-12
---

# Discord連携マイクラサーバーGCE構築プロジェクト

## 概要
本プロジェクトは、Google Cloud Platform（GCP）上に構築されたマインクラフト（Bedrock版）ゲームサーバー、およびそれを制御するDiscord Bot環境を、Terraformによるインフラのコード化（IaC）とGitHub ActionsによるCI/CDパイプラインを用いて自動管理するマルチモジュール（モノレポ）リポジトリである。

## 単一の正（SSOT）の定義
本環境におけるすべての「あるべき姿」は、本GitHubリポジトリにコミットされたコードを正（Single Source of Truth）とする。
- GCP上の物理状態（State）はGCSバケット（`tf-mintommm-alwaysfree-gce`）で排他ロック管理されるが、構成定義の決定権はリポジトリのコードに帰属する。
- 実行環境（Cloud Shell等）への個別カスタマイズは排除し、ツール類は都度インストールを行う。

## ディレクトリ構造
```text
.
├── .github/
│   └── workflows/
│       ├── deploy-infra.yml          # インフラ自動適用（Terraform）
│       └── deploy-minecraft.yml      # 制御Bot自動適用（Go）
├── .gitignore                        # 各種除外定義
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
