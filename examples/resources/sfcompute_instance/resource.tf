# Requires allow_spending = true in the provider block: a running instance
# draws on its pool's purchased compute allocation.
data "sfcompute_skus" "all" {}

resource "sfcompute_pool" "dev" {
  name = "dev"
}

resource "sfcompute_instance" "devbox" {
  name                 = "devbox"
  pool                 = sfcompute_pool.dev.id
  sku                  = data.sfcompute_skus.all.data[0].id
  image                = "sfc:image:sfcompute:public:ubuntu-24.04-cuda-13.2"
  cloud_init_user_data = base64encode(file("${path.module}/cloud-init.yaml"))
  priority_level       = "preferred"
}

data "sfcompute_instance_ssh" "devbox" {
  id = sfcompute_instance.devbox.id
}

output "ssh" {
  value = "ssh -p ${data.sfcompute_instance_ssh.devbox.port} ubuntu@${data.sfcompute_instance_ssh.devbox.hostname}"
}
