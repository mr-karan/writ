# Each sensitive value below sets a for_each key through nonsensitive(),
# so the key, and with it the address, carries a secret. kind spells the
# type of terraform_data.café, whose name is not ASCII.
variable "long" {
  type      = string
  sensitive = true
}

variable "short" {
  type      = string
  sensitive = true
}

variable "tag" {
  type      = string
  sensitive = true
}

variable "kind" {
  type      = string
  sensitive = true
}

module "cert" {
  source = "./child"
  keys   = [var.long, var.short, var.tag]
}

resource "terraform_data" "café" {
  input = "local-only"
}

resource "terraform_data" "counted" {
  count = 2
  input = count.index
}

# Keys that hold no secret and need HCL's escapes, so OpenTofu's text of
# each address is kept as it wrote it.
resource "terraform_data" "plain" {
  for_each = toset(["q\"b\\s", "t$${x}%%{y}", "nb\u00a0sp", "tag\U000e0002", "é-日本"])
  input    = each.key
}
