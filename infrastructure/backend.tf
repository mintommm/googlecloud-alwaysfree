terraform {
  backend "gcs" {
    bucket = "tf-mintommm-alwaysfree-gce"
    prefix = "terraform/state/minecraft"
  }
}
