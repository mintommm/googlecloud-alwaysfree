# ゲームサーバー用インスタンス（オンデマンド稼働）
resource "google_compute_instance" "minecraft01" {
  name         = var.instance_name
  machine_type = "e2-highcpu-2"
  zone         = var.zone

  tags = ["minecraft-server"]

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
      // エフェメラル外部IP（起動ごとに動的割り当て）
    }
  }

  # テンプレート関数を使用してRCONパスワードをスクリプトへ注入
  metadata_startup_script = templatefile("${path.module}/scripts/minecraft-startup.sh", {
    rcon_password = var.rcon_password
  })

  lifecycle {
    ignore_changes = [
      boot_disk[0].initialize_params[0].image,
    ]
  }
}

# 制御用メインインスタンス（Always Free対象・常時稼働）
resource "google_compute_instance" "always_free" {
  name         = var.always_free_name
  machine_type = "e2-micro"
  zone         = var.always_free_zone

  tags = []

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

  # インポート時のサービスアカウントおよびAPIアクセススコープの剥奪を防ぐための定義
  service_account {
    email  = "381098905316-compute@developer.gserviceaccount.com"
    scopes = ["https://www.googleapis.com/auth/cloud-platform"]
  }

  lifecycle {
    prevent_destroy = true
    ignore_changes = [
      boot_disk[0].initialize_params[0].image,
    ]
  }
}
