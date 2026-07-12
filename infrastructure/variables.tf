variable "project_id" {
  type    = string
  default = "mintommm-alwaysfree-gce"
}

variable "region" {
  type    = string
  default = "asia-northeast1"
}

variable "zone" {
  type    = string
  default = "asia-northeast1-a"
}

variable "instance_name" {
  type    = string
  default = "minecraft01"
}

variable "always_free_zone" {
  type    = string
  default = "us-central1-a"
}

variable "always_free_name" {
  type    = string
  default = "always-free"
}

variable "rcon_password" {
  type      = string
  sensitive = true
}

variable "allow_list_users" {
  type      = string
  sensitive = true
}

variable "ssh_public_key" {
  type      = string
  sensitive = true
}
