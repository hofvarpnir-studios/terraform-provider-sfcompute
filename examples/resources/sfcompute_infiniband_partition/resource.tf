resource "sfcompute_infiniband_partition" "training" {
  name              = "training"
  region            = "europe-north1"
  availability_zone = "a"
}
