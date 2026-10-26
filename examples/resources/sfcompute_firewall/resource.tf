resource "sfcompute_firewall" "ssh_and_https" {
  name = "ssh-and-https"
  rules = [
    { direction = "ingress", protocol = "tcp", port = "22", source = "203.0.113.0/24" },
    # source defaults to 0.0.0.0/0.
    { direction = "ingress", protocol = "tcp", port = "443" },
  ]
}
