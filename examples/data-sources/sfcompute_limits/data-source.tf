data "sfcompute_limits" "current" {}

output "limits" {
  value = { for l in data.sfcompute_limits.current.data : l.name => "${l.usage} / ${l.value}" }
}
