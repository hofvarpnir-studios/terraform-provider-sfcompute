# All pools in the provider's workspace (every page is fetched).
data "sfcompute_pools" "all" {}

output "pool_names" {
  value = data.sfcompute_pools.all.data[*].name
}
