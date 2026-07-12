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
      image = "debian-cloud/debian-12"
      size  = 30
      type  = "pd-standard"
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
    # GCPベストプラクティス：OS Loginをプロジェクト統合モードで強制有効化
    enable-oslogin = "TRUE"
  }

  lifecycle {
    prevent_destroy = true
    ignore_changes = [
      boot_disk[0].initialize_params[0].image,
    ]
  }
}
