resource "google_compute_instance" "minecraft01" {
  name                      = var.instance_name
  machine_type              = "e2-highcpu-2"
  zone                      = var.zone
  tags                      = ["minecraft-server"]
  allow_stopping_for_update = true

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
      // エフェメラル外部IP
    }
  }

  metadata = {
    enable-oslogin = "FALSE"
    ssh-keys       = var.bot_ssh_public_key != "" ? "bot:${var.bot_ssh_public_key}" : ""
    startup-script = templatefile("${path.module}/scripts/minecraft-startup.sh", {
      allow_list_users = var.allow_list_users
    })
  }

  lifecycle {
    prevent_destroy = true
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
    enable-oslogin = "TRUE"
  }

  lifecycle {
    prevent_destroy = true
    ignore_changes = [
      boot_disk[0].initialize_params[0].image,
    ]
  }
}

resource "google_storage_bucket" "minecraft_backup" {
  name          = "${var.project_id}-minecraft-backup"
  location      = "US-CENTRAL1"
  storage_class = "STANDARD"

  versioning {
    enabled = true
  }

  lifecycle_rule {
    action {
      type = "Delete"
    }
    condition {
      num_newer_versions = 5
      with_state         = "ARCHIVED"
    }
  }

  uniform_bucket_level_access = true
}

resource "google_storage_bucket" "deploy_temp" {
  name                        = "${var.project_id}-deploy-temp"
  location                    = "US-CENTRAL1"
  storage_class               = "STANDARD"
  uniform_bucket_level_access = true
  force_destroy               = true

  lifecycle_rule {
    action {
      type = "Delete"
    }
    condition {
      age = 1
    }
  }
}
