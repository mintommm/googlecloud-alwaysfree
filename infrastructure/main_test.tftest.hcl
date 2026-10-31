variables {
  project_id       = "test-project-12345"
  allow_list_users = "PlayerOne,PlayerTwo"
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

  assert {
    condition     = contains(google_compute_instance.minecraft01.service_account[0].scopes, "https://www.googleapis.com/auth/cloud-platform")
    error_message = "minecraft01 に cloud-platform スコープの service_account が設定されていません"
  }
}

run "verify_minecraft_bedrock_network_config" {
  command = plan

  assert {
    condition     = contains([for a in google_compute_firewall.allow_minecraft_bedrock.allow : a.protocol if contains(a.ports, "19132")], "tcp")
    error_message = "Minecraft Bedrock の TCP 19132 ポートがファイアウォールで許可されていません"
  }

  assert {
    condition     = contains([for a in google_compute_firewall.allow_minecraft_bedrock.allow : a.protocol if contains(a.ports, "19132") && contains(a.ports, "19140-19155")], "udp")
    error_message = "Minecraft Bedrock の UDP 19132 および 19140-19155 ポートがファイアウォールで許可されていません"
  }

  assert {
    condition     = strcontains(google_compute_instance.minecraft01.metadata["startup-script"], "-p tcp --dport 19132 -j ACCEPT") && strcontains(google_compute_instance.minecraft01.metadata["startup-script"], "-p udp --dport 19140:19155 -j ACCEPT")
    error_message = "startup-script にホスト iptables の TCP 19132 および UDP 19140:19155 許可設定が含まれていません"
  }

  assert {
    condition     = strcontains(google_compute_instance.minecraft01.metadata["startup-script"], "EXTERNAL_IP=$(curl -fsSL -H \"Metadata-Flavor: Google\" \"http://metadata.google.internal/computeMetadata/v1/instance/network-interfaces/0/access-configs/0/external-ip\")")
    error_message = "startup-script に GCE メタデータサーバーからの EXTERNAL_IP 取得設定が含まれていません"
  }

  assert {
    condition     = strcontains(google_compute_instance.minecraft01.metadata["startup-script"], "-p 19132:19132/tcp") && strcontains(google_compute_instance.minecraft01.metadata["startup-script"], "-p 19140-19155:19140-19155/udp")
    error_message = "startup-script に NetherNet 用の Docker ポート公開設定が含まれていません"
  }

  assert {
    condition     = strcontains(google_compute_instance.minecraft01.metadata["startup-script"], "-e TRANSPORT=nethernet") && strcontains(google_compute_instance.minecraft01.metadata["startup-script"], "-e SERVER_UDP_PORTS=\"$EXTERNAL_IP:19140-19155:19140-19155\"")
    error_message = "startup-script に TRANSPORT=nethernet および SERVER_UDP_PORTS 設定が含まれていません"
  }
}
