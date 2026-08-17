variables {
  project_id       = "test-project-12345"
  allow_list_users = "MockPencil3834,DaftBurrito7340"
}

run "verify_backup_bucket_config" {
  command = plan

  assert {
    condition     = google_storage_bucket.minecraft_backup.name == "test-project-12345-minecraft-backup"
    error_message = "GCS バケット名が仕様と一致しません"
  }

  assert {
    condition     = google_storage_bucket.minecraft_backup.location == "US-CENTRAL1"
    error_message = "GCS バケットのリージョンが US-CENTRAL1 ではありません"
  }

  assert {
    condition     = google_storage_bucket.minecraft_backup.versioning[0].enabled == true
    error_message = "GCS バケットのバージョニングが無効です"
  }
}

run "verify_webhook_firewall_rule" {
  command = plan

  assert {
    condition     = contains(flatten([for a in google_compute_firewall.allow_webhook.allow : a.ports]), "8080")
    error_message = "Webhook ポート 8080 がファイアウォールで許可されていません"
  }
}

run "verify_deploy_temp_bucket_config" {
  command = plan

  assert {
    condition     = google_storage_bucket.deploy_temp.name == "test-project-12345-deploy-temp"
    error_message = "デプロイ一時バケット名が仕様と一致しません"
  }

  assert {
    condition     = tolist(google_storage_bucket.deploy_temp.lifecycle_rule[0].condition)[0].age == 1
    error_message = "デプロイ一時バケットのライフサイクルルールが 1 日に設定されていません"
  }
}

run "verify_minecraft01_ssh_metadata" {
  command = plan

  variables {
    bot_ssh_public_key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMockBotKey"
  }

  assert {
    condition     = google_compute_instance.minecraft01.metadata["enable-oslogin"] == "FALSE"
    error_message = "minecraft01 の enable-oslogin が FALSE に設定されていません"
  }

  assert {
    condition     = google_compute_instance.minecraft01.metadata["ssh-keys"] == "bot:ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMockBotKey"
    error_message = "minecraft01 の ssh-keys に公開鍵が設定されていません"
  }
}
