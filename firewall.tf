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
