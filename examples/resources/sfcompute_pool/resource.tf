# A pool holds purchased compute time; a new pool is empty and free.
resource "sfcompute_pool" "train" {
  name = "train"
  tags = {
    team = "ml"
  }
  # workspace defaults to the provider's workspace.
}
