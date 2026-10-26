data "sfcompute_skus" "all" {}

output "h100_skus" {
  value = [for s in data.sfcompute_skus.all.data : s.id if try(s.accelerator.value, "") == "H100"]
}
