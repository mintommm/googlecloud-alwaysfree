resource "google_compute_instance" "always_free_bot" {
  name         = var.always_free_name
  machine_type = "e2-micro"
  zone         = var.always_free_zone

  tags = ["discord-bot"]

  boot_disk {
    initialize_params {
      image = "debian-cloud/debian-12"
      size  = 30
      type  = "pd-standard"
    }
  }

  network_interface {
    network = "default"
    access_config {}
  }

  metadata = {
    block-project-ssh-keys = "false"
  }

  scheduling {
    preemptible       = false
    automatic_restart = true
  }
}

resource "google_compute_instance" "minecraft_server" {
  name         = var.instance_name
  machine_type = "e2-medium"
  zone         = var.zone

  tags = ["minecraft-server"]

  boot_disk {
    initialize_params {
      image = "debian-cloud/debian-12"
      size  = 20
      type  = "pd-balanced"
    }
  }

  network_interface {
    network = "default"
    access_config {}
  }

  metadata = {
    block-project-ssh-keys = "false"
  }

  scheduling {
    preemptible       = false
    automatic_restart = false
  }
}
