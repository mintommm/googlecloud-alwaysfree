resource "google_compute_instance" "minecraft01" {
  name         = var.instance_name
  machine_type = "e2-highcpu-2"
  zone         = var.zone
  tags         = ["minecraft-server"]

  boot_disk {
    initialize_params {
      image = "cos-cloud/cos-stable"
      size  = 10
      type  = "pd-balanced"
    }
  }

  network_interface {
    network = "default"
    access_config {
      // エフェメラル外部IP（動的割り当て）
    }
  }

  metadata = {
    startup-script = templatefile("${path.module}/scripts/minecraft-startup.sh", {
      rcon_password    = var.rcon_password
      allow_list_users = var.allow_list_users
    })
  }

  lifecycle {
    ignore_changes = [
      boot_disk[0].initialize_params[0].image,
    ]
  }
}

resource "google_compute_instance" "always_free" {
  name         = var.always_free_name
  machine_type = "e2-micro"
  zone         = var.always_free_zone
  tags         = []

  boot_disk {
    initialize_params {
      size = 30
      type = "pd-standard"
    }
  }

  network_interface {
    network = "default"
    access_config {
      // 既存の外部IP設定を維持
    }
  }

  service_account {
    email  = "381098905316-compute@developer.gserviceaccount.com"
    scopes = ["https://www.googleapis.com/auth/cloud-platform"]
  }

  metadata = {
    startup-script = <<-EOT
      #!/bin/bash
      set -euo pipefail

      # ユーザーが存在しない場合のみ作成
      if ! id -u github-actions >/dev/null 2>&1; then
        useradd -m -s /bin/bash github-actions
      fi

      # systemd --user の永続化（Linger）を有効化
      loginctl enable-linger github-actions

      # SSH公開鍵の配置と権限適正化
      mkdir -p /home/github-actions/.ssh
      chmod 700 /home/github-actions/.ssh
      echo "${var.ssh_public_key}" > /home/github-actions/.ssh/authorized_keys
      chmod 600 /home/github-actions/.ssh/authorized_keys
      chown -R github-actions:github-actions /home/github-actions/.ssh
    EOT
  }

  lifecycle {
    prevent_destroy = true
    ignore_changes = [
      boot_disk[0].initialize_params[0].image,
    ]
  }
}
