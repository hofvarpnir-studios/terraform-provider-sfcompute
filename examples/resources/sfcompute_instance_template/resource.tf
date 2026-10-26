resource "sfcompute_instance_template" "worker" {
  name                 = "gpu-worker"
  image                = "sfc:image:sfcompute:public:ubuntu-24.04-cuda-13.2"
  cloud_init_user_data = base64encode(file("${path.module}/cloud-init.yaml"))
  tags = {
    role = "worker"
  }
}
