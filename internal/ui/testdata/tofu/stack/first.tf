resource "random_pet" "kept" {}

resource "random_pet" "gone" {}

resource "random_pet" "rotated" {
  keepers = {
    secret = var.secret
  }
}

resource "local_file" "note" {
  filename = "${path.module}/note.txt"
  content  = var.secret
}

output "rotated" {
  value = random_pet.rotated.id
}

output "secret" {
  value     = var.secret
  sensitive = true
}
