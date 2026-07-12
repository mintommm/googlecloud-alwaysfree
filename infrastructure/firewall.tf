resource "google_compute_firewall" "allow_minecraft_bedrock" {
  name    = "allow-minecraft-bedrock"
  network = "default"

  allow {
    protocol = "udp"
    ports    = ["19132"]
  }

  source_ranges = ["0.0.0.0/0"]
  target_tags   = ["minecraft-server"]
  direction     = "INGRESS"
}

resource "google_compute_firewall" "allow_minecraft_rcon_internal" {
  name    = "allow-minecraft-rcon-internal"
  network = "default"

  allow {
    protocol = "tcp"
    ports    = ["25575"]
  }

  source_ranges = ["10.0.0.0/8"]
  target_tags   = ["minecraft-server"]
  direction     = "INGRESS"
}

resource "google_compute_firewall" "allow_ssh" {
  name    = "allow-ssh-iap"
  network = "default"

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }

  source_ranges = ["35.235.240.0/20"]
  direction     = "INGRESS"
}
