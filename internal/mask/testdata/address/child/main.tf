variable "keys" {
  type = list(string)
}

resource "terraform_data" "item" {
  for_each = toset([for k in var.keys : nonsensitive(k)])
  input    = "local-only"
}
