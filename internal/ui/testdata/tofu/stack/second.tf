resource "random_pet" "kept" {}

# Forgets the pet: OpenTofu stops managing it and leaves it in place.
removed {
  from = random_pet.gone
}

# The secret changes, so the pet is replaced.
resource "random_pet" "rotated" {
  keepers = {
    secret = var.secret
  }
}

# The recording deletes note.txt before this plan, so the plan finds drift.
resource "local_file" "note" {
  filename = "${path.module}/note.txt"
  content  = var.secret
}

import {
  to = random_id.imported
  id = "p-9hUg"
}

resource "random_id" "imported" {
  byte_length = 4
}

module "inner" {
  source = "./inner"
}

output "rotated" {
  value = random_pet.rotated.id
}

output "secret" {
  value     = var.secret
  sensitive = true
}
