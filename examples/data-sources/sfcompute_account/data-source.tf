data "sfcompute_account" "me" {}

output "account" {
  value = data.sfcompute_account.me.account_id
}
