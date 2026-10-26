# Requires allow_spending = true in the provider block. This places a real
# order on the SF Compute market; credits are spent as soon as it fills.
# Destroying the resource cancels whatever has not filled yet.
resource "sfcompute_pool" "dev" {
  name = "dev"
}

resource "sfcompute_order" "two_hours" {
  pool                              = sfcompute_pool.dev.id
  side                              = "buy"
  sku                               = "sku_4UpxzQw7A8N"
  allow_standing                    = true
  allow_partial                     = false
  limit_price_dollars_per_node_hour = "20.00"
  allocation_schedule_delta = [
    # Unix timestamps on the allocation schedule grid
    # (see data source sfcompute_platform_constants).
    { start_at = 1767225600, end_at = 1767232800, node_count = 1 },
  ]
}
