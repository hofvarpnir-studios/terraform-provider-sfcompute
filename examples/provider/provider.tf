terraform {
  required_providers {
    sfcompute = {
      source = "hofvarpnir-studios/sfcompute"
    }
  }
}

# The token is read from SFC_TOKEN when not set here. Create one with
# `sf tokens create` and grant it a role with `sf grants create`.
provider "sfcompute" {
  workspace = "sfc:workspace:acme:default"

  # Resources that spend money (sfcompute_order, sfcompute_instance) cannot
  # be created unless this is set. Leave it unset (false) for configurations
  # that should only manage free resources.
  # allow_spending = true
}
