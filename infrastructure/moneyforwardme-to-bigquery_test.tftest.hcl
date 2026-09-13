variables {
  project_id          = "test-project-12345"
  allow_list_users    = "MockPencil3834,DaftBurrito7340"
  discord_webhook_url = "https://discord.com/api/webhooks/mock/test"
}

run "verify_bigquery_config" {
  command = plan

  assert {
    condition     = google_bigquery_dataset.moneyforward.dataset_id == "moneyforward"
    error_message = "BigQuery データセットIDが moneyforward ではありません"
  }

  assert {
    condition     = google_bigquery_dataset.moneyforward.location == "asia-northeast1"
    error_message = "BigQuery データセットのリージョンが asia-northeast1 ではありません"
  }

  assert {
    condition     = google_bigquery_table.raw_transactions.time_partitioning[0].type == "MONTH"
    error_message = "raw_transactions のパーティショニング種別が MONTH ではありません"
  }

  assert {
    condition     = google_bigquery_table.raw_transactions.time_partitioning[0].field == "transaction_date"
    error_message = "raw_transactions のパーティショニング列が transaction_date ではありません"
  }

  assert {
    condition     = strcontains(google_bigquery_table.v_transactions_latest.view[0].query, "ROW_NUMBER() OVER(PARTITION BY transaction_id ORDER BY loaded_at DESC)")
    error_message = "最新ビューのクエリ定義に ROW_NUMBER による重複除外が含まれていません"
  }
}

run "verify_secret_manager_config" {
  command = plan

  assert {
    condition     = google_secret_manager_secret.mf_session_cookie.secret_id == "mf-session-cookie"
    error_message = "Secret Manager の secret_id が mf-session-cookie ではありません"
  }
}

run "verify_cloud_run_config" {
  command = plan

  assert {
    condition     = google_cloud_run_v2_service.moneyforward_sync.name == "moneyforwardme-to-bigquery"
    error_message = "Cloud Run サービス名が moneyforwardme-to-bigquery ではありません"
  }

  assert {
    condition     = google_cloud_run_v2_service.moneyforward_sync.template[0].scaling[0].max_instance_count == 1
    error_message = "Cloud Run の max_instance_count が 1 に設定されていません（多重スクレイピング防止）"
  }

  assert {
    condition     = google_cloud_run_v2_service.moneyforward_sync.template[0].containers[0].image == "docker.io/cimg/python:3.13-browsers"
    error_message = "Cloud Run コンテナイメージが docker.io/cimg/python:3.13-browsers ではありません"
  }

  assert {
    condition     = google_cloud_run_v2_service.moneyforward_sync.template[0].execution_environment == "EXECUTION_ENVIRONMENT_GEN2"
    error_message = "Cloud Run 実行環境が EXECUTION_ENVIRONMENT_GEN2 ではありません"
  }

  assert {
    condition     = google_cloud_run_v2_service.moneyforward_sync.template[0].timeout == "600s"
    error_message = "Cloud Run タイムアウトが 600s ではありません"
  }

  assert {
    condition     = strcontains(google_cloud_run_v2_service.moneyforward_sync.template[0].containers[0].command[2], "entrypoint.sh")
    error_message = "Cloud Run 起動コマンドに entrypoint.sh が含まれていません"
  }
}

run "verify_iam_permissions" {
  command = plan

  assert {
    condition     = google_secret_manager_secret_iam_member.mf_session_accessor.role == "roles/secretmanager.secretAccessor"
    error_message = "Cloud Run SA に secretAccessor 権限が付与されていません"
  }

  assert {
    condition     = google_secret_manager_secret_iam_member.mf_session_updater.role == "roles/secretmanager.secretVersionAdder"
    error_message = "Cloud Run SA に secretVersionAdder 権限が付与されていません"
  }

  assert {
    condition     = google_bigquery_dataset_iam_member.mf_sync_bq.role == "roles/bigquery.dataEditor"
    error_message = "Cloud Run SA に BigQuery dataEditor 権限が付与されていません"
  }

  assert {
    condition     = google_cloud_run_v2_service_iam_member.gce_invoker.role == "roles/run.invoker"
    error_message = "GCE SA に run.invoker 権限が付与されていません"
  }

  assert {
    condition     = google_cloud_run_v2_service_iam_member.gce_viewer.role == "roles/run.viewer"
    error_message = "GCE SA に run.viewer 権限が付与されていません"
  }
}

run "verify_always_free_systemd_timer" {
  command = plan

  override_resource {
    target          = google_cloud_run_v2_service.moneyforward_sync
    override_during = plan
    values = {
      uri = "https://moneyforwardme-to-bigquery-test.a.run.app"
    }
  }

  assert {
    condition     = strcontains(google_compute_instance.always_free.metadata["startup-script"], "moneyforward-sync.timer")
    error_message = "always_free の startup-script に moneyforward-sync.timer の設定が含まれていません"
  }

  assert {
    condition     = strcontains(google_compute_instance.always_free.metadata["startup-script"], "moneyforward-sync.service")
    error_message = "always_free の startup-script に moneyforward-sync.service の設定が含まれていません"
  }

  assert {
    condition     = strcontains(google_compute_instance.always_free.metadata["startup-script"], "MemoryMax=256M")
    error_message = "always_free の systemd service にメモリ制限 (MemoryMax=256M) が設定されていません"
  }
}


