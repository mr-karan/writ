terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "3.9.1"
    }
    local = {
      source  = "hashicorp/local"
      version = "2.9.1"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "4.4.2"
    }
  }
}

variable "pw" {
  type      = string
  sensitive = true
}

# The run overrides the default, so the default appears only in configuration.
variable "fallback" {
  type      = string
  sensitive = true
  default   = "wc-default-91ad5e"
}

# Not declared sensitive, and used only through length(), so its value
# appears only under variables.
variable "unmarked" {
  type = string
}

# Not declared sensitive; the run's environment names its value a secret.
variable "label" {
  type = string
}

# A value nobody marks, under an attribute whose name says secret.
variable "owner" {
  type = string
}

variable "pin" {
  type      = number
  sensitive = true
}

resource "random_password" "db" {
  length = 24
}

resource "tls_private_key" "k" {
  algorithm = "ED25519"
}

resource "local_file" "encoded" {
  filename = "${path.module}/encoded.txt"
  content  = base64encode(var.pw)
}

resource "local_file" "plain" {
  filename = "${path.module}/plain.txt"
  content  = nonsensitive(var.pw)
}

resource "local_file" "wrapped" {
  filename = "${path.module}/wrapped.txt"
  content  = nonsensitive(base64encode("user:${var.pw}"))
}

resource "random_id" "keyed" {
  for_each    = toset([var.label])
  byte_length = 4
}

# nonsensitive() strips the mark from a sensitive number, as a number and
# as a string.
resource "random_integer" "pinned" {
  min = nonsensitive(var.pin)
  max = nonsensitive(var.pin) + 10
  keepers = {
    pin = tostring(nonsensitive(var.pin))
  }
}

# jsonencode() escapes the & in the secret as \u0026.
resource "local_file" "json" {
  filename = "${path.module}/json.txt"
  content  = nonsensitive(jsonencode({ v = var.pw }))
}

resource "random_pet" "named" {
  keepers = {
    db_password = var.owner
    size        = length(var.unmarked)
    fallback    = var.fallback
  }
}

output "pw" {
  value     = var.pw
  sensitive = true
}

output "pw_copy" {
  value = nonsensitive(var.pw)
}

output "pin" {
  value     = var.pin
  sensitive = true
}

output "key_pem" {
  value     = tls_private_key.k.private_key_pem
  sensitive = true
}

# An unmarked output whose value holds a key that says secret.
output "settings" {
  value = {
    db_password = var.owner
    size        = 3
  }
}
