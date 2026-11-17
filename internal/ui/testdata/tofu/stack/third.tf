resource "random_pet" "kept" {}

resource "random_pet" "rotated" {
  keepers = {
    secret = var.secret
  }
}

resource "local_file" "note" {
  filename = "${path.module}/note.txt"
  content  = var.secret
}

resource "random_id" "imported" {
  byte_length = 4
}

module "inner" {
  source = "./inner"
}

# Its filename is the stack's own directory, so the apply fails.
resource "local_file" "broken" {
  filename = path.module
  content  = var.secret
}

output "rotated" {
  value = random_pet.rotated.id
}

output "secret" {
  value     = var.secret
  sensitive = true
}
