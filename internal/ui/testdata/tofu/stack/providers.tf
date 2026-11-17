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
  }
}

variable "secret" {
  type      = string
  sensitive = true
}
