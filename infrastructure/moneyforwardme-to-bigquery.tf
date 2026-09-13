resource "google_bigquery_dataset" "moneyforward" {
  dataset_id  = "moneyforward"
  description = "マネーフォワードME 家計簿明細データセット"
  location    = "asia-northeast1"
}

resource "google_bigquery_table" "raw_transactions" {
  dataset_id          = google_bigquery_dataset.moneyforward.dataset_id
  table_id            = "raw_transactions"
  deletion_protection = false

  time_partitioning {
    type  = "MONTH"
    field = "transaction_date"
  }

  schema = jsonencode([
    { name = "is_calculation_target", type = "BOOLEAN", mode = "REQUIRED" },
    { name = "transaction_date", type = "DATE", mode = "REQUIRED" },
    { name = "content", type = "STRING", mode = "REQUIRED" },
    { name = "amount", type = "INTEGER", mode = "REQUIRED" },
    { name = "financial_institution", type = "STRING", mode = "NULLABLE" },
    { name = "large_category", type = "STRING", mode = "NULLABLE" },
    { name = "middle_category", type = "STRING", mode = "NULLABLE" },
    { name = "memo", type = "STRING", mode = "NULLABLE" },
    { name = "transfer_flag", type = "BOOLEAN", mode = "REQUIRED" },
    { name = "transaction_id", type = "STRING", mode = "REQUIRED" },
    { name = "row_hash", type = "STRING", mode = "REQUIRED" },
    { name = "loaded_at", type = "TIMESTAMP", mode = "NULLABLE", defaultValueExpression = "CURRENT_TIMESTAMP()" }
  ])
}

resource "google_bigquery_table" "v_transactions_latest" {
  dataset_id = google_bigquery_dataset.moneyforward.dataset_id
  table_id   = "v_transactions_latest"

  view {
    query          = <<-SQL
      SELECT * EXCEPT(row_num)
      FROM (
        SELECT
          *,
          ROW_NUMBER() OVER(PARTITION BY transaction_id ORDER BY loaded_at DESC) as row_num
        FROM `${var.project_id}.${google_bigquery_dataset.moneyforward.dataset_id}.${google_bigquery_table.raw_transactions.table_id}`
      )
      WHERE row_num = 1
    SQL
    use_legacy_sql = false
  }

  depends_on = [google_bigquery_table.raw_transactions]
}

resource "google_secret_manager_secret" "mf_session_cookie" {
  secret_id = "mf-session-cookie"
  replication {
    auto {}
  }
}

resource "google_service_account" "mf_sync" {
  account_id   = "moneyforward-sync-sa"
  display_name = "MoneyForward Sync Cloud Run Service Account"
}

resource "google_secret_manager_secret_iam_member" "mf_session_accessor" {
  secret_id = google_secret_manager_secret.mf_session_cookie.secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.mf_sync.email}"
}

resource "google_secret_manager_secret_iam_member" "mf_session_updater" {
  secret_id = google_secret_manager_secret.mf_session_cookie.secret_id
  role      = "roles/secretmanager.secretVersionAdder"
  member    = "serviceAccount:${google_service_account.mf_sync.email}"
}

resource "google_bigquery_dataset_iam_member" "mf_sync_bq" {
  dataset_id = google_bigquery_dataset.moneyforward.dataset_id
  role       = "roles/bigquery.dataEditor"
  member     = "serviceAccount:${google_service_account.mf_sync.email}"
}

resource "google_cloud_run_v2_service" "moneyforward_sync" {
  name     = "moneyforwardme-to-bigquery"
  location = var.region

  template {
    service_account       = google_service_account.mf_sync.email
    timeout               = "600s"
    execution_environment = "EXECUTION_ENVIRONMENT_GEN2"

    scaling {
      # Prevent concurrent scraping sessions from colliding or getting rate-limited by MoneyForward
      max_instance_count = 1
    }

    containers {
      # Use CircleCI browsers image to consume 0 bytes of Artifact Registry storage (Always Free)
      image = "docker.io/cimg/python:3.13-browsers"

      ports {
        container_port = 8080
      }

      command = [
        "/bin/bash",
        "-c",
        "git clone --depth 1 --branch ${var.github_branch} https://github.com/${var.github_repository}.git /tmp/repo && bash /tmp/repo/apps/moneyforwardme-to-bigquery/entrypoint.sh"
      ]

      resources {
        limits = {
          cpu    = "1000m"
          memory = "1024Mi"
        }
      }

      env {
        name  = "GITHUB_REPOSITORY"
        value = var.github_repository
      }
      env {
        name  = "GITHUB_BRANCH"
        value = var.github_branch
      }
      env {
        name  = "PROJECT_ID"
        value = var.project_id
      }
      env {
        name  = "SECRET_ID"
        value = google_secret_manager_secret.mf_session_cookie.secret_id
      }
      env {
        name  = "DATASET_ID"
        value = google_bigquery_dataset.moneyforward.dataset_id
      }
      env {
        name  = "TABLE_ID"
        value = google_bigquery_table.raw_transactions.table_id
      }
      env {
        name  = "DISCORD_WEBHOOK_URL"
        value = var.discord_webhook_url
      }
    }
  }
}

resource "google_cloud_run_v2_service_iam_member" "gce_invoker" {
  name     = google_cloud_run_v2_service.moneyforward_sync.name
  location = google_cloud_run_v2_service.moneyforward_sync.location
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_compute_instance.always_free.service_account[0].email}"
}

resource "google_cloud_run_v2_service_iam_member" "gce_viewer" {
  name     = google_cloud_run_v2_service.moneyforward_sync.name
  location = google_cloud_run_v2_service.moneyforward_sync.location
  role     = "roles/run.viewer"
  member   = "serviceAccount:${google_compute_instance.always_free.service_account[0].email}"
}

locals {
  moneyforward_systemd_service = <<-EOF
    [Unit]
    Description=MoneyForward ME to BigQuery Daily Sync
    Wants=network-online.target
    After=network-online.target

    [Service]
    Type=oneshot
    Environment="SERVICE_URL=${google_cloud_run_v2_service.moneyforward_sync.uri}"
    ExecStart=/usr/local/bin/trigger-moneyforwardme-to-bigquery.sh
    # Limit memory to protect the collocated Discord bot on the 1GB e2-micro instance
    MemoryMax=256M
  EOF

  moneyforward_systemd_timer = <<-EOF
    [Unit]
    Description=MoneyForward ME to BigQuery Daily Sync Timer

    [Timer]
    OnCalendar=*-*-* 04:00:00 Asia/Tokyo
    Persistent=true

    [Install]
    WantedBy=timers.target
  EOF

  always_free_startup_script = <<-EOF
    #!/bin/bash
    set -euo pipefail

    # 1. Download latest trigger script from GitHub
    curl -sSL "https://raw.githubusercontent.com/${var.github_repository}/${var.github_branch}/apps/moneyforwardme-to-bigquery/trigger-moneyforwardme-to-bigquery.sh" -o /usr/local/bin/trigger-moneyforwardme-to-bigquery.sh
    chmod +x /usr/local/bin/trigger-moneyforwardme-to-bigquery.sh

    # 2. Configure systemd service and timer
    cat << 'SERVICE_EOF' > /etc/systemd/system/moneyforward-sync.service
${local.moneyforward_systemd_service}
SERVICE_EOF

    cat << 'TIMER_EOF' > /etc/systemd/system/moneyforward-sync.timer
${local.moneyforward_systemd_timer}
TIMER_EOF

    # 3. Reload systemd and enable timer
    systemctl daemon-reload
    systemctl enable --now moneyforward-sync.timer
  EOF
}

