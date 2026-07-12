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

resource "google_compute_firewall" "allow_minecraft_rcon" {
  name    = "allow-minecraft-rcon"
  network = "default"

  allow {
    protocol = "tcp"
    ports    = ["25575"]
  }

  source_ranges = ["10.128.0.50/32"]
  target_tags   = ["minecraft-server"]
  direction     = "INGRESS"
}

resource "google_compute_firewall" "allow_ssh" {
  name    = "allow-ssh-ingress"
  network = "default"

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }

  source_ranges = ["0.0.0.0/0"]
  direction     = "INGRESS"
}
